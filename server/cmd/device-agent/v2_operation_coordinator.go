package main

import (
	"context"
	"crypto/sha256"
	"errors"

	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"google.golang.org/protobuf/proto"
)

const v2MaximumInFlightOperations = 512

var errV2OperationInProgress = errors.New("remote/v2 operation is still in progress")

// v2InFlightOperation is the process-local exactly-once fence. The durable
// journal remains the restart boundary, while this coordinator prevents two
// concurrent attempts from both observing a hot-cache/SQLite miss and running
// the same mutation.
type v2InFlightOperation struct {
	digest   [sha256.Size]byte
	done     chan struct{}
	response *remotev2.RpcResponse
}

func (state *agentState) claimV2Operation(operationID string, digest [sha256.Size]byte) (*v2InFlightOperation, bool, error) {
	if state == nil || operationID == "" {
		return nil, false, errRPCInvalid
	}
	state.v2OperationMu.Lock()
	defer state.v2OperationMu.Unlock()
	if state.v2Operations == nil {
		state.v2Operations = make(map[string]*v2InFlightOperation)
	}
	if existing := state.v2Operations[operationID]; existing != nil {
		if existing.digest != digest {
			return nil, false, errRPCIdempotency
		}
		return existing, false, nil
	}
	if len(state.v2Operations) >= v2MaximumInFlightOperations {
		return nil, false, errRPCBusy
	}
	claim := &v2InFlightOperation{digest: digest, done: make(chan struct{})}
	state.v2Operations[operationID] = claim
	return claim, true, nil
}

func (state *agentState) finishV2Operation(operationID string, claim *v2InFlightOperation, response *remotev2.RpcResponse) {
	if state == nil || claim == nil {
		return
	}
	state.v2OperationMu.Lock()
	if state.v2Operations[operationID] != claim {
		state.v2OperationMu.Unlock()
		return
	}
	if response != nil {
		claim.response = proto.Clone(response).(*remotev2.RpcResponse)
	}
	delete(state.v2Operations, operationID)
	close(claim.done)
	state.v2OperationMu.Unlock()
}

func (claim *v2InFlightOperation) wait(ctx context.Context) (*remotev2.RpcResponse, error) {
	if claim == nil {
		return nil, errRPCInvalid
	}
	select {
	case <-ctx.Done():
		return nil, errV2OperationInProgress
	case <-claim.done:
		if claim.response == nil {
			return nil, errV2OperationInProgress
		}
		return proto.Clone(claim.response).(*remotev2.RpcResponse), nil
	}
}

func (state *agentState) v2InFlightOperationCount() int {
	if state == nil {
		return 0
	}
	state.v2OperationMu.Lock()
	defer state.v2OperationMu.Unlock()
	return len(state.v2Operations)
}
