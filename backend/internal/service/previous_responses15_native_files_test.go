package service

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestNativeFilesExpandedHistoryAndFreshCopyFacts15(t *testing.T) {
	parent := []byte(`{"version":1,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-old"}]}],"output":[{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}","status":"completed"}],"tools":[{"type":"function","name":"read"}]}`)
	current := []byte(`{"model":"mapped","input":[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_file","file_id":"file-new"}]}],"store":false,"instructions":"current only"}`)
	body, turns, err := expandedNativeFileRequest15(current, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 || turns[0] != 0 || turns[1] != 0 || turns[2] != 1 {
		t.Fatalf("turn coordinates: %v", turns)
	}
	copy := managedfiles.NativeAttachmentWireCopy15{PublicFileID: "file-old", FileVersion: 1, SHA256: strings.Repeat("a", 64), Filename: "a.txt", MIME: "text/plain", Bytes: 4, AccountID: 3, BackendID: "backend", SubjectDigest: strings.Repeat("b", 64), CredentialGeneration: "epoch", OperationID: "operation-old", RemoteID: "file-remote-old", InputTokens: 2}
	other := copy
	other.PublicFileID = "file-new"
	other.OperationID = "operation-new"
	other.RemoteID = "file-remote-new"
	snapshot, err := nativeFileSnapshot15(body, turns, []managedfiles.NativeAttachmentWireCopy15{copy, other})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Positions) != 2 || snapshot.Positions[1].CallInput != 1 || snapshot.Positions[1].Input != 2 || snapshot.Positions[1].Turn != 1 {
		t.Fatalf("call ancestry lost: %+v", snapshot)
	}
	var source map[string]any
	if err = json.Unmarshal(body, &source); err != nil {
		t.Fatal(err)
	}
	if source["instructions"] != "current only" || len(source["tools"].([]any)) != 1 {
		t.Fatal("current controls/catalog changed")
	}
	if !bytes.Contains(body, []byte("file-old")) || bytes.Contains(body, []byte("file-remote")) {
		t.Fatal("expanded public input replaced with remote authority")
	}
	if _, err = nativeFileSnapshot15(body, turns, []managedfiles.NativeAttachmentWireCopy15{copy}); err == nil {
		t.Fatal("missing current file copy accepted")
	}
	oldCurrent := append([]byte(nil), current...)
	_, _, _ = expandedNativeFileRequest15(current, parent)
	if !bytes.Equal(current, oldCurrent) {
		t.Fatal("current delta mutated")
	}
}

func TestNativeFilesExpansionRejectsMissingTypedHistory15(t *testing.T) {
	for _, parent := range []string{`{}`, `{"version":2,"turns":[]}`, `{"version":99}`, `{"version":1,"input":null,"output":[]}`} {
		if _, _, err := expandedNativeFileRequest15([]byte(`{"model":"m","input":"x","store":false}`), []byte(parent)); err == nil {
			t.Fatalf("malformed history accepted: %s", parent)
		}
	}
	body, turns, err := expandedNativeFileRequest15([]byte(`{"model":"m","input":[{"type":"function_call_output","call_id":"orphan","output":[{"type":"input_file","file_id":"file-public"}]}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	copy := managedfiles.NativeAttachmentWireCopy15{PublicFileID: "file-public", FileVersion: 1, SHA256: strings.Repeat("a", 64), Filename: "a.txt", MIME: "text/plain", Bytes: 4, AccountID: 3, BackendID: "backend", SubjectDigest: strings.Repeat("b", 64), CredentialGeneration: "epoch", OperationID: "operation", RemoteID: "file-remote", InputTokens: 2}
	if _, err = nativeFileSnapshot15(body, turns, []managedfiles.NativeAttachmentWireCopy15{copy}); err == nil {
		t.Fatal("file tool result without original call accepted")
	}
}
