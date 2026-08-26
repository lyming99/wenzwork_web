package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type protocolGoldenVector struct {
	Name            string `json:"name"`
	Base64URL       string `json:"base64Url"`
	SHA256          string `json:"sha256"`
	EncodedBytes    int    `json:"encodedBytes"`
	ProtocolVersion uint32 `json:"protocolVersion"`
	MessageField    int    `json:"messageField"`
	EventKind       int32  `json:"eventKind"`
	JSONKind        string `json:"jsonKind"`
}

type protocolGoldenFixture struct {
	ContractVersion uint32                 `json:"contractVersion"`
	GeneratedBy     string                 `json:"generatedBy"`
	Relay           []protocolGoldenVector `json:"relay"`
	RPC             []protocolGoldenVector `json:"rpc"`
	Forward         []protocolGoldenVector `json:"forwardCompatibility"`
}

func TestGeneratedProtocolGoldenVectorsDecodeInGo(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "remote", "v1", "fixtures", "protocol_golden_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture protocolGoldenFixture
	if json.Unmarshal(contents, &fixture) != nil || fixture.ContractVersion != 1 ||
		fixture.GeneratedBy != "server/cmd/remote-contract-vectors" || len(fixture.Relay) != 6 || len(fixture.RPC) != 24 || len(fixture.Forward) != 2 {
		t.Fatalf("golden fixture header = %+v", fixture)
	}
	for _, vector := range fixture.Relay {
		encoded := decodeGoldenVector(t, vector)
		var envelope remotev1.Envelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil || envelope.GetProtocolVersion() != vector.ProtocolVersion ||
			oneofFieldNumber(envelope.ProtoReflect(), "frame") != protoreflect.FieldNumber(vector.MessageField) {
			t.Fatalf(
				"relay vector %q did not decode: protocol=%d frame=%d error=%v",
				vector.Name,
				envelope.GetProtocolVersion(),
				oneofFieldNumber(envelope.ProtoReflect(), "frame"),
				err,
			)
		}
	}
	for _, vector := range append(append([]protocolGoldenVector{}, fixture.RPC...), fixture.Forward...) {
		encoded := decodeGoldenVector(t, vector)
		var envelope remotev1.RpcEnvelope
		if err := proto.Unmarshal(encoded, &envelope); err != nil || envelope.GetProtocolVersion() != vector.ProtocolVersion ||
			oneofFieldNumber(envelope.ProtoReflect(), "message") != protoreflect.FieldNumber(vector.MessageField) {
			t.Fatalf(
				"RPC vector %q did not decode: protocol=%d message=%d error=%v",
				vector.Name,
				envelope.GetProtocolVersion(),
				oneofFieldNumber(envelope.ProtoReflect(), "message"),
				err,
			)
		}
		if vector.EventKind == 0 {
			if vector.Name == "rpc-envelope-exact-limit" && len(encoded) != maximumPeerRPCPlaintext {
				t.Fatalf("RPC boundary vector size = %d, want %d", len(encoded), maximumPeerRPCPlaintext)
			}
			continue
		}
		if int32(envelope.GetEvent().GetKind()) != vector.EventKind {
			t.Fatalf("RPC vector %q enum = %d, want %d", vector.Name, envelope.GetEvent().GetKind(), vector.EventKind)
		}
		var payload struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(envelope.GetEvent().GetJsonPayload(), &payload) != nil || payload.Kind != vector.JSONKind {
			t.Fatalf("RPC vector %q payload kind = %q, want %q", vector.Name, payload.Kind, vector.JSONKind)
		}
	}
}

func decodeGoldenVector(t *testing.T, vector protocolGoldenVector) []byte {
	t.Helper()
	encoded, err := base64.RawURLEncoding.Strict().DecodeString(vector.Base64URL)
	if err != nil || base64.RawURLEncoding.EncodeToString(encoded) != vector.Base64URL {
		t.Fatalf("vector %q has invalid base64url: %v", vector.Name, err)
	}
	digest := sha256.Sum256(encoded)
	if len(encoded) != vector.EncodedBytes || hex.EncodeToString(digest[:]) != vector.SHA256 {
		t.Fatalf("vector %q SHA-256 mismatch", vector.Name)
	}
	return encoded
}

func oneofFieldNumber(message protoreflect.Message, name protoreflect.Name) protoreflect.FieldNumber {
	oneof := message.Descriptor().Oneofs().ByName(name)
	if oneof == nil {
		return 0
	}
	field := message.WhichOneof(oneof)
	if field == nil {
		return 0
	}
	return field.Number()
}
