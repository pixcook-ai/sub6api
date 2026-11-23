package managedfiles_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func exampleManifest() managedfiles.AdmitManifest {
	return managedfiles.AdmitManifest{
		Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: "request-1",
		Expected: managedfiles.Identity{BackendID: "backend-1", SubjectFingerprint: "sha256:" + strings.Repeat("1", 64), CredentialGeneration: "epoch-1"},
		OwnerRef: "owner-1", GroupRef: "group-1",
		RequestTemplate: json.RawMessage(`{"model":"fixture","metadata":{"n":9007199254740993},"input":[{"role":"user","content":[{"type":"input_text","text":"Read both"},{"type":"input_file","file_id":"file-public-a"},{"type":"input_file","file_id":"file-public-b"},{"type":"input_file","file_id":"file-public-a"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{\"file_id\":\"file-public-a\"}"}]}`),
		Files: []managedfiles.File{
			{FileRef: "file-public-a", Mode: "upload", Part: "file_0", Filename: "a.txt", MIME: "text/plain", Bytes: 2, SHA256: strings.Repeat("a", 64), Positions: []managedfiles.Position{{Input: 0, Part: 1}, {Input: 0, Part: 3}}},
			{FileRef: "file-public-b", Mode: "existing", Filename: "b.pdf", MIME: "application/pdf", Bytes: 3, SHA256: strings.Repeat("b", 64), RemoteID: "file-remote-b", OperationID: "operation-b", Positions: []managedfiles.Position{{Input: 0, Part: 2}}},
		},
	}
}

func TestManifestRejectsWrongAndUndeclaredFilePositions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*managedfiles.AdmitManifest)
		code, param string
	}{
		{"duplicate ref", func(m *managedfiles.AdmitManifest) { m.Files[1].FileRef = m.Files[0].FileRef }, "invalid_managed_manifest", "files[1].file_ref"},
		{"duplicate position", func(m *managedfiles.AdmitManifest) {
			m.Files[0].Positions = append(m.Files[0].Positions, m.Files[0].Positions[0])
		}, "invalid_managed_position", "files[0].positions"},
		{"crossed position", func(m *managedfiles.AdmitManifest) { m.Files[0].Positions[0].Part = 2 }, "invalid_managed_position", "files[0].positions"},
		{"missing position", func(m *managedfiles.AdmitManifest) { m.Files[0].Positions = m.Files[0].Positions[:1] }, "invalid_managed_position", "files"},
		{"negative position", func(m *managedfiles.AdmitManifest) { m.Files[0].Positions[0].Input = -1 }, "invalid_managed_position", "files[0].positions"},
		{"unknown remote", func(m *managedfiles.AdmitManifest) { m.Files[1].RemoteID = "not-a-remote-id" }, "invalid_managed_manifest", "files[1]"},
		{"no prior operation", func(m *managedfiles.AdmitManifest) { m.Files[1].OperationID = "" }, "invalid_managed_manifest", "files[1]"},
		{"sha field", func(m *managedfiles.AdmitManifest) { m.Files[0].SHA256 = "bad" }, "invalid_managed_manifest", "files[0].sha256"},
		{"unsafe name", func(m *managedfiles.AdmitManifest) { m.Files[0].Filename = "../private.txt" }, "invalid_managed_manifest", "files[0].filename"},
		{"budget repeated per position", func(m *managedfiles.AdmitManifest) { m.Files[0].Bytes = 200 << 20; m.Files[1].Bytes = 200 << 20 }, "managed_file_budget", "files[1].positions"},
		{"image file ID mixed", func(m *managedfiles.AdmitManifest) {
			m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `{"type":"input_text","text":"Read both"}`, `{"type":"input_image","file_id":"file-public-image"}`, 1))
		}, "unsupported_managed_input", "request_template.input[0].content[0]"},
		{"inline file mixed", func(m *managedfiles.AdmitManifest) {
			m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `{"type":"input_file","file_id":"file-public-b"}`, `{"type":"input_file","file_data":"aGVsbG8="}`, 1))
		}, "unsupported_managed_input", "request_template.input[0].content[2].file_data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := exampleManifest()
			tc.change(&m)
			err := managedfiles.ValidateManifest(m)
			var typed *managedfiles.Error
			if !errors.As(err, &typed) || typed.Code != tc.code || typed.Param != tc.param {
				t.Fatalf("got %#v; want %s %s", err, tc.code, tc.param)
			}
		})
	}
}

