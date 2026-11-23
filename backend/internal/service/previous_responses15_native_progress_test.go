//go:build recovery15native

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Install first with the native CaptureEvent dispatch and observe actual RED.
// A signed completed terminal is covered by the separate capture/receipt tests.
func TestStoredResponseNative15StreamsFunctionAndCustomWithoutPartialCalls(t *testing.T) {
	c, db := native15ProgressCapture(t)
	events := []struct{ kind, raw string }{
		{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"response_id":"upstream","item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"read","status":"in_progress","arguments":"","encrypted_function_args":[]}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","sequence_number":2,"output_index":0,"response_id":"upstream","item_id":"fc_read","delta":"{\"path\":\"a\"}"}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":3,"output_index":0,"item_id":"fc_read","arguments":"{\"path\":\"a\"}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"read","status":"completed","arguments":"{\"path\":\"a\"}","encrypted_function_args":[]}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_patch","call_id":"call_patch","name":"patch","status":"in_progress","input":""}}`},
		{"response.custom_tool_call_input.delta", `{"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_patch","delta":"patch body"}`},
		{"response.custom_tool_call_input.done", `{"type":"response.custom_tool_call_input.done","output_index":1,"item_id":"ctc_patch","input":"patch body"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_patch","call_id":"call_patch","name":"patch","status":"completed","input":"patch body"}}`},
	}
	for _, event := range events {
		out, err := c.CaptureEvent(context.Background(), event.kind, []byte(event.raw))
		if err != nil {
			t.Fatalf("%s: %v", event.kind, err)
		}
		var got map[string]any
		json.Unmarshal(out, &got)
		if got["type"] != event.kind {
			t.Fatal("changed public stream event")
		}
		if _, exists := got["response_id"]; exists && got["response_id"] != c.PublicID() {
			t.Fatal("native response id leaked")
		}
	}
	if db.terminal != nil {
		t.Fatal("tool progress finalized storage")
	}
	if err := c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var failed struct{ Output []json.RawMessage }
	json.Unmarshal(db.terminal.PublicResponse, &failed)
	if len(failed.Output) != 0 {
		t.Fatal("partial function/custom calls became durable failed output")
	}
}

func TestStoredResponseNative15RetainsOnlyCompletedTextAndUsage(t *testing.T) {
	c, db := native15ProgressCapture(t)
	for _, event := range []struct{ kind, raw string }{
		{"response.in_progress", `{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","output_index":3,"content_index":0,"item_id":"msg_partial","delta":"unfinished"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":5,"item":{"type":"message","id":"msg_done","role":"assistant","status":"completed","content":[{"type":"output_text","text":"finished text","annotations":[]}]}}`},
	} {
		if _, err := c.CaptureEvent(context.Background(), event.kind, []byte(event.raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Output []struct {
			ID      string
			Content []struct{ Text string }
		}
		Usage map[string]int
	}
	if json.Unmarshal(db.terminal.PublicResponse, &saved) != nil || len(saved.Output) != 1 || saved.Output[0].ID != "msg_done" || saved.Output[0].Content[0].Text != "finished text" || saved.Usage["total_tokens"] != 7 {
		t.Fatalf("unsafe or missing partial facts: %s", db.terminal.PublicResponse)
	}
}

func TestStoredResponseNative15RejectsPrivateStreamingFieldsAndKeepsTextStrict(t *testing.T) {
	for _, event := range []struct{ kind, raw string }{
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_x","delta":"{}","native_replay":{"secret":"hidden"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_x","call_id":"call_x","name":"x","status":"in_progress","arguments":"","native_replay":{}}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_x","call_id":"call_x","name":"x","status":"completed","arguments":"{}","encrypted_function_args":["opaque"]}}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_x","arguments":"{\"x\":1,\"x\":2}"}`},
		{"response.custom_tool_call_input.done", `{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_x","input":{"private":"hidden"}}`},
		{"response.content_part.done", `{"type":"response.content_part.done","output_index":0,"content_index":0,"item_id":"msg_x","part":{"type":"output_text","text":"x","annotations":[{"type":"file_citation","file_id":"file_private"}]}}`},
		{"response.in_progress", `{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"private_recovery":{"secret":"hidden"}}}`},
	} {
		c, db := native15ProgressCapture(t)
		out, err := c.CaptureEvent(context.Background(), event.kind, []byte(event.raw))
		if err == nil || out != nil || db.terminal != nil {
			t.Fatalf("released private native event: %s", event.raw)
		}
	}
	c, _ := native15ProgressCapture(t)
	c.record.RecoveryVersion = 1
	if _, err := c.CaptureEvent(context.Background(), "response.function_call_arguments.delta", []byte(`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_x","delta":"{}"}`)); err == nil {
		t.Fatal("text1 accepted new native streaming contract")
	}
}

func TestStoredResponseNative15RejectsOversizedProgressBeforeRelease(t *testing.T) {
	c, _ := native15ProgressCapture(t)
	c.service.cfg.Limits.MaxResponseBytes = 4096
	raw, _ := json.Marshal(map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc_x", "delta": strings.Repeat("x", 4096)})
	if out, err := c.CaptureEvent(context.Background(), "response.custom_tool_call_input.delta", raw); err == nil || out != nil {
		t.Fatal("released oversized native progress")
	}
}

func native15ProgressCapture(t *testing.T) (*StoredResponseCapture, *captureTestDB) {
	t.Helper()
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := native15RecordFixture()
	record.State = "in_progress"
	record.ExecutionState = "not_sent"
	record.LeaseToken = "fixture"
	record.Version = 1
	return &StoredResponseCapture{service: svc, record: record, store: true}, db
}
