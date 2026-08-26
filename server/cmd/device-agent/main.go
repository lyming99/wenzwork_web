// device-agent is the independently deployable target-side RPC process. Its
// state file contains only this installation's Ed25519 private identity and
// device-local data. Peer session tickets, controller keys and plaintext RPC
// payloads are never persisted by this process.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/proto"
)

var version = "dev"

func main() {
	if handled, exitCode := runAICommandSandboxInternal(os.Args[1:]); handled {
		os.Exit(exitCode)
	}
	if handled, err := runPlatformService(os.Args[1:], os.Stderr); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "device-agent service:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		var taskExit taskExecExitError
		if errors.As(err, &taskExit) {
			os.Exit(taskExit.code)
		}
		fmt.Fprintln(os.Stderr, "device-agent:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(arguments) == 1 && (arguments[0] == "version" || arguments[0] == "--version") {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if len(arguments) == 0 {
		return errors.New("usage: device-agent <serve|call|identity|project> [options]")
	}
	switch arguments[0] {
	case "internal-task-exec":
		return runInternalTaskExec(arguments[1:], stdout, stderr)
	case "serve":
		return runServe(arguments[1:], stderr)
	case "rpc-stdio":
		flags := flag.NewFlagSet("rpc-stdio", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath := flags.String("state-file", "", "private device state file")
		workspace := flags.String("workspace", "", "root exposed by file RPC methods")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid rpc-stdio options")
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		if err := state.startTaskEngine(); err != nil {
			return fmt.Errorf("start task engine: %w", err)
		}
		return serveRPC(context.Background(), state, stdin, stdout)
	case "call":
		flags := flag.NewFlagSet("call", flag.ContinueOnError)
		flags.SetOutput(stderr)
		method := flags.String("method", "", "generic RPC method")
		input := flags.String("input", "{}", "JSON object payload")
		requestID := flags.String("request-id", uuid.NewString(), "UUID request ID")
		deadline := flags.Duration("deadline", 30*time.Second, "request deadline")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid call options")
		}
		envelope, err := newCallEnvelope(*requestID, *method, []byte(*input), *deadline)
		if err != nil {
			return err
		}
		payload, err := proto.Marshal(envelope)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, base64.RawURLEncoding.EncodeToString(payload))
		return err
	case "identity":
		flags := flag.NewFlagSet("identity", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statePath := flags.String("state-file", "", "private device state file")
		workspace := flags.String("workspace", "", "device workspace")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid identity options")
		}
		state, err := loadOrCreateAgentState(*statePath, *workspace)
		if err != nil {
			return err
		}
		defer state.close()
		return json.NewEncoder(stdout).Encode(map[string]any{
			"deviceId": state.DeviceID, "identityAlgorithm": "Ed25519", "identityPublicKey": state.publicKey(), "keyVersion": state.KeyVersion,
		})
	case "project":
		return runProject(arguments[1:], stdout, stderr)
	default:
		return errors.New("usage: device-agent <serve|call|identity|project> [options]")
	}
}

func runServe(arguments []string, stderr io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServeContext(ctx, arguments, stderr)
}

