package service

import (
	"encoding/json"
	"testing"
)

func TestStoredResponsePureTextDefaultsToLocalStorage(t *testing.T) {
	r, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"hello","instructions":"be brief"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Store || string(r.Input) != `"hello"` {
		t.Fatalf("unexpected request: %+v", r)
	}
	var up map[string]json.RawMessage
	if err = json.Unmarshal(r.UpstreamBody, &up); err != nil {
		t.Fatal(err)
	}
	if string(up["store"]) != "false" {
		t.Fatal("upstream must remain stateless")
	}
}

func TestStoredResponseStatelessToolsHaveNoHistoryCopy(t *testing.T) {
	r, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","store":false,"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"tools":[{"type":"function"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Store || r.Input != nil || r.Parameters != nil {
		t.Fatal("stateless request retained recoverable content")
	}
}

func TestStoredResponseRejectsAmbiguousAndUnsupportedStorage(t *testing.T) {
	for _, body := range []string{
		`{"model":"fixture","input":"x","store":null}`,
		`{"model":"fixture","input":"x","store":false,"store":true}`,
		`{"model":"fixture","input":"x","st\u006fre":false,"store":true}`,
		`{"model":"fixture","input":"x","tools":[{"type":"function"}]}`,
		`{"model":"fixture","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
		`{"model":"fixture","input":"x","previous_response_id":"resp_prior"}`,
		`{"model":"fixture","input":"x","conversation":"conv_prior"}`,
		`{"model":"fixture","input":"x","background":true}`,
		`{"model":"fixture","input":[{"role":"user","content":"x","native_replay":"hidden"}]}`,
		`{"model":"fixture","input":"x","text":{"native_replay":"hidden"}}`,
		`{"model":"fixture","input":"x","reasoning":{"native_replay":"hidden"}}`,
		`{"model":"fixture","input":"x","reasoning_effort":{"native_replay":"hidden"}}`,
		`{"model":"fixture","input":"x","tool_choice":{"native_replay":"hidden"}}`,
		`{"model":"fixture","input":"x","truncation":{"native_replay":"hidden"}}`,
	} {
		if _, err := NormalizeStoredResponseRequest([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestStoredResponseAcceptsPlainMessageHistory(t *testing.T) {
	_, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"role":"assistant","content":"hello"}],"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
}
