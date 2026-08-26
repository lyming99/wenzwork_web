package relaymaintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

type OperationStore interface {
	ClaimOperation(context.Context, time.Duration) (relaymanagement.OperationClaim, bool, error)
	FailOperation(context.Context, relaymanagement.OperationClaim, string, string) error
	GetEndpoint(context.Context, uuid.UUID) (relaymanagement.ManagedEndpoint, error)
	ExecuteCellUpdate(context.Context, relaymanagement.OperationClaim) error
	ExecuteDrain(context.Context, relaymanagement.OperationClaim) error
	CompleteEndpointValidation(context.Context, relaymanagement.OperationClaim, relaymanagement.EndpointValidationResult) error
	FailEndpointValidation(context.Context, relaymanagement.OperationClaim, string) error
	ExecuteEndpointActivation(context.Context, relaymanagement.OperationClaim) error
	ExecuteUserAssignmentOperation(context.Context, relaymanagement.OperationClaim) error
}

type Worker struct {
	store     OperationStore
	validator EndpointValidator
	claimTTL  time.Duration
}

func NewWorker(store OperationStore, validator EndpointValidator) (*Worker, error) {
	if store == nil || validator.Identities == nil {
		return nil, errors.New("Relay Host maintenance dependencies are required")
	}
	return &Worker{store: store, validator: validator, claimTTL: 2 * time.Minute}, nil
}

func (worker *Worker) ProcessOne(ctx context.Context) (bool, error) {
	claim, ok, err := worker.store.ClaimOperation(ctx, worker.claimTTL)
	if err != nil || !ok {
		return ok, err
	}
	switch claim.Type {
	case "cell_update":
		err = worker.store.ExecuteCellUpdate(ctx, claim)
	case "node_drain", "cell_drain":
		err = worker.store.ExecuteDrain(ctx, claim)
	case "endpoint_validate":
		err = worker.validateEndpoint(ctx, claim)
	case "endpoint_activate":
		err = worker.store.ExecuteEndpointActivation(ctx, claim)
	case "migrate_user", "user_unpin":
		err = worker.store.ExecuteUserAssignmentOperation(ctx, claim)
	default:
		err = fmt.Errorf("unsupported Relay operation type %q", claim.Type)
	}
	if err == nil {
		return true, nil
	}
	// The public operation record gets a stable code and deliberately omits
	// database, network, endpoint, and credential details from the message.
	if failErr := worker.store.FailOperation(ctx, claim, operationErrorCode(err), "Relay operation could not be completed"); failErr != nil && !errors.Is(failErr, relaymanagement.ErrConflict) {
		return true, errors.Join(err, failErr)
	}
	return true, err
}

func (worker *Worker) validateEndpoint(ctx context.Context, claim relaymanagement.OperationClaim) error {
	if claim.TargetID == nil {
		return relaymanagement.ErrInvalidInput
	}
	endpoint, err := worker.store.GetEndpoint(ctx, *claim.TargetID)
	if err != nil {
		return err
	}
	result, err := worker.validator.Validate(ctx, endpoint)
	if err != nil {
		code := operationErrorCode(err)
		if failErr := worker.store.FailEndpointValidation(ctx, claim, code); failErr != nil {
			return errors.Join(err, failErr)
		}
		// The claim has already been moved to failed by FailEndpointValidation.
		return nil
	}
	return worker.store.CompleteEndpointValidation(ctx, claim, result)
}

func operationErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrEndpointResolution):
		return "endpoint_dns_failed"
	case errors.Is(err, ErrEndpointUnsafe):
		return "endpoint_ssrf_blocked"
	case errors.Is(err, ErrEndpointTLS):
		return "endpoint_tls_failed"
	case errors.Is(err, ErrEndpointProtocol):
		return "endpoint_protocol_failed"
	case errors.Is(err, ErrEndpointIdentity), errors.Is(err, relaymanagement.ErrIdentityMismatch):
		return "endpoint_identity_failed"
	case errors.Is(err, relaymanagement.ErrNotFound):
		return "relay_resource_not_found"
	case errors.Is(err, relaymanagement.ErrConflict), errors.Is(err, relaymanagement.ErrVersionConflict):
		return "relay_state_conflict"
	case errors.Is(err, relaymanagement.ErrInvalidInput):
		return "relay_input_invalid"
	default:
		return "relay_operation_failed"
	}
}
