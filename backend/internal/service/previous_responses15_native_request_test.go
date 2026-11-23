//go:build recovery15native

package service

import (
	"encoding/json"
	"testing"
)

// Candidate tests must be installed and observed RED before the implementation.
func TestStoredResponseNative15NormalizesOnlyCurrentToolResultDelta(t *testing.T) {
	body := []byte(`{"model":"public","store":true,"instructions":"current only","input":[{"type":"function_call_output","call_id":"call_read","output":"read result"}],"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"write","parameters":{"type":"object","properties":{"payload":{"type":"string"}}},"strict":true},{"type":"custom","name":"patch","format":{"type":"text"}}]}],"tool_choice":"required","parallel_tool_calls":true,"stream":true}`)
	req, err := NormalizeStoredResponseNativeRequest15(body)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Store || req.Model != "public" {
		t.Fatal("lost client storage/model")
	}
	var input []map[string]any
	if json.Unmarshal(req.Input, &input) != nil || len(input) != 1 || input[0]["call_id"] != "call_read" || input[0]["output"] != "read result" {
		t.Fatalf("delta changed: %s", req.Input)
	}
	var params, wire map[string]any
	json.Unmarshal(req.Parameters, &params)
	json.Unmarshal(req.UpstreamBody, &wire)
	if params["instructions"] != "current only" || params["tool_choice"] != "required" || params["parallel_tool_calls"] != true || wire["store"] != false {
		t.Fatal("lost native parameters or forwarded store")
	}
	if len(params["tools"].([]any)) != 1 || len(wire["input"].([]any)) != 1 {
		t.Fatal("catalog lost or expanded historical input")
	}
	if _, err = NormalizeStoredResponseRequest(body); err == nil {
		t.Fatal("global pure-text normalizer was relaxed")
	}
	second, err := NormalizeStoredResponseNativeRequest15(req.UpstreamBody)
	if err != nil || second.Store || string(second.Input) != string(req.Input) || string(second.Parameters) != string(req.Parameters) {
		t.Fatalf("store:false revalidation lost canonical fields: %v", err)
	}
}

func TestStoredResponseNative15PreservesOmittedVersusEmptyCatalog(t *testing.T) {
	for _, tc := range []struct {
		body    string
		present bool
	}{
		{`{"model":"public","input":"root"}`, false},
		{`{"model":"public","input":"root","tools":[]}`, true},
	} {
		req, err := NormalizeStoredResponseNativeRequest15([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		var params map[string]any
		json.Unmarshal(req.Parameters, &params)
		_, exists := params["tools"]
		if exists != tc.present {
			t.Fatal("omitted and replacement catalog collapsed")
		}
	}
}

func TestStoredResponseNative15RejectsUnsupportedRecoveryInput(t *testing.T) {
	for _, body := range []string{
		`{"model":"public","input":"x","previous_response_id":null}`,
		`{"model":"public","input":"x","conversation":"c"}`,
		`{"model":"public","input":[{"type":"function_call","call_id":"call_x","name":"x","arguments":"{}"}]}`,
		`{"model":"public","input":[{"type":"custom_tool_call","call_id":"call_x","name":"x","input":"x"}]}`,
		`{"model":"public","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_x"}]}]}`,
		`{"model":"public","input":[{"type":"function_call_output","call_id":"call_x","output":[{"type":"input_image","image_url":"https://invalid"}]}]}`,
		`{"model":"public","input":[{"type":"function_call_output","call_id":"call_x","output":"x"},{"type":"custom_tool_call_output","call_id":"call_x","output":"y"}]}`,
		`{"model":"public","input":"x","tools":[{"type":"web_search"}]}`,
		`{"model":"public","input":"x","tools":[{"type":"tool_search"}]}`,
		`{"model":"public","input":"x","tools":[{"type":"namespace","name":"n","tools":[{"type":"namespace","name":"m","tools":[]}]}]}`,
		`{"model":"public","input":"x","tools":[{"type":"function","name":"f","input_schema":{}}]}`,
		`{"model":"public","input":"x","additional_tools":[]}`,
		`{"model":"public","input":"x","tool_choice":{"type":"allowed_tools","tools":[]}}`,
		`{"model":"public","input":"x","unknown":true}`,
		`{"model":"public","input":"x","store":false,"unknown":true}`,
		`{"model":"public","input":"x","tools":[{"type":"function","name":"f","name":"g"}]}`,
		`{"model":"public","input":"x","tools":[{"type":"function","name":"f","parameters":{"x":1,"\u0078":2}}]}`,
	} {
		if _, err := NormalizeStoredResponseNativeRequest15([]byte(body)); err == nil {
			t.Fatalf("accepted unsupported native request: %s", body)
		}
	}
}

func TestStoredResponseNative15AcceptsTextCustomResultAndNamedChoice(t *testing.T) {
	body := []byte(`{"model":"public","input":[{"type":"custom_tool_call_output","call_id":"call_patch","output":[{"type":"input_text","text":"patched"}],"status":"completed"}],"tools":[{"type":"custom","name":"patch","description":"apply change","format":{"type":"grammar","syntax":"regex","definition":".*"}}],"tool_choice":{"type":"custom","name":"patch"}}`)
	if _, err := NormalizeStoredResponseNativeRequest15(body); err != nil {
		t.Fatal(err)
	}
}
