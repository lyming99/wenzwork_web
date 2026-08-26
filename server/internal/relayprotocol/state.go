package relayprotocol

import "fmt"

type CommandState string

const (
	CommandQueued          CommandState = "queued"
	CommandDispatched      CommandState = "dispatched"
	CommandAccepted        CommandState = "accepted"
	CommandRunning         CommandState = "running"
	CommandCancelRequested CommandState = "cancel_requested"
	CommandSucceeded       CommandState = "succeeded"
	CommandFailed          CommandState = "failed"
	CommandRejected        CommandState = "rejected"
	CommandCancelled       CommandState = "cancelled"
	CommandExpired         CommandState = "expired"
	CommandTimedOut        CommandState = "timed_out"
)

var commandTransitions = map[CommandState]map[CommandState]struct{}{
	CommandQueued: {
		CommandDispatched: {}, CommandCancelled: {}, CommandExpired: {},
	},
	CommandDispatched: {
		CommandQueued: {}, CommandAccepted: {}, CommandCancelRequested: {}, CommandExpired: {},
	},
	CommandAccepted: {
		CommandRunning: {}, CommandRejected: {}, CommandCancelRequested: {}, CommandTimedOut: {},
	},
	CommandRunning: {
		CommandSucceeded: {}, CommandFailed: {}, CommandCancelRequested: {}, CommandTimedOut: {},
	},
	CommandCancelRequested: {
		CommandRunning: {}, CommandCancelled: {}, CommandSucceeded: {}, CommandFailed: {}, CommandTimedOut: {},
	},
	CommandTimedOut: {
		CommandSucceeded: {}, CommandFailed: {}, CommandCancelled: {},
	},
}

func ValidateCommandTransition(from, to CommandState) error {
	if _, ok := commandTransitions[from][to]; !ok {
		return fmt.Errorf("invalid command transition %q -> %q", from, to)
	}
	return nil
}

type PeerSessionState string

const (
	PeerOpening     PeerSessionState = "opening"
	PeerActive      PeerSessionState = "active"
	PeerClosed      PeerSessionState = "closed"
	PeerExpired     PeerSessionState = "expired"
	PeerInterrupted PeerSessionState = "interrupted"
)

func ValidatePeerSessionTransition(from, to PeerSessionState) error {
	valid := (from == PeerOpening && (to == PeerActive || to == PeerClosed || to == PeerExpired || to == PeerInterrupted)) ||
		(from == PeerActive && (to == PeerClosed || to == PeerExpired || to == PeerInterrupted))
	if !valid {
		return fmt.Errorf("invalid peer session transition %q -> %q", from, to)
	}
	return nil
}

type TransferState string

const (
	TransferOffered      TransferState = "offered"
	TransferAccepted     TransferState = "accepted"
	TransferRejected     TransferState = "rejected"
	TransferTransferring TransferState = "transferring"
	TransferInterrupted  TransferState = "interrupted"
	TransferResuming     TransferState = "resuming"
	TransferVerifying    TransferState = "verifying"
	TransferCompleted    TransferState = "completed"
	TransferFailed       TransferState = "failed"
	TransferCancelled    TransferState = "cancelled"
	TransferExpired      TransferState = "expired"
)

var transferTransitions = map[TransferState]map[TransferState]struct{}{
	TransferOffered: {
		TransferAccepted: {}, TransferRejected: {}, TransferCancelled: {}, TransferExpired: {},
	},
	TransferAccepted: {
		TransferTransferring: {}, TransferCancelled: {}, TransferInterrupted: {},
	},
	TransferTransferring: {
		TransferInterrupted: {}, TransferVerifying: {}, TransferCancelled: {}, TransferFailed: {},
	},
	TransferInterrupted: {
		TransferResuming: {}, TransferCancelled: {}, TransferExpired: {},
	},
	TransferResuming: {
		TransferTransferring: {}, TransferInterrupted: {}, TransferCancelled: {}, TransferExpired: {},
	},
	TransferVerifying: {
		TransferCompleted: {}, TransferFailed: {}, TransferInterrupted: {},
	},
}

func ValidateTransferTransition(from, to TransferState) error {
	if _, ok := transferTransitions[from][to]; !ok {
		return fmt.Errorf("invalid transfer transition %q -> %q", from, to)
	}
	return nil
}