func TestManifestRejectsImageFileIDsIncludingAmbiguousAliases(t *testing.T) {
	for _, fields := range []string{
		`"file_id":"file-public-image"`, `"file_id":null`, `"file_id":""`, `"file_id":17`,
		`"FILE_ID":"file-public-image"`, `"File_Id":null`, `"file_ID":""`,
		`"file_id":"file-public-image","FILE_ID":null`, `"\u0066ile_id":"file-public-image"`,
	} {
		t.Run(fields, func(t *testing.T) {
			m := exampleManifest()
			image := `{"type":"input_image","image_url":"https://example.invalid/image.png",` + fields + `}`
			m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), `{"type":"input_text","text":"Read both"}`, image, 1))
			for _, err := range []error{
				managedfiles.ValidateManifest(m),
				func() error {
					_, err := managedfiles.ReplacedWire(m, map[string]string{"file-public-a": "file-remote-a", "file-public-b": "file-remote-b"})
					return err
				}(),
			} {
				var typed *managedfiles.Error
				if !errors.As(err, &typed) || typed.Code != "unsupported_managed_input" || typed.Param != "request_template.input[0].content[0]" {
					t.Fatalf("accepted ambiguous image ID or wrong rejection: %#v", err)
				}
			}
		})
	}
}

func TestManifestStillRejectsTopLevelImages(t *testing.T) {
	m := exampleManifest()
	const call = `{"type":"function_call","call_id":"c","name":"f","arguments":"{\"file_id\":\"file-public-a\"}"}`
	m.RequestTemplate = []byte(strings.Replace(string(m.RequestTemplate), call, call+`,{"type":"input_image","image_url":"https://example.invalid/image.png"}`, 1))
	var typed *managedfiles.Error
	if err := managedfiles.ValidateManifest(m); !errors.As(err, &typed) || typed.Code != "unsupported_managed_input" || typed.Param != "request_template.input[2]" {
		t.Fatalf("changed top-level image boundary: %#v", err)
	}
}

func TestManifestToolOutputImagesPreservePairedReferences(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		t.Run(kind, func(t *testing.T) {
			m := exampleManifest()
			parsed, err := managedfiles.Parse(m.RequestTemplate)
			if err != nil {
				t.Fatal(err)
			}
			source := parsed.(map[string]any)
			input := source["input"].([]any)
			call := input[1].(map[string]any)
			callKind, outputKind := "function_call", "function_call_output"
			if kind == "custom" {
				callKind, outputKind = "custom_tool_call", "custom_tool_call_output"
				delete(call, "arguments")
				call["input"] = "opaque image request"
			}
			call["type"] = callKind
			const imageURL = "https://example.invalid/image.png"
			source["input"] = append(input, map[string]any{"type": outputKind, "call_id": "c", "output": []any{map[string]any{"type": "input_image", "image_url": imageURL, "detail": "high"}}})
			source["tools"] = []any{map[string]any{"type": kind, "name": "f"}}
			m.RequestTemplate, err = json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			original := string(m.RequestTemplate)
			if err := managedfiles.ValidateManifest(m); err != nil {
				t.Fatalf("paired typed image rejected by file manifest: %v", err)
			}
			wire, err := managedfiles.ReplacedWire(m, map[string]string{"file-public-a": "file-remote-a", "file-public-b": "file-remote-b"})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err = managedfiles.Parse(wire)
			if err != nil {
				t.Fatal(err)
			}
			items := parsed.(map[string]any)["input"].([]any)
			gotCall := items[1].(map[string]any)
			result := items[2].(map[string]any)
			image := result["output"].([]any)[0].(map[string]any)
			if gotCall["type"] != callKind || gotCall["call_id"] != "c" || result["type"] != outputKind || result["call_id"] != "c" || image["type"] != "input_image" || image["image_url"] != imageURL || image["detail"] != "high" {
				t.Fatalf("manifest replacement changed the paired tool image: %s", wire)
			}
			parts := items[0].(map[string]any)["content"].([]any)
			if parts[1].(map[string]any)["file_id"] != "file-remote-a" || parts[2].(map[string]any)["file_id"] != "file-remote-b" || parts[3].(map[string]any)["file_id"] != "file-remote-a" {
				t.Fatalf("declared file replacement changed alongside image preservation: %s", wire)
			}
			if string(m.RequestTemplate) != original {
				t.Fatal("replacement mutated the caller template")
			}
		})
	}
}

