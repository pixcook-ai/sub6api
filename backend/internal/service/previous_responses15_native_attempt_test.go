//go:build recovery15native

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestStoredResponseNative15AttemptWrapperAcceptsFunctionAndCustomProgress(t *testing.T) {
	capture, db := native15ProgressCapture(t)
	ctx := native15WrapperContext(capture)
	for _, event := range []struct{ kind, raw string }{
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"response_id":"upstream","item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"read","status":"in_progress","arguments":"","encrypted_function_args":[]}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"response_id":"upstream","item_id":"fc_read","delta":"{\"path\":\"a\"}"}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_read","arguments":"{\"path\":\"a\"}"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"read","status":"completed","arguments":"{\"path\":\"a\"}","encrypted_function_args":[]}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_patch","call_id":"call_patch","name":"patch","status":"in_progress","input":""}}`},
		{"response.custom_tool_call_input.delta", `{"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_patch","delta":"patch body"}`},
		{"response.custom_tool_call_input.done", `{"type":"response.custom_tool_call_input.done","output_index":1,"item_id":"ctc_patch","input":"patch body"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_patch","call_id":"call_patch","name":"patch","status":"completed","input":"patch body"}}`},
	} {
		out, err := captureStoredResponseEvent(ctx, event.kind, []byte(event.raw))
		if err != nil {
			t.Fatalf("actual attempt wrapper rejected %s: %v", event.kind, err)
		}
		var public map[string]any
		if json.Unmarshal(out, &public) != nil || public["type"] != event.kind {
			t.Fatal("wrapper changed the public event contract")
		}
		if id, exists := public["response_id"]; exists && id != capture.PublicID() {
			t.Fatal("wrapper released an upstream response ID")
		}
	}
	if db.terminal != nil || len(capture.partialOutput) != 0 {
		t.Fatal("tool progress created a terminal or durable partial call")
	}
}

func TestStoredResponseNative15AttemptWrapperRejectsPrivateAndMalformedProgress(t *testing.T) {
	for _, event := range []struct{ kind, raw string }{
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_x","delta":"{}","native_replay":{"secret":"hidden"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_x","call_id":"call_x","name":"read","status":"in_progress","arguments":"","snapshot_base64":"hidden"}}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_x","call_id":"call_x","name":"read","status":"completed","arguments":"{}","encrypted_function_args":["opaque"]}}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_x","arguments":"{\"x\":1,\"x\":2}"}`},
		{"response.custom_tool_call_input.done", `{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_x","input":{"private":"hidden"}}`},
		{"response.function_call_arguments.delta", `{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"fc_x","delta":"{}"}`},
		{"response.in_progress", `{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"private_recovery":{"secret":"hidden"}}}`},
		{"response.native_replay", `{"type":"response.native_replay","snapshot_base64":"hidden"}`},
	} {
		t.Run(event.kind, func(t *testing.T) {
			capture, db := native15ProgressCapture(t)
			out, err := captureStoredResponseEvent(native15WrapperContext(capture), event.kind, []byte(event.raw))
			if err == nil || out != nil || db.terminal != nil {
				t.Fatal("actual wrapper released malformed/private native progress")
			}
		})
	}
	capture, _ := native15ProgressCapture(t)
	capture.service.cfg.Limits.MaxResponseBytes = 4096
	raw, _ := json.Marshal(map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": 0, "item_id": "ctc_x", "delta": strings.Repeat("x", 4096)})
	if out, err := captureStoredResponseEvent(native15WrapperContext(capture), "response.custom_tool_call_input.delta", raw); err == nil || out != nil {
		t.Fatal("wrapper released oversized native progress")
	}
}

func TestStoredResponseNative15AttemptWrapperKeepsTextContractAndTerminalProof(t *testing.T) {
	for _, event := range []struct{ kind, raw string }{
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_x","call_id":"call_x","name":"read","status":"in_progress","arguments":""}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_x","delta":"{}"}`},
	} {
		capture, db := native15ProgressCapture(t)
		capture.record.RecoveryVersion = 1
		if out, err := captureStoredResponseEvent(native15WrapperContext(capture), event.kind, []byte(event.raw)); err == nil || out != nil || db.terminal != nil {
			t.Fatal("text capture opted into the native event contract")
		}
	}
	capture, db := native15ProgressCapture(t)
	raw := []byte(`{"type":"response.completed","response":{"object":"response","status":"completed","output":[{"type":"function_call","id":"fc_x","call_id":"call_x","name":"read","status":"completed","arguments":"{}","encrypted_function_args":[]}]}}`)
	if out, err := captureStoredResponseEvent(native15WrapperContext(capture), "response.completed", raw); err == nil || out != nil || db.terminal != nil || capture.finished {
		t.Fatal("wrapper accepted public completed without authenticated private recovery")
	}
}

func native15WrapperContext(capture *StoredResponseCapture) context.Context {
	return context.WithValue(context.Background(), storedResponseCaptureKey{}, &storedResponseAttempt{capture: capture, stop: func() error { return nil }})
}
