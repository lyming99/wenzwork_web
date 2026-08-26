package main

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"google.golang.org/protobuf/proto"
)

const (
	v2TerminalMaximumOutputCredit = 64 << 10
	v2TerminalInboundFrameLimit   = 64
)

type v2AgentTerminalStream struct {
	session    *terminalSession
	channelID  string
	streamID   string
	generation uint64
	inbound    chan *remotev2.TerminalStreamFrame

	mu               sync.Mutex
	queuedInputBytes int
}

type v2TerminalSentOutput struct {
	sequence uint64
	bytes    uint32
}

func (session *terminalSession) registerDuplex(cancel context.CancelFunc) uint64 {
	if session == nil || cancel == nil {
		return 0
	}
	now := session.service.now()
	session.mu.Lock()
	previous := session.duplexCancel
	session.duplexGeneration++
	generation := session.duplexGeneration
	session.duplexCancel = cancel
	// A v4 Hello is the duplex equivalent of a v3 attach. Keep the disconnect
	// grace anchored to confirmed application traffic rather than to the time
	// at which terminal.open happened.
	session.lastAttachAt = now
	session.mu.Unlock()
	if previous != nil {
		previous()
	}
	return generation
}

func (session *terminalSession) acceptDuplexKeepAlive(ack *remotev2.TerminalOutputAck) bool {
	if session == nil || session.service == nil || ack == nil || ack.GetThroughSequence() != 0 || ack.GetCreditBytes() != 0 {
		return false
	}
	// (0, 0) is reserved as a content-free terminal v4 liveness frame. It does
	// not acknowledge output or return byte credit, so it cannot advance a
	// watermark or inflate the flow-control window.
	session.markAttached(session.service.now())
	return true
}

func (session *terminalSession) unregisterDuplex(generation uint64) {
	if session == nil || generation == 0 {
		return
	}
	session.mu.Lock()
	if session.duplexGeneration == generation {
		session.duplexCancel = nil
	}
	session.mu.Unlock()
}