func TestDecodeManifestRejectsMissingFractionalAndExponentialIndices(t *testing.T) {
	for _, position := range []string{`{"input":0}`, `{"input":0,"part":1e0}`, `{"input":0,"part":1.0}`, `{"input":0,"part":-0}`, `{"input":0,"part":1,"extra":true}`} {
		raw, err := json.Marshal(exampleManifest())
		if err != nil {
			t.Fatal(err)
		}
		raw = []byte(strings.Replace(string(raw), `{"input":0,"part":1}`, position, 1))
		if _, err := managedfiles.DecodeManifest(raw); err == nil {
			t.Fatalf("accepted position %s", position)
		}
	}
}

func TestDecodeManifestRejectsCaseAliasesAndNullOptionalStrings(t *testing.T) {
	for _, change := range []struct{ name, old, replacement string }{
		{"manifest alias", `"contract":`, `"CONTRACT":`},
		{"identity alias", `"backend_id":`, `"BACKEND_ID":`},
		{"file alias", `"filename":`, `"FILENAME":`},
		{"alias duplicate", `"contract":"bps-files/1"`, `"contract":"bps-files/1","CONTRACT":"bps-files/1"`},
		{"null optional part", `"mode":"existing"`, `"mode":"existing","part":null`},
		{"null optional remote", `"mode":"upload"`, `"mode":"upload","remote_id":null`},
		{"unknown property", `"contract":`, `"extra":true,"contract":`},
	} {
		t.Run(change.name, func(t *testing.T) {
			raw, err := json.Marshal(exampleManifest())
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), change.old, change.replacement, 1))
			if _, err := managedfiles.DecodeManifest(raw); err == nil {
				t.Fatal("accepted ambiguous or non-schema manifest")
			}
		})
	}
}

func TestManifestCountsRepeatedFileTowardMessageBudget(t *testing.T) {
	m := exampleManifest()
	m.Files[0].Bytes = 200 << 20
	m.Files[1].Bytes = 100 << 20
	if err := managedfiles.ValidateManifest(m); err != nil {
		t.Fatalf("exact 500MiB should pass structural budget: %v", err)
	}
}

