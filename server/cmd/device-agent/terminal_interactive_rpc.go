package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (d dispatcher) callInteractiveTerminalRPC(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if d.state == nil || d.state.business == nil || d.requestProjectID == "" {
		return nil, 0, errRPCProject
	}
	project, err := d.fileProject()
	if err != nil {
		return nil, 0, err
	}
	service, err := d.state.terminalService()
	if err != nil {
		return nil, 0, err
	}
	switch method {
	case "terminal.open":
		output, err := service.OpenContext(ctx, project, input)
		return output, project.Revision, err
	case "terminal.attach":
		output, err := service.Attach(project, input)
		return output, project.Revision, err
	case "terminal.write":
		output, err := service.WriteContext(ctx, project, input)
		return output, project.Revision, err
	case "terminal.resize":
		output, err := service.ResizeContext(ctx, project, input)
		return output, project.Revision, err
	case "terminal.signal":
		output, err := service.SignalContext(ctx, project, input)
		return output, project.Revision, err
	case "terminal.close":
		output, err := service.CloseSessionContext(ctx, project, input)
		return output, project.Revision, err
	default:
		return nil, 0, errRPCNotFound
	}
}

func (service *terminalService) session(project registeredProject, sessionID uuid.UUID) (*terminalSession, error) {
	if service == nil || sessionID == uuid.Nil || project.ID == uuid.Nil {
		return nil, errRPCInvalid
	}
	if project.State != "available" || !project.Policy.AllowInteractiveTerminal ||
		!agentFeatureFlags(service.state)["terminal.interactive"] {
		service.mu.Lock()
		session := service.sessions[sessionID]
		service.mu.Unlock()
		if session != nil && session.projectID == project.ID {
			_ = session.process.Close("policy_revoked")
		}
		return nil, errRPCCapability
	}
	service.mu.Lock()
	session := service.sessions[sessionID]
	service.mu.Unlock()
	if session == nil {
		return nil, errRPCNotFound
	}
	if session.projectID != project.ID {
		return nil, errRPCProject
	}
	return session, nil
}

func (service *terminalService) Attach(project registeredProject, input rpcInput) (map[string]any, error) {
	if !inputHasOnly(input, "sessionId", "lastSequence", "waitSeconds") {
		return nil, errRPCInvalid
	}
	sessionID, lastSequence, wait, err := terminalAttachParameters(input)
	if err != nil {
		return nil, err
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return nil, err
	}
	session.markAttached(service.now())
	batch, err := session.snapshotAfter(lastSequence)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sessionId": session.id.String(), "firstSequence": batch.FirstSequence,
		"highWatermark": batch.HighWatermark, "resetRequired": batch.ResetRequired,
		"running": batch.Running, "waitSeconds": uint64(wait / time.Second),
	}, nil
}

func terminalAttachParameters(input rpcInput) (uuid.UUID, uint64, time.Duration, error) {
	sessionID, err := terminalSessionID(input)
	if err != nil {
		return uuid.Nil, 0, 0, err
	}
	lastSequence, present, ok := optionalUint64(input, "lastSequence")
	if !present || !ok {
		return uuid.Nil, 0, 0, errRPCInvalid
	}
	wait := terminalDefaultAttachWait
	if raw, found := input["waitSeconds"]; found && raw != nil {
		number, valid := raw.(float64)
		if !valid || number < 0 || number > terminalMaximumAttachWait.Seconds() || number != float64(uint64(number)) {
			return uuid.Nil, 0, 0, errRPCInvalid
		}
		wait = time.Duration(uint64(number)) * time.Second
	}
	return sessionID, lastSequence, wait, nil
}

func (session *terminalSession) markAttached(now time.Time) {
	session.mu.Lock()
	session.lastAttachAt = now
	session.mu.Unlock()
}

type terminalAttachCompletion struct {
	Reason        string
	Held          time.Duration
	EventCount    uint64
	HighWatermark uint64
}