func (session *terminalSession) terminalWatermarks() (input, resize uint64) {
	if session == nil {
		return 0, 0
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.inputSequence, session.resizeSequence
}

func (terminal *v2AgentTerminalStream) enqueue(frame *remotev2.TerminalStreamFrame) bool {
	if terminal == nil || frame == nil {
		return false
	}
	clone, ok := proto.Clone(frame).(*remotev2.TerminalStreamFrame)
	if !ok {
		return false
	}
	inputBytes := 0
	if input := clone.GetInput(); input != nil {
		inputBytes = len(input.GetData())
	}
	terminal.mu.Lock()
	if inputBytes > 0 && terminal.queuedInputBytes+inputBytes > maximumTerminalInputBytes {
		terminal.mu.Unlock()
		return false
	}
	terminal.queuedInputBytes += inputBytes
	select {
	case terminal.inbound <- clone:
		terminal.mu.Unlock()
		return true
	default:
		terminal.queuedInputBytes -= inputBytes
		terminal.mu.Unlock()
		return false
	}
}

func (terminal *v2AgentTerminalStream) consumed(frame *remotev2.TerminalStreamFrame) {
	if terminal == nil || frame == nil || frame.GetInput() == nil {
		return
	}
	terminal.mu.Lock()
	terminal.queuedInputBytes -= len(frame.GetInput().GetData())
	if terminal.queuedInputBytes < 0 {
		terminal.queuedInputBytes = 0
	}
	terminal.mu.Unlock()
}

func (link *v2AgentLink) handleTerminalStreamData(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if link == nil || record == nil || !link.isActive() {
		return errV2AgentLink
	}
	frame := new(remotev2.TerminalStreamFrame)
	if !unmarshalV2AgentMessage(plaintext, frame) || uuid.Validate(frame.GetSessionId()) != nil || frame.GetBody() == nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	channel := link.channels[record.GetChannelId()]
	var stream *v2AgentStream
	if channel != nil {
		stream = channel.streams[record.GetStreamId()]
	}
	if channel == nil || stream == nil || stream.kind != remotev2.StreamKind_STREAM_KIND_TERMINAL ||
		stream.operationID != frame.GetSessionId() || channel.projectID == "" {
		link.mu.Unlock()
		return errV2AgentStream
	}
	if _, allowed := channel.scopes["remote.peer.terminal.interactive"]; !allowed {
		link.mu.Unlock()
		return errV2AgentStream
	}
	terminal := stream.terminal
	streamContext := stream.context
	streamCancel := stream.cancel
	projectID := channel.projectID
	link.mu.Unlock()

	if terminal != nil {
		if frame.GetHello() != nil || !terminal.enqueue(frame) {
			return errV2AgentLink
		}
		return nil
	}
	hello := frame.GetHello()
	if hello == nil || hello.GetOutputCreditBytes() == 0 || hello.GetOutputCreditBytes() > v2TerminalMaximumOutputCredit {
		return errV2AgentLink
	}
	projectUUID, err := uuid.Parse(projectID)
	if err != nil || projectUUID == uuid.Nil || link.state == nil || link.state.business == nil {
		return errV2AgentLink
	}
	project, err := link.state.business.projectByID(ctx, projectUUID)
	if err != nil || project.State != "available" || !project.Policy.AllowInteractiveTerminal {
		return errV2AgentLink
	}
	service, err := link.state.terminalService()
	if err != nil {
		return err
	}
	sessionID, err := uuid.Parse(frame.GetSessionId())
	if err != nil || sessionID == uuid.Nil {
		return errV2AgentLink
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return err
	}
	inputWatermark, resizeWatermark := session.terminalWatermarks()
	if hello.GetAfterInputSequence() > inputWatermark || hello.GetAfterResizeSequence() > resizeWatermark {
		return errV2AgentLink
	}
	if _, err := session.snapshotAfter(hello.GetAfterOutputSequence()); err != nil {
		return err
	}
	terminal = &v2AgentTerminalStream{
		session: session, channelID: record.GetChannelId(), streamID: record.GetStreamId(),
		inbound: make(chan *remotev2.TerminalStreamFrame, v2TerminalInboundFrameLimit),
	}
	terminal.generation = session.registerDuplex(streamCancel)
	if terminal.generation == 0 {
		return errV2AgentLink
	}
	link.mu.Lock()
	currentChannel := link.channels[record.GetChannelId()]
	if currentChannel == nil || currentChannel.streams[record.GetStreamId()] != stream || stream.terminal != nil {
		link.mu.Unlock()
		session.unregisterDuplex(terminal.generation)
		return errV2AgentStream
	}
	stream.terminal = terminal
	stream.cleanup = func() { session.unregisterDuplex(terminal.generation) }
	link.mu.Unlock()

	go func() {
		err := link.serveTerminalStreamV4(streamContext, terminal, hello)
		if err != nil && !errors.Is(err, context.Canceled) {
			link.failStream(context.Background(), terminal.channelID, terminal.streamID, remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
			return
		}
		link.closeStream(terminal.channelID, terminal.streamID)
	}()
	return nil
}

func (link *v2AgentLink) serveTerminalStreamV4(ctx context.Context, terminal *v2AgentTerminalStream, hello *remotev2.TerminalStreamHello) error {
	if link == nil || terminal == nil || terminal.session == nil || hello == nil {
		return errV2AgentLink
	}
	sessionID := terminal.session.id.String()
	inputWatermark, resizeWatermark := terminal.session.terminalWatermarks()
	if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
		SessionId: sessionID,
		Body:      &remotev2.TerminalStreamFrame_InputAck{InputAck: &remotev2.TerminalInputAck{ThroughSequence: inputWatermark}},
	}); err != nil {
		return err
	}
	if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
		SessionId: sessionID,
		Body:      &remotev2.TerminalStreamFrame_ResizeAck{ResizeAck: &remotev2.TerminalResizeAck{ThroughSequence: resizeWatermark}},
	}); err != nil {
		return err
	}
	if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
		SessionId: sessionID,
		Body:      &remotev2.TerminalStreamFrame_WindowUpdate{WindowUpdate: &remotev2.TerminalWindowUpdate{CreditBytes: maximumTerminalInputBytes}},
	}); err != nil {
		return err
	}

	cursor := hello.GetAfterOutputSequence()
	ackedOutput := cursor
	outputCredit := uint64(hello.GetOutputCreditBytes())
	inputCredit := uint64(maximumTerminalInputBytes)
	sent := make([]v2TerminalSentOutput, 0, 16)
	for {
		batch, err := terminal.session.snapshotAfter(cursor)
		if err != nil {
			return err
		}
		if batch.ResetRequired {
			return link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
				SessionId: sessionID,
				Body: &remotev2.TerminalStreamFrame_Reset_{Reset_: &remotev2.TerminalReset{
					FirstSequence: batch.FirstSequence, HighWatermark: batch.HighWatermark,
				}},
			})
		}
		for _, event := range batch.Events {
			switch event.Kind {
			case "output":
				if len(event.Data) == 0 {
					return errV2AgentLink
				}
				if uint64(len(event.Data)) > outputCredit {
					break
				}
				if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
					SessionId: sessionID,
					Body: &remotev2.TerminalStreamFrame_Output{Output: &remotev2.TerminalOutput{
						Sequence: event.Sequence, Data: append([]byte(nil), event.Data...),
					}},
				}); err != nil {
					return err
				}
				outputCredit -= uint64(len(event.Data))
				sent = append(sent, v2TerminalSentOutput{sequence: event.Sequence, bytes: uint32(len(event.Data))})
				cursor = event.Sequence
			case "exit":
				if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
					SessionId: sessionID,
					Body: &remotev2.TerminalStreamFrame_Exit{Exit: &remotev2.TerminalExit{
						Sequence: event.Sequence, ExitCode: int32(event.ExitCode), Reason: event.ExitReason,
					}},
				}); err != nil {
					return err
				}
				return nil
			default:
				return errV2AgentLink
			}
			if cursor != event.Sequence {
				break
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-terminal.inbound:
			terminal.consumed(frame)
			if frame == nil || frame.GetSessionId() != sessionID {
				return errV2AgentLink
			}
			if ack := frame.GetOutputAck(); ack != nil {
				if terminal.session.acceptDuplexKeepAlive(ack) {
					continue
				}
				through := ack.GetThroughSequence()
				if through < ackedOutput || through > cursor {
					return errV2AgentLink
				}
				var returned uint64
				index := 0
				for index < len(sent) && sent[index].sequence <= through {
					returned += uint64(sent[index].bytes)
					index++
				}
				if returned != uint64(ack.GetCreditBytes()) || outputCredit+returned > v2TerminalMaximumOutputCredit {
					return errV2AgentLink
				}
				sent = append(sent[:0], sent[index:]...)
				ackedOutput, outputCredit = through, outputCredit+returned
				terminal.session.markAttached(terminal.session.service.now())
				continue
			}
			if err := link.handleTerminalInboundV4(ctx, terminal, frame, &inputCredit); err != nil {
				return err
			}
			terminal.session.markAttached(terminal.session.service.now())
		case <-batch.Notify:
		}
	}
}