func TestReplacedWireOnlyChangesDeclaredFilePositions(t *testing.T) {
	m := exampleManifest()
	got, err := managedfiles.ReplacedWire(m, map[string]string{"file-public-a": "file-remote-a", "file-public-b": "file-remote-b"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"input":[{"content":[{"text":"Read both","type":"input_text"},{"file_id":"file-remote-a","type":"input_file"},{"file_id":"file-remote-b","type":"input_file"},{"file_id":"file-remote-a","type":"input_file"}],"role":"user"},{"arguments":"{\"file_id\":\"file-public-a\"}","call_id":"c","name":"f","type":"function_call"}],"metadata":{"n":9007199254740993},"model":"fixture"}`
	if string(got) != want {
		t.Fatalf("wire mismatch\ngot %s\nwant%s", got, want)
	}
	if !strings.Contains(string(m.RequestTemplate), `"file_id":"file-public-a"`) {
		t.Fatal("mutated caller template")
	}
}

func TestReplacedWirePreservesOpaqueImagesArgumentsAndSidecars(t *testing.T) {
	m := exampleManifest()
	m.RequestTemplate = json.RawMessage(`{"model":"fixture","metadata":{"n":9007199254740993,"screenshot":{"type":"input_image","file_id":"file-public-a"},"sidecar":{"type":"input_file","file_id":"file-public-b","label":"原样🧪","n":18446744073709551615}},"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/截图.png?opaque=file-public-a","detail":"high","sidecar":{"file_id":"file-public-b","n":9007199254740993,"label":"保留雪"}},{"type":"input_file","file_id":"file-public-a"},{"type":"input_image","image_url":"data:image/png;base64,opaque-file-public-b","sidecar":{"type":"input_file","file_id":"file-public-a"}},{"type":"input_file","file_id":"file-public-b"},{"type":"input_file","file_id":"file-public-a"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{\"screenshot\":{\"type\":\"input_image\",\"file_id\":\"file-public-a\"},\"n\":9007199254740993,\"label\":\"参数雪\"}"}]}`)
	m.Files[0].Positions = []managedfiles.Position{{Input: 0, Part: 1}, {Input: 0, Part: 4}}
	m.Files[1].Positions = []managedfiles.Position{{Input: 0, Part: 3}}
	original := string(m.RequestTemplate)
	got, err := managedfiles.ReplacedWire(m, map[string]string{"file-public-a": "file-remote-a", "file-public-b": "file-remote-b"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"fixture","metadata":{"n":9007199254740993,"screenshot":{"type":"input_image","file_id":"file-public-a"},"sidecar":{"type":"input_file","file_id":"file-public-b","label":"原样🧪","n":18446744073709551615}},"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/截图.png?opaque=file-public-a","detail":"high","sidecar":{"file_id":"file-public-b","n":9007199254740993,"label":"保留雪"}},{"type":"input_file","file_id":"file-remote-a"},{"type":"input_image","image_url":"data:image/png;base64,opaque-file-public-b","sidecar":{"type":"input_file","file_id":"file-public-a"}},{"type":"input_file","file_id":"file-remote-b"},{"type":"input_file","file_id":"file-remote-a"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{\"screenshot\":{\"type\":\"input_image\",\"file_id\":\"file-public-a\"},\"n\":9007199254740993,\"label\":\"参数雪\"}"}]}`
	canonical, err := managedfiles.Canonical([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("opaque wire changed\ngot %s\nwant %s", got, canonical)
	}
	if string(m.RequestTemplate) != original {
		t.Fatal("mutated caller template")
	}
	if m.Files[0].Positions[1].Part != 4 || m.Files[1].Positions[0].Part != 3 {
		t.Fatal("mutated declared positions")
	}
}

func TestManifestEnforcesUniqueFilesAndPerMessagePositionLimits(t *testing.T) {
	for _, count := range []int{128, 129} {
		m := exampleManifest()
		m.Files = nil
		input := make([]any, count)
		for i := 0; i < count; i++ {
			ref := fmt.Sprintf("file-public-%d", i)
			input[i] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_file", "file_id": ref}}}
			m.Files = append(m.Files, managedfiles.File{
				FileRef: ref, Mode: "existing", Filename: "a.txt", MIME: "text/plain", Bytes: 1,
				SHA256: strings.Repeat("a", 64), RemoteID: fmt.Sprintf("file-remote-%d", i),
				OperationID: fmt.Sprintf("operation-%d", i), Positions: []managedfiles.Position{{Input: i, Part: 0}},
			})
		}
		m.RequestTemplate, _ = json.Marshal(map[string]any{"input": input})
		err := managedfiles.ValidateManifest(m)
		if (err == nil) != (count == 128) {
			t.Fatalf("%d unique files: %v", count, err)
		}
	}
	for _, count := range []int{20, 21} {
		m := exampleManifest()
		m.Files = m.Files[:1]
		m.Files[0].Positions = nil
		content := make([]any, count)
		for i := range content {
			content[i] = map[string]any{"type": "input_file", "file_id": m.Files[0].FileRef}
			m.Files[0].Positions = append(m.Files[0].Positions, managedfiles.Position{Input: 0, Part: i})
		}
		m.RequestTemplate, _ = json.Marshal(map[string]any{"input": []any{map[string]any{"role": "user", "content": content}}})
		err := managedfiles.ValidateManifest(m)
		if (err == nil) != (count == 20) {
			t.Fatalf("%d repeated positions: %v", count, err)
		}
	}
}
