//go:build recovery15native

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestStoredNativeFiles15NormalizeOnlyPublicTypedParts(t *testing.T) {
	for _, input := range []string{
		`[{"role":"user","content":[{"type":"input_text","text":"read"},{"type":"input_file","file_id":"file-public1"}]}]`,
		`[{"type":"function_call_output","call_id":"call_read","output":[{"type":"input_file","file_id":"file-public1"}]}]`,
		`[{"type":"custom_tool_call_output","call_id":"call_read","output":[{"type":"input_file","file_id":"file-public1"}]}]`,
	} {
		request, err := NormalizeStoredResponseNativeRequest15([]byte(`{"model":"public","store":true,"input":` + input + `}`))
		if err != nil || request == nil || !request.Store {
			t.Fatalf("typed public input rejected: %v", err)
		}
		if _, err = NormalizeStoredResponseRequest([]byte(`{"model":"public","store":true,"input":` + input + `}`)); err == nil {
			t.Fatal("ordinary text contract accepted attachments")
		}
	}
	for _, part := range []string{
		`{"type":"input_file","file_id":"file-public1","filename":"injected.txt"}`,
		`{"type":"input_file","file_id":"file-public1","file_data":"data:text/plain;base64,eA=="}`,
		`{"type":"input_file","file_url":"https://example.invalid/file"}`,
		`{"type":"input_file","file_id":"file-public1","file_id":"file-other"}`,
		`{"type":"Input_File","file_id":"file-public1"}`,
		`{"type":"input_file","File_ID":"file-public1"}`,
		`{"type":"input_file","file_id":null}`,
		`{"type":"input_file","file_id":"file_x"}`,
		`{"type":"input_image","file_id":"file-public1"}`,
	} {
		body := []byte(`{"model":"public","input":[{"role":"user","content":[` + part + `]}]}`)
		if _, err := NormalizeStoredResponseNativeRequest15(body); err == nil {
			t.Fatalf("unsupported file part accepted: %s", part)
		}
	}
}

func nativeFileShapeTurn15(t *testing.T) map[string]any {
	t.Helper()
	snapshot := managedfiles.NativeAttachmentSnapshot15{
		Version:   managedfiles.NativeAttachmentContract15,
		Copies:    []managedfiles.NativeAttachmentWireCopy15{{PublicFileID: "file-public1", FileVersion: 3, SHA256: strings.Repeat("b", 64), Filename: "report.txt", MIME: "text/plain", Bytes: 28, AccountID: 4, BackendID: "backend", SubjectDigest: strings.Repeat("a", 64), CredentialGeneration: "generation", OperationID: "operation-one", RemoteID: "file-remote1", InputTokens: 9}},
		Positions: []managedfiles.NativeAttachmentPosition15{{Turn: 0, Input: 0, Field: "content", Part: 0, SourceKind: "message", CallInput: -1, CallItem: json.RawMessage("null"), CopyIndex: 0}},
	}
	raw, err := managedfiles.CanonicalNativeAttachmentSnapshot15(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := decodeStoredResponseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"version": json.Number("4"),
		"binding": map[string]any{"owner_id": json.Number("1"), "group_id": json.Number("3"), "account_id": json.Number("4"), "model": "resolved", "subject_digest": strings.Repeat("a", 64)},
		"input":   []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_file", "file_id": "file-public1"}}}},
		"tools":   []any{}, "output": []any{}, "replays": []any{}, "attachments": attachment,
	}
}

func nativeFileShapeClone15(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := decodeStoredResponseJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return clone.(map[string]any)
}