func (link *v2AgentLink) handleTerminalInboundV4(ctx context.Context, terminal *v2AgentTerminalStream, frame *remotev2.TerminalStreamFrame, inputCredit *uint64) error {
	if link == nil || terminal == nil || terminal.session == nil || frame == nil || inputCredit == nil {
		return errV2AgentLink
	}
	session := terminal.session
	sessionID := session.id.String()
	if input := frame.GetInput(); input != nil {
		if input.GetSequence() == 0 || len(input.GetData()) == 0 || len(input.GetData()) > maximumTerminalInputBytes || uint64(len(input.GetData())) > *inputCredit {
			return errV2AgentLink
		}
		*inputCredit -= uint64(len(input.GetData()))
		if _, err := session.sendInput(input.GetSequence(), input.GetData()); err != nil {
			return err
		}
		*inputCredit += uint64(len(input.GetData()))
		return link.sendTerminalInputProgress(ctx, terminal, sessionID, uint32(len(input.GetData())))
	}
	if resize := frame.GetResize(); resize != nil {
		if resize.GetSequence() == 0 || resize.GetRows() < 2 || resize.GetRows() > 500 || resize.GetColumns() < 10 || resize.GetColumns() > 1000 {
			return errV2AgentLink
		}
		if _, err := session.resize(resize.GetSequence(), uint16(resize.GetRows()), uint16(resize.GetColumns())); err != nil {
			return err
		}
		_, through := session.terminalWatermarks()
		return link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
			SessionId: sessionID,
			Body:      &remotev2.TerminalStreamFrame_ResizeAck{ResizeAck: &remotev2.TerminalResizeAck{ThroughSequence: through}},
		})
	}
	if signal := frame.GetSignal(); signal != nil {
		switch signal.GetSignal() {
		case "interrupt", "eof":
			if signal.GetInputSequence() == 0 || *inputCredit == 0 {
				return errV2AgentLink
			}
			data := []byte{3}
			if signal.GetSignal() == "eof" {
				data[0] = 4
			}
			*inputCredit--
			if _, err := session.sendInput(signal.GetInputSequence(), data); err != nil {
				return err
			}
			*inputCredit++
			return link.sendTerminalInputProgress(ctx, terminal, sessionID, 1)
		case "terminate":
			if signal.GetInputSequence() != 0 {
				return errV2AgentLink
			}
			return session.process.Close("client_terminate")
		default:
			return errV2AgentLink
		}
	}
	if closeFrame := frame.GetClose(); closeFrame != nil {
		if closeFrame.GetReason() != "client_close" {
			return errV2AgentLink
		}
		return session.process.Close("client_close")
	}
	return errV2AgentLink
}

func (link *v2AgentLink) sendTerminalInputProgress(ctx context.Context, terminal *v2AgentTerminalStream, sessionID string, credit uint32) error {
	through, _ := terminal.session.terminalWatermarks()
	if err := link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
		SessionId: sessionID,
		Body:      &remotev2.TerminalStreamFrame_InputAck{InputAck: &remotev2.TerminalInputAck{ThroughSequence: through}},
	}); err != nil {
		return err
	}
	return link.sendTerminalFrame(ctx, terminal, &remotev2.TerminalStreamFrame{
		SessionId: sessionID,
		Body:      &remotev2.TerminalStreamFrame_WindowUpdate{WindowUpdate: &remotev2.TerminalWindowUpdate{CreditBytes: credit}},
	})
}

func (link *v2AgentLink) sendTerminalFrame(ctx context.Context, terminal *v2AgentTerminalStream, frame *remotev2.TerminalStreamFrame) error {
	if link == nil || terminal == nil || frame == nil || frame.GetBody() == nil || frame.GetSessionId() != terminal.session.id.String() {
		return errV2AgentLink
	}
	return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_STREAM_DATA, terminal.channelID, terminal.streamID, frame)
}