func runServeContext(ctx context.Context, arguments []string, stderr io.Writer) error {
	if ctx == nil {
		return errors.New("serve context is required")
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	envFile := flags.String("env-file", "", "environment file path (defaults to .env when present)")
	statePath := flags.String("state-file", "", "private device state file")
	workspace := flags.String("workspace", "", "root exposed by file RPC methods")
	controlRaw := flags.String("control-url", "", "Control Plane base URL")
	accessKey := flags.String("access-key", "", "initial DeviceKey (or WENZWORK_DEVICE_ACCESS_KEY; never logged or persisted)")
	tlsCAFile := flags.String("tls-ca-file", "", "additional trusted CA PEM file")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("invalid serve options")
	}
	if err := loadAgentEnvironment(*envFile); err != nil {
		return err
	}
	stateFile := valueOrEnvironment(*statePath, "WENZWORK_DEVICE_STATE_FILE")
	instanceLock, err := acquireAgentInstanceLock(stateFile)
	if err != nil {
		return err
	}
	defer instanceLock.Close()

	state, err := loadOrCreateAgentState(
		stateFile,
		valueOrEnvironment(*workspace, "WENZWORK_DEVICE_WORKSPACE"),
	)
	if err != nil {
		return err
	}
	defer state.close()
	protocolLog := slog.New(slog.NewJSONHandler(stderr, nil))
	state.protocolDiagnosticSink = func(diagnostic deviceProtocolDiagnostic) {
		protocolLog.Warn("remote protocol failure",
			"stage", diagnostic.Stage, "reason", diagnostic.Reason, "fault_level", diagnostic.FaultLevel,
			"direction", diagnostic.Direction, "method_class", diagnostic.MethodClass, "scope", diagnostic.Scope,
			"connection_epoch", diagnostic.ConnectionEpoch, "payload_size_bucket", diagnostic.PayloadSizeBucket,
			"request_hash", diagnostic.RequestHash, "session_hash", diagnostic.SessionHash,
			"root_failure_id", diagnostic.RootFailureID)
	}
	state.connectionDiagnosticSink = func(diagnostic deviceConnectionDiagnostic) {
		attributes := []any{
			"event", diagnostic.Event, "reason", diagnostic.Reason,
			"connection_epoch", diagnostic.ConnectionEpoch,
			"reconnect_attempt", diagnostic.ReconnectAttempt,
		}
		if diagnostic.RetryAfterMilliseconds > 0 {
			attributes = append(attributes, "retry_after_ms", diagnostic.RetryAfterMilliseconds)
		}
		if diagnostic.HeartbeatMilliseconds > 0 {
			attributes = append(attributes, "heartbeat_ms", diagnostic.HeartbeatMilliseconds)
		}
		switch diagnostic.Event {
		case "relay_allocation_failed", "relay_dial_failed", "relay_handshake_failed", "relay_disconnected", "relay_heartbeat_timeout":
			protocolLog.Warn("remote Relay lifecycle", attributes...)
		default:
			protocolLog.Info("remote Relay lifecycle", attributes...)
		}
	}
	controlURL, err := validateTargetControlURL(valueOrEnvironment(*controlRaw, "WENZWORK_CONTROL_URL"))
	if err != nil {
		return err
	}
	key := valueOrEnvironment(*accessKey, "WENZWORK_DEVICE_ACCESS_KEY")
	if key != "" && !validDeviceKey(key) {
		return errors.New("--access-key or WENZWORK_DEVICE_ACCESS_KEY is invalid")
	}
	if err := state.startTaskEngine(); err != nil {
		return fmt.Errorf("start task engine: %w", err)
	}
	return runTarget(ctx, targetConfig{
		controlURL: controlURL,
		accessKey:  key,
		tlsCAFile:  valueOrEnvironment(*tlsCAFile, "WENZWORK_DEVICE_TLS_CA_FILE"),
		state:      state,
	})
}

// loadAgentEnvironment reads an explicit environment file, or a .env file in
// the current working directory when present. godotenv.Load deliberately does
// not overwrite variables inherited from the service manager or user shell.
func loadAgentEnvironment(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		if _, err := os.Stat(".env"); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect device agent .env: %w", err)
		}
		path = ".env"
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("load device agent environment: %w", err)
	}
	return nil
}

func valueOrEnvironment(value, environment string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(environment))
}

func validDeviceKey(value string) bool {
	if len(value) != len("device_")+43 || !strings.HasPrefix(value, "device_") {
		return false
	}
	for _, character := range value[len("device_"):] {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

// serveRPC is a length-bounded stdio adapter for the decrypted inner RPC
// channel. A Relay/Peer transport feeds one base64url protobuf per line after
// authenticating and opening PeerCiphertext; responses are encoded the same
// way for sealing. This separation makes it impossible for the dispatcher to
// accidentally persist a ticket or a remote controller private key.
func serveRPC(ctx context.Context, state *agentState, input io.Reader, output io.Writer) error {
	if state == nil || input == nil || output == nil {
		return errors.New("RPC stream dependencies are required")
	}
	dispatch := dispatcher{state: state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.query"}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	writer := bufio.NewWriter(output)
	defer writer.Flush()
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(scanner.Text())
		payload, err := base64.RawURLEncoding.Strict().DecodeString(line)
		if err != nil || len(payload) == 0 || len(payload) > maximumRPCPayload || base64.RawURLEncoding.EncodeToString(payload) != line {
			return errors.New("RPC stream frame is invalid")
		}
		envelope := new(remotev1.RpcEnvelope)
		if err := proto.Unmarshal(payload, envelope); err != nil {
			return errors.New("RPC protobuf is invalid")
		}
		response := dispatch.dispatch(ctx, envelope)
		encoded, err := proto.Marshal(response)
		if err != nil || len(encoded) > maximumRPCPayload {
			return errors.New("RPC response is invalid")
		}
		if _, err := fmt.Fprintln(writer, base64.RawURLEncoding.EncodeToString(encoded)); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func newCallEnvelope(requestID, method string, input []byte, deadline time.Duration) (*remotev1.RpcEnvelope, error) {
	requestID, method = strings.TrimSpace(requestID), strings.TrimSpace(method)
	if uuid.Validate(requestID) != nil || !validMethod(method) || deadline < time.Second || deadline > 15*time.Minute ||
		len(input) == 0 || len(input) > maximumRPCPayloadForMethod(method) {
		return nil, errRPCInvalid
	}
	var object map[string]any
	if json.Unmarshal(input, &object) != nil {
		return nil, errRPCInvalid
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, errRPCInvalid
	}
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header: &remotev1.RpcRequestHeader{RequestId: requestID, Deadline: rpcDeadline(deadline)},
			Method: method, JsonPayload: canonical,
		}},
	}, nil
}
