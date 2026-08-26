package relayprotocol

import (
	"errors"
	"testing"
)

func TestCommandStateMachineDoesNotTreatRelayEnqueueAsAcceptance(t *testing.T) {
	for _, transition := range [][2]CommandState{
		{CommandQueued, CommandDispatched},
		{CommandDispatched, CommandAccepted},
		{CommandDispatched, CommandCancelRequested},
		{CommandAccepted, CommandRunning},
		{CommandRunning, CommandSucceeded},
		{CommandDispatched, CommandQueued},
		{CommandCancelRequested, CommandRunning},
	} {
		if err := ValidateCommandTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("ValidateCommandTransition(%q, %q): %v", transition[0], transition[1], err)
		}
	}
	for _, transition := range [][2]CommandState{
		{CommandDispatched, CommandSucceeded},
		{CommandDispatched, CommandCancelled},
		{CommandQueued, CommandRunning},
		{CommandSucceeded, CommandRunning},
	} {
		if err := ValidateCommandTransition(transition[0], transition[1]); err == nil {
			t.Fatalf("ValidateCommandTransition(%q, %q) unexpectedly succeeded", transition[0], transition[1])
		}
	}
}

func TestFileTransferRequiresVerifiedTerminalState(t *testing.T) {
	path := [][2]TransferState{
		{TransferOffered, TransferAccepted},
		{TransferAccepted, TransferTransferring},
		{TransferTransferring, TransferVerifying},
		{TransferVerifying, TransferCompleted},
	}
	for _, transition := range path {
		if err := ValidateTransferTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("ValidateTransferTransition(%q, %q): %v", transition[0], transition[1], err)
		}
	}
	if err := ValidateTransferTransition(TransferTransferring, TransferCompleted); err == nil {
		t.Fatal("FILE_COMPLETE bypassed verifying state")
	}
}

func TestPeerAndFilePayloadCannotUsePersistentBus(t *testing.T) {
	for _, frameType := range []string{"PEER_QUERY", "PEER_DELTA", "FILE_MANIFEST", "FILE_CHUNK", "FILE_ACK", "FILE_VERIFIED"} {
		if MayUsePersistentBus(frameType) {
			t.Fatalf("%s may use persistent bus", frameType)
		}
	}
	if !MayUsePersistentBus("DEVICE_EVENT") {
		t.Fatal("allowed control routing was rejected")
	}
}

func TestFrameLimits(t *testing.T) {
	if err := ValidateFrameSize("COMMAND", ControlFrameLimit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFrameSize("COMMAND", ControlFrameLimit+1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("control oversize error = %v", err)
	}
	if err := ValidateFrameSize("FILE_CHUNK", AbsoluteFrameLimit); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFrameSize("FILE_CHUNK", AbsoluteFrameLimit+1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("absolute oversize error = %v", err)
	}
}
