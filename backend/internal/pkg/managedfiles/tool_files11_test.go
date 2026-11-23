package managedfiles_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestToolFiles11BindingKeepsTypedOutputAndOpaqueArguments(t *testing.T) {
	m := exampleManifest()
	m.RequestTemplate = json.RawMessage(`{"store":false,"input":[{"type":"function_call","call_id":"call_read","name":"read","arguments":"{\"file_id\":\"file-public-a\"}"},{"type":"function_call_output","call_id":"call_read","output":[{"type":"input_text","text":"first"},{"type":"input_file","file_id":"file-public-a"}]},{"type":"custom_tool_call","call_id":"call_run","name":"run","input":"file-public-b"},{"type":"custom_tool_call_output","call_id":"call_run","output":[{"type":"input_file","file_id":"file-public-b"}]}]}`)
	m.Files[0].Positions = []managedfiles.Position{{Input: 1, Part: 1}}
	m.Files[1].Positions = []managedfiles.Position{{Input: 3, Part: 0}}
	wire, err := managedfiles.ReplacedWire(m, map[string]string{"file-public-a": "file-remote-a", "file-public-b": "file-remote-b"})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(wire, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]any)
	if input[0].(map[string]any)["arguments"] != `{"file_id":"file-public-a"}` || input[2].(map[string]any)["input"] != "file-public-b" {
		t.Fatal("opaque tool input changed")
	}
	for _, want := range []string{`"type":"function_call_output"`, `"type":"custom_tool_call_output"`, `"file_id":"file-remote-a"`, `"file_id":"file-remote-b"`} {
		if !strings.Contains(string(wire), want) {
			t.Fatalf("missing typed output binding %s", want)
		}
	}
}

func TestToolFiles11RejectsAmbiguousSourcesBeforeBinding(t *testing.T) {
	const call = `{"type":"function_call","call_id":"call_read","name":"read","arguments":"{}"}`
	const output = `{"type":"function_call_output","call_id":"call_read","output":[{"type":"input_file","file_id":"file-public-a"}]}`
	for name, input := range map[string]string{
		"later duplicate call":         call + "," + output + "," + call,
		"later duplicate text result":  call + "," + output + `,{"type":"function_call_output","call_id":"call_read","output":"again"}`,
		"later alias-only call type":   call + "," + output + `,{"Type":"function_call","call_id":"call_read","name":"read","arguments":"{}"}`,
		"later alias-only call id":     call + "," + output + `,{"type":"function_call","Call_ID":"call_read","name":"read","arguments":"{}"}`,
		"later alias-only output type": call + "," + output + `,{"Type":"function_call_output","call_id":"call_read","output":"again"}`,
		"later alias-only output id":   call + "," + output + `,{"type":"function_call_output","Call_ID":"call_read","output":"again"}`,
		"later both aliases":           call + "," + output + `,{"Type":"custom_tool_call","Call_ID":"call_read","name":"run","input":"opaque"}`,
		"later kind alias":             call + "," + output + `,{"type":"FUNCTION_CALL","call_id":"call_read","name":"read","arguments":"{}"}`,
		"source call alias":            strings.Replace(call, `"call_id":"call_read"`, `"call_id":"call_read","Call_ID":"call_other"`, 1) + "," + output,
		"output id alias":              call + "," + strings.Replace(output, `"call_id":"call_read"`, `"call_id":"call_read","Call_ID":"call_other"`, 1),
		"output content alias":         call + "," + strings.Replace(output, `"output":`, `"Content":[],"output":`, 1),
		"wrong earlier call":           strings.Replace(call, "function_call", "custom_tool_call", 1) + "," + output,
		"output before call":           output + "," + call,
	} {
		t.Run(name, func(t *testing.T) {
			m := exampleManifest()
			m.RequestTemplate = []byte(`{"store":false,"input":[` + input + `]}`)
			m.Files = m.Files[:1]
			m.Files[0].Positions = []managedfiles.Position{{Input: 1, Part: 0}}
			if name == "output before call" {
				m.Files[0].Positions[0].Input = 0
			}
			if err := managedfiles.ValidateManifest(m); err == nil {
				t.Fatal("ambiguous tool source accepted")
			}
		})
	}
}

func TestToolFiles11KeepsAliasTextOpaqueWithoutMedia(t *testing.T) {
	root, err := managedfiles.Parse([]byte(`{"input":[{"Type":"function_call","Call_ID":"call_read","arguments":"{\"Type\":\"function_call_output\",\"Call_ID\":\"call_read\"}"},{"type":"custom_tool_call","call_id":"call_other","input":"{\"Type\":\"function_call\",\"Call_ID\":\"call_read\"}"},{"type":"function_call_output","call_id":"call_read","output":"{\"type\":\"input_file\",\"file_id\":\"file-opaque\"}"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	sources, err := managedfiles.ValidateToolFileSources(root.(map[string]any)["input"].([]any))
	if err != nil || len(sources) != 0 {
		t.Fatalf("text-only replay changed: sources=%v err=%v", sources, err)
	}
}
