package managedfiles

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesPreflightPreservesExactWireBytes(t *testing.T) {
	wire := json.RawMessage(`{"model": "fixture-text", "input": "hello", "store":false}`)
	request := ResponsesPreflightRequest{
		Version:  ResponsesContract,
		Expected: Identity{BackendID: "backend-1", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "generation-1"},
		OwnerRef: "user-1", GroupRef: "group-1", ClientRequest: wire, WireRequest: wire,
	}
	encoded, err := EncodeResponsesPreflight(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, wire) {
		t.Fatal("preflight compacted exact request bytes")
	}
	decoded, err := DecodeResponsesPreflight(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.WireRequest, wire) || !bytes.Equal(decoded.ClientRequest, wire) {
		t.Fatal("preflight changed request bytes")
	}
	if ResponsesWireSHA256(decoded.WireRequest) == ResponsesWireSHA256(bytes.ReplaceAll(wire, []byte(" "), nil)) {
		t.Fatal("wire digest ignored exact byte differences")
	}
}