func TestStoredNativeFiles15ValidateEveryTypedPositionAndIdentity(t *testing.T) {
	record := native15RecordFixture()
	turn := nativeFileShapeTurn15(t)
	check := func(turn map[string]any) error {
		return validateStoredNativeAttachmentTurn15([]map[string]any{turn}, record, strings.Repeat("a", 64), "backend", "generation")
	}
	if err := check(turn); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(map[string]any){
		"missing_snapshot": func(v map[string]any) { delete(v, "attachments") },
		"missing_position": func(v map[string]any) { v["attachments"].(map[string]any)["positions"] = []any{} },
		"wrong_public_id": func(v map[string]any) {
			v["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)["public_file_id"] = "file-other"
		},
		"wrong_account": func(v map[string]any) {
			v["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)["account_id"] = json.Number("5")
		},
		"wrong_backend": func(v map[string]any) {
			v["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)["backend_id"] = "different"
		},
		"wrong_generation": func(v map[string]any) {
			v["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)["credential_generation"] = "different"
		},
		"wrong_subject": func(v map[string]any) {
			v["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)["subject_digest"] = strings.Repeat("c", 64)
		},
		"wrong_input": func(v map[string]any) {
			v["attachments"].(map[string]any)["positions"].([]any)[0].(map[string]any)["input"] = json.Number("8")
		},
		"wrong_turn": func(v map[string]any) {
			v["attachments"].(map[string]any)["positions"].([]any)[0].(map[string]any)["turn"] = json.Number("1")
		},
		"extra_position": func(v map[string]any) {
			a := v["attachments"].(map[string]any)
			position := nativeFileShapeClone15(t, a["positions"].([]any)[0].(map[string]any))
			position["part"] = json.Number("1")
			a["positions"] = append(a["positions"].([]any), position)
		},
		"text_version_with_file": func(v map[string]any) { v["version"] = json.Number("1") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copy := nativeFileShapeClone15(t, turn)
			mutate(copy)
			if err := check(copy); err == nil {
				t.Fatal("inconsistent attachment snapshot accepted")
			}
		})
	}
	raw, _ := json.Marshal(turn)
	if _, err := storedNativeTurns15(raw, record, strings.Repeat("a", 64), 16384); err != nil {
		t.Fatalf("v4 root rejected: %v", err)
	}
}

func TestStoredNativeFiles15ReuploadPreservesHistoricalStableFacts(t *testing.T) {
	first := nativeFileShapeTurn15(t)
	second := nativeFileShapeClone15(t, first)
	second["input"] = []any{map[string]any{"role": "user", "content": "continue"}}
	copy := second["attachments"].(map[string]any)["copies"].([]any)[0].(map[string]any)
	copy["remote_id"], copy["operation_id"] = "file-remote2", "operation-two"
	before, _ := json.Marshal(first)
	if err := validateStoredNativeAttachmentTurn15([]map[string]any{first, second}, native15RecordFixture(), strings.Repeat("a", 64), "backend", "generation"); err != nil {
		t.Fatalf("fresh confirmed replacement copy rejected: %v", err)
	}
	after, _ := json.Marshal(first)
	if string(before) != string(after) {
		t.Fatal("old turn mutated by replacement copy")
	}
	copy["sha256"] = strings.Repeat("c", 64)
	if err := validateStoredNativeAttachmentTurn15([]map[string]any{first, second}, native15RecordFixture(), strings.Repeat("a", 64), "backend", "generation"); err == nil {
		t.Fatal("same public file ID reinterpreted as changed bytes")
	}
}

func TestStoredNativeFiles15ToolOutputMatchesExactAncestorCall(t *testing.T) {
	call := map[string]any{"type": "function_call", "call_id": "call_read", "id": "fc_original", "name": "read", "arguments": `{"opaque":"file-unused"}`, "status": "completed"}
	first := nativeFileShapeTurn15(t)
	first["version"] = json.Number("1")
	delete(first, "attachments")
	first["input"] = []any{map[string]any{"role": "user", "content": "run tool"}}
	first["output"] = []any{call}
	second := nativeFileShapeTurn15(t)
	second["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_read", "output": []any{map[string]any{"type": "input_file", "file_id": "file-public1"}}}}
	attachment := second["attachments"].(map[string]any)
	position := attachment["positions"].([]any)[0].(map[string]any)
	position["turn"], position["input"], position["call_input"] = json.Number("1"), json.Number("2"), json.Number("1")
	position["source_kind"], position["field"], position["call_id"], position["call_item"] = "function_call_output", "output", "call_read", call
	check := func() error {
		return validateStoredNativeAttachmentTurn15([]map[string]any{first, second}, native15RecordFixture(), strings.Repeat("a", 64), "backend", "generation")
	}
	if err := check(); err != nil {
		t.Fatalf("ancestor call position rejected: %v", err)
	}
	position["call_item"] = nativeFileShapeClone15(t, call)
	position["call_item"].(map[string]any)["arguments"] = `{"opaque":"changed"}`
	if err := check(); err == nil {
		t.Fatal("attachment call source changed opaque arguments")
	}
	position["call_item"] = call
	position["call_input"] = json.Number("0")
	if err := check(); err == nil {
		t.Fatal("attachment matched wrong ancestor position")
	}
}

func TestStoredNativeFiles15DurableMaterialBindsAttachmentBackend(t *testing.T) {
	record := native15RecordFixture()
	turn := nativeFileShapeTurn15(t)
	record.Input, _ = json.Marshal(turn["input"])
	snapshot, _ := json.Marshal(turn)
	public := []byte(`{"id":"resp_local_00000000000000000000000000000001","object":"response","created_at":1,"model":"public","store":true,"status":"completed","output":[]}`)
	gateway := []byte(`{"object":"response","status":"completed","output":[]}`)
	proof := native15VerifiedFixture(record, gateway, snapshot)
	if _, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, proof, 16384); err != nil {
		t.Fatalf("valid durable file material rejected: %v", err)
	}
	proof.backendID = "different"
	if _, err := BuildStoredResponseNativeRecovery15(record, public, gateway, snapshot, proof, 16384); err == nil {
		t.Fatal("file copy backend differs from authenticated material")
	}
}

func TestStoredNativeFiles15MinimumIncludesConfirmedSnapshotBeforeReserve(t *testing.T) {
	turn := nativeFileShapeTurn15(t)
	body, _ := json.Marshal(map[string]any{"model": "public", "store": true, "input": turn["input"]})
	request, err := NormalizeStoredResponseNativeRequest15(body)
	if err != nil {
		t.Fatal(err)
	}
	record := native15RecordFixture()
	record.Input = request.Input
	admission := native15AdmissionFixture(request)
	if _, err = storedNativeMinimum15(record, admission, 16384); err == nil {
		t.Fatal("pre-send budget admitted file without fresh confirmed snapshot")
	}
	raw, _ := json.Marshal(turn["attachments"])
	snapshot, err := managedfiles.DecodeNativeAttachmentSnapshot15(raw)
	if err != nil {
		t.Fatal(err)
	}
	admission.attachmentSnapshot = &snapshot
	minimum, err := storedNativeMinimum15(record, admission, 16384)
	if err != nil {
		t.Fatalf("confirmed file minimum rejected: %v", err)
	}
	var material storedResponseNative15
	if json.Unmarshal(minimum, &material) != nil || !strings.Contains(string(material.Snapshot), `"attachments"`) || !strings.Contains(string(material.Snapshot), `"remote_id":"file-remote1"`) {
		t.Fatal("file facts absent from durable minimum budget")
	}
	if _, err = storedNativeMinimum15(record, admission, int64(len(minimum)-1)); err == nil {
		t.Fatal("minimum exceeds reservation byte limit")
	}
}