func (session *terminalSession) beginAttach(now time.Time) bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.attachActive {
		return false
	}
	if session.attachTokenAt.IsZero() {
		session.attachTokenAt = now
		session.attachTokens = terminalAttachTokenBurst
	}
	if elapsed := now.Sub(session.attachTokenAt); elapsed > 0 {
		session.attachTokens += elapsed.Minutes() * terminalAttachTokensPerMinute
		if session.attachTokens > terminalAttachTokenBurst {
			session.attachTokens = terminalAttachTokenBurst
		}
		session.attachTokenAt = now
	}
	if session.attachTokens < 1 {
		return false
	}
	session.attachTokens--
	session.attachActive = true
	return true
}

func (session *terminalSession) endAttach() {
	session.mu.Lock()
	session.attachActive = false
	session.mu.Unlock()
}

func (service *terminalService) StreamAttach(ctx context.Context, project registeredProject, input rpcInput, emit func(terminalBufferedEvent, uint64) error) (terminalAttachCompletion, error) {
	var completion terminalAttachCompletion
	if emit == nil {
		return completion, errRPCInvalid
	}
	sessionID, lastSequence, wait, err := terminalAttachParameters(input)
	if err != nil {
		return completion, err
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return completion, err
	}
	if !session.beginAttach(service.now()) {
		return completion, errRPCBusy
	}
	defer session.endAttach()
	startedAt := time.Now()
	finish := func(reason string, highWatermark uint64) terminalAttachCompletion {
		return terminalAttachCompletion{
			Reason: reason, Held: time.Since(startedAt), EventCount: completion.EventCount,
			HighWatermark: highWatermark,
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	cursor := lastSequence
	for {
		session.markAttached(service.now())
		batch, snapshotErr := session.snapshotAfter(cursor)
		if snapshotErr != nil {
			return completion, snapshotErr
		}
		if batch.ResetRequired {
			return finish("reset", batch.HighWatermark), nil
		}
		for _, event := range batch.Events {
			if err := emit(event, batch.HighWatermark); err != nil {
				return completion, err
			}
			cursor = event.Sequence
			completion.EventCount++
		}
		if !batch.Running {
			return finish("exit", batch.HighWatermark), nil
		}
		if wait == 0 {
			reason := "timeout"
			if completion.EventCount > 0 {
				reason = "events"
			}
			return finish(reason, batch.HighWatermark), nil
		}
		select {
		case <-ctx.Done():
			return completion, ctx.Err()
		case <-deadline.C:
			reason := "timeout"
			if completion.EventCount > 0 {
				reason = "events"
			}
			return finish(reason, batch.HighWatermark), nil
		case <-batch.Notify:
		}
	}
}

func (service *terminalService) Write(project registeredProject, input rpcInput) (map[string]any, error) {
	return service.WriteContext(context.Background(), project, input)
}

func (service *terminalService) WriteContext(ctx context.Context, project registeredProject, input rpcInput) (map[string]any, error) {
	if !inputHasOnly(input, "sessionId", "inputSequence", "encoding", "data") {
		return nil, errRPCInvalid
	}
	sessionID, err := terminalSessionID(input)
	sequence, present, ok := optionalUint64(input, "inputSequence")
	encoding, okEncoding := inputString(input, "encoding", 16)
	encoded, okData := inputString(input, "data", maximumTerminalInputBytes*2)
	if err != nil || !present || !ok || sequence == 0 || !okEncoding || encoding != "base64url" || !okData {
		return nil, errRPCInvalid
	}
	data, decodeErr := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if decodeErr != nil || len(data) == 0 || len(data) > maximumTerminalInputBytes || base64.RawURLEncoding.EncodeToString(data) != encoded {
		return nil, errRPCInvalid
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return nil, err
	}
	replayed, err := session.sendInputContext(ctx, sequence, data)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sessionId": session.id.String(), "inputSequence": sequence, "acceptedBytes": len(data),
		"nextInputSequence": sequence + 1, "replayed": replayed,
	}, nil
}

func (session *terminalSession) sendInput(sequence uint64, data []byte) (bool, error) {
	return session.sendInputContext(context.Background(), sequence, data)
}

func (session *terminalSession) sendInputContext(ctx context.Context, sequence uint64, data []byte) (bool, error) {
	digest := sha256.Sum256(data)
	now := session.service.now()
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.running {
		return false, errRPCNotFound
	}
	if sequence <= session.inputSequence {
		previous, found := session.inputRecords[sequence]
		if !found || previous != digest {
			return false, errRPCRevision
		}
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if sequence != session.inputSequence+1 {
		return false, errRPCRevision
	}
	if now.Sub(session.inputRateWindow) >= time.Second {
		session.inputRateWindow, session.inputRateBytes = now, 0
	}
	if session.inputRateBytes+uint64(len(data)) > maximumTerminalInputRateBytes {
		if err := beginV2SideEffect(ctx); err != nil {
			return false, err
		}
		go func() { _ = session.process.Close("input_rate_limit") }()
		if err := commitV2SideEffect(ctx); err != nil {
			return false, err
		}
		return false, errRPCBusy
	}
	// Reserve the sequence before the PTY write. If the OS reports a partial
	// write, closing the session is safer than retrying bytes that may already
	// have reached the shell.
	if err := beginV2SideEffect(ctx); err != nil {
		return false, err
	}
	session.inputSequence = sequence
	session.inputRecords[sequence] = digest
	session.inputRateBytes += uint64(len(data))
	for old := range session.inputRecords {
		if old+terminalRecentInputRecords <= sequence {
			delete(session.inputRecords, old)
		}
	}
	written, err := session.process.Write(data)
	if err != nil || written != len(data) {
		go func() { _ = session.process.Close("pty_write_error") }()
		if err != nil {
			return false, err
		}
		return false, io.ErrShortWrite
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (service *terminalService) Resize(project registeredProject, input rpcInput) (map[string]any, error) {
	return service.ResizeContext(context.Background(), project, input)
}

func (service *terminalService) ResizeContext(ctx context.Context, project registeredProject, input rpcInput) (map[string]any, error) {
	if !inputHasOnly(input, "sessionId", "resizeSequence", "rows", "columns") {
		return nil, errRPCInvalid
	}
	sessionID, err := terminalSessionID(input)
	sequence, present, ok := optionalUint64(input, "resizeSequence")
	rows, okRows := terminalDimension(input, "rows", 0, 2, 500)
	columns, okColumns := terminalDimension(input, "columns", 0, 10, 1000)
	_, rowsPresent := input["rows"]
	_, columnsPresent := input["columns"]
	if err != nil || !present || !ok || sequence == 0 || !rowsPresent || !columnsPresent || !okRows || !okColumns {
		return nil, errRPCInvalid
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return nil, err
	}
	replayed, err := session.resizeContext(ctx, sequence, rows, columns)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sessionId": session.id.String(), "resizeSequence": sequence, "rows": rows, "columns": columns,
		"nextResizeSequence": sequence + 1, "replayed": replayed,
	}, nil
}

func (session *terminalSession) resize(sequence uint64, rows, columns uint16) (bool, error) {
	return session.resizeContext(context.Background(), sequence, rows, columns)
}

func (session *terminalSession) resizeContext(ctx context.Context, sequence uint64, rows, columns uint16) (bool, error) {
	size := terminalSize{Rows: rows, Columns: columns}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.running {
		return false, errRPCNotFound
	}
	if sequence <= session.resizeSequence {
		previous, found := session.resizeRecords[sequence]
		if !found || previous != size {
			return false, errRPCRevision
		}
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return false, err
		}
		return true, nil
	}
	if sequence != session.resizeSequence+1 {
		return false, errRPCRevision
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return false, err
	}
	if err := session.process.Resize(rows, columns); err != nil {
		return false, err
	}
	session.resizeSequence = sequence
	session.resizeRecords[sequence] = size
	session.rows, session.columns = rows, columns
	for old := range session.resizeRecords {
		if old+terminalRecentResizeRecords <= sequence {
			delete(session.resizeRecords, old)
		}
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (service *terminalService) Signal(project registeredProject, input rpcInput) (map[string]any, error) {
	return service.SignalContext(context.Background(), project, input)
}

func (service *terminalService) SignalContext(ctx context.Context, project registeredProject, input rpcInput) (map[string]any, error) {
	if !inputHasOnly(input, "sessionId", "signal", "inputSequence") {
		return nil, errRPCInvalid
	}
	sessionID, err := terminalSessionID(input)
	signal, okSignal := inputString(input, "signal", 16)
	if err != nil || !okSignal {
		return nil, errRPCInvalid
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return nil, err
	}
	if signal == "terminate" {
		if _, present := input["inputSequence"]; present {
			return nil, errRPCInvalid
		}
		if err := beginV2SideEffect(ctx); err != nil {
			return nil, err
		}
		err := session.process.Close("client_terminate")
		if err == nil {
			err = commitV2SideEffect(ctx)
		}
		return map[string]any{"sessionId": session.id.String(), "signal": signal, "closing": true, "replayed": false}, err
	}
	sequence, present, ok := optionalUint64(input, "inputSequence")
	if !present || !ok || sequence == 0 || (signal != "interrupt" && signal != "eof") {
		return nil, errRPCInvalid
	}
	data := []byte{3}
	if signal == "eof" {
		data[0] = 4
	}
	replayed, err := session.sendInputContext(ctx, sequence, data)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sessionId": session.id.String(), "signal": signal, "inputSequence": sequence,
		"nextInputSequence": sequence + 1, "replayed": replayed,
	}, nil
}

func (service *terminalService) CloseSession(project registeredProject, input rpcInput) (map[string]any, error) {
	return service.CloseSessionContext(context.Background(), project, input)
}

func (service *terminalService) CloseSessionContext(ctx context.Context, project registeredProject, input rpcInput) (map[string]any, error) {
	if !inputHasOnly(input, "sessionId") {
		return nil, errRPCInvalid
	}
	sessionID, err := terminalSessionID(input)
	if err != nil {
		return nil, err
	}
	session, err := service.session(project, sessionID)
	if err != nil {
		return nil, err
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, err
	}
	if err := session.process.Close("client_close"); err != nil {
		return nil, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"sessionId": session.id.String(), "closing": true}, nil
}

func (d dispatcher) streamInteractiveTerminalAttach(ctx context.Context, envelope *remotev1.RpcEnvelope, emit func(*remotev1.RpcEnvelope) error) (terminalAttachCompletion, error) {
	var completion terminalAttachCompletion
	request := envelope.GetRequest()
	if request == nil || request.GetHeader() == nil || request.GetMethod() != "terminal.attach" || emit == nil {
		return completion, errRPCInvalid
	}
	var input rpcInput
	if err := json.Unmarshal(request.GetJsonPayload(), &input); err != nil {
		return completion, errRPCInvalid
	}
	project, err := d.fileProject()
	if err != nil {
		return completion, err
	}
	service, err := d.state.terminalService()
	if err != nil {
		return completion, err
	}
	return service.StreamAttach(ctx, project, input, func(event terminalBufferedEvent, highWatermark uint64) error {
		payload, err := json.Marshal(terminalEventPayload(event, terminalSessionIDMust(input)))
		if err != nil {
			return err
		}
		kind := remotev1.RpcEventKind_RPC_EVENT_KIND_TERMINAL_OUTPUT
		if event.Kind == "exit" {
			kind = remotev1.RpcEventKind_RPC_EVENT_KIND_TERMINAL_EXIT
		}
		return emit(&remotev1.RpcEnvelope{
			ProtocolVersion: 1,
			Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
				EventId: uuid.NewString(), RequestId: request.GetHeader().GetRequestId(), Kind: kind,
				Sequence: event.Sequence, HighWatermark: highWatermark,
				OccurredAt: timestamppb.New(event.OccurredAt), JsonPayload: payload,
			}},
		})
	})
}

func terminalSessionIDMust(input rpcInput) uuid.UUID {
	id, _ := terminalSessionID(input)
	return id
}
