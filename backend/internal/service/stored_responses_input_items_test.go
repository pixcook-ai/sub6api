//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStoredResponseInputItemsProjectsOnlyPublicCurrentInput(t *testing.T) {
	record := &StoredResponse{PublicID: "resp_local_test", Input: json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"result"},{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"custom result"}]}]`), Recovery: json.RawMessage(`{"private":"never-read"}`)}
	items, err := StoredResponseInputItems(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0]["type"] != "function_call_output" || items[0]["call_id"] != "call_1" || items[0]["output"] != "result" || items[1]["type"] != "custom_tool_call_output" {
		t.Fatalf("public call-output identities lost: %+v", items)
	}
	if items[0]["id"] == items[1]["id"] || items[0]["id"] == "" || items[1]["id"] == "" {
		t.Fatal("list cursors are missing or collide")
	}
	again, err := StoredResponseInputItems(record)
	if err != nil {
		t.Fatal(err)
	}
	if again[0]["id"] != items[0]["id"] {
		t.Fatal("cursor changed")
	}
	if string(record.Input) != `[{"type":"function_call_output","call_id":"call_1","output":"result"},{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"custom result"}]}]` {
		t.Fatal("projection mutated saved input")
	}
	record.Input = json.RawMessage(`[]`)
	empty, err := StoredResponseInputItems(record)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty public list wrong: %#v/%v", empty, err)
	}
	for _, input := range []string{`null`, `{}`, `[{"role":"user","content":"x","private_replay":"secret"}]`, `[{"type":"reasoning","encrypted_content":"private"}]`} {
		record.Input = json.RawMessage(input)
		if _, err := StoredResponseInputItems(record); err == nil {
			t.Fatalf("unsupported saved input shape accepted: %s", input)
		}
	}
}

func TestStoredResponseInputItemsKeepsFileIdentity(t *testing.T) {
	record := &StoredResponse{PublicID: "resp_local_test", Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_text","text":"x"}]}]`)}
	items, err := StoredResponseInputItems(record)
	if err != nil {
		t.Fatal(err)
	}
	content := items[0]["content"].([]any)
	file := content[0].(map[string]any)
	if file["type"] != "input_file" || file["file_id"] != "file-public" || len(file) != 2 {
		t.Fatalf("file identity changed: %+v", file)
	}
}

func TestStoredResponseInputProjectionOnlySkipsExactOwnedLocalCursors(t *testing.T) {
	svc, _ := deploymentFixture()
	session, err := svc.BindManagedDeploymentRequest(context.Background(), 1, 1, 1, "local-list", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithManagedDeploymentSession(context.Background(), session)
	record := &StoredResponse{PublicID: "resp_local_test", UserID: 1, APIKeyID: 1, GroupID: 1, AccountID: session.Binding.AccountID, Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file-owned"}]}]`)}
	items, err := StoredResponseInputItems(record)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"object": "list", "data": items})
	projected := WithStoredResponseInputItemsProjection(ctx, record)
	if err = svc.ValidateManagedDeploymentOutputPayload(projected, payload); err == nil {
		t.Fatal("unowned file escaped local cursor projection")
	}
	if err = svc.RecordManagedDeploymentResource(ctx, session, "file", "file-owned"); err != nil {
		t.Fatal(err)
	}
	if err = svc.ValidateManagedDeploymentOutputPayload(ctx, payload); err == nil {
		t.Fatal("local cursors trusted without projection")
	}
	if err = svc.ValidateManagedDeploymentOutputPayload(projected, payload); err != nil {
		t.Fatal(err)
	}
	items[0]["id"] = "item-forged"
	forged, _ := json.Marshal(map[string]any{"object": "list", "data": items})
	if err = svc.ValidateManagedDeploymentOutputPayload(projected, forged); err == nil {
		t.Fatal("forged cursor escaped")
	}
	record.APIKeyID = 2
	foreign := WithStoredResponseInputItemsProjection(ctx, record)
	if err = svc.ValidateManagedDeploymentOutputPayload(foreign, payload); err == nil {
		t.Fatal("cross-key projection escaped")
	}
}
