package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPreviousResponseText15KeepsInputButNotRoundInstructions(t *testing.T) {
	previous := []byte(`{"object":"response","status":"completed","instructions":"old secret instruction","output":[{"type":"message","id":"msg_prior","status":"completed","role":"assistant","content":[{"type":"output_text","text":"old answer","annotations":[]}]}]}`)
	material, err := BuildStoredResponseTextRecovery15([]byte(`"old question"`), previous, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(material), "old secret instruction") || strings.Contains(string(material), "msg_prior") {
		t.Fatal("round parameters or display identity entered replay material")
	}
	body, err := expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"new question","stream":true}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, inherited := got["instructions"]; inherited {
		t.Fatal("inherited previous instructions")
	}
	input := got["input"].([]any)
	if len(input) != 3 || input[0].(map[string]any)["content"] != "old question" || input[2].(map[string]any)["content"] != "new question" {
		t.Fatalf("history/current input changed: %s", body)
	}
	if got["store"] != false || got["stream"] != true {
		t.Fatalf("upstream options changed: %s", body)
	}
	body, err = expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"new question","instructions":"current only"}`), 4096)
	if err != nil || !strings.Contains(string(body), "current only") {
		t.Fatalf("current instructions lost: %s %v", body, err)
	}
}

func TestPreviousResponseText15RejectsMissingNativeMaterial(t *testing.T) {
	for _, output := range []string{
		`[{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"}]`,
		`[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"file_citation","file_id":"file_1"}]}]}]`,
	} {
		_, err := BuildStoredResponseTextRecovery15([]byte(`"question"`), []byte(`{"object":"response","status":"completed","output":`+output+`}`), 4096)
		if err == nil {
			t.Fatal("accepted display without native recovery")
		}
	}
	for _, status := range []string{"failed", "incomplete", "in_progress"} {
		if _, err := BuildStoredResponseTextRecovery15([]byte(`"question"`), []byte(`{"object":"response","status":"`+status+`","output":[]}`), 4096); err == nil {
			t.Fatal("accepted noncompleted response")
		}
	}
}

func TestPreviousResponseText15BoundsAndVersionAreExplicit(t *testing.T) {
	material, err := BuildStoredResponseTextRecovery15([]byte(`"question"`), []byte(`{"object":"response","status":"completed","output":[]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, corrupt := range []string{
		strings.Replace(string(material), `"version":1`, `"version":2`, 1),
		strings.Replace(string(material), `"kind":"text"`, `"kind":"native"`, 1),
		strings.TrimSuffix(string(material), "}") + `,"authorization":"must not persist"}`,
		`{"version":1,"version":1,"kind":"text","input":"x","output":[]}`,
	} {
		if _, err := expandPreviousResponseText15([]byte(corrupt), []byte(`{"model":"fixture","input":"next"}`), 4096); err == nil {
			t.Fatalf("accepted corrupt recovery: %s", corrupt)
		}
	}
	if _, err := expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"next"}`), int64(len(material)-1)); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("material budget: %v", err)
	}
	if _, err := expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"`+strings.Repeat("x", 300)+`"}`), 250); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("final wire budget: %v", err)
	}
}

func TestPreviousResponseText15BranchesDoNotChangeParent(t *testing.T) {
	material, err := BuildStoredResponseTextRecovery15([]byte(`[{"role":"developer","content":"explicit historical input"},{"role":"user","content":"first"}]`), []byte(`{"object":"response","status":"completed","output":[]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	before := string(material)
	for _, current := range []string{"branch one", "branch two"} {
		body, err := expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"`+current+`"}`), 4096)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(body), current) != 1 || !strings.Contains(string(body), "explicit historical input") {
			t.Fatalf("branch expansion: %s", body)
		}
	}
	if string(material) != before {
		t.Fatal("mutated parent material")
	}
}

func TestPreviousResponseText15RefusalReentersTextAdmission(t *testing.T) {
	material, err := BuildStoredResponseTextRecovery15([]byte(`"question"`), []byte(`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"cannot assist"}]}]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := expandPreviousResponseText15(material, []byte(`{"model":"fixture","input":"new question"}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	// The expanded wire is stateless; force storage admission so the normalizer
	// cannot return before checking each recovered text item.
	var admission map[string]json.RawMessage
	if err := json.Unmarshal(expanded, &admission); err != nil {
		t.Fatal(err)
	}
	admission["store"] = json.RawMessage(`true`)
	admissionBody, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NormalizeStoredResponseRequest(admissionBody); err != nil {
		t.Fatalf("recovered refusal rejected by text admission: %s %v", admissionBody, err)
	}
	if !strings.Contains(string(expanded), `"text":"cannot assist"`) {
		t.Fatalf("refusal text lost: %s", expanded)
	}
}

func TestPreviousResponseText15StatelessCannotBypassRecoveryContract(t *testing.T) {
	material, err := BuildStoredResponseTextRecovery15([]byte(`"question"`), []byte(`{"object":"response","status":"completed","output":[]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"model":"fixture","store":false,"input":"next","tools":[{"type":"function","name":"read"}]}`,
		`{"model":"fixture","store":false,"input":"next","previous_response_id":"other"}`,
		`{"model":"fixture","store":false,"input":"next","conversation":"other"}`,
	} {
		if _, err := expandPreviousResponseText15(material, []byte(body), 4096); err == nil {
			t.Fatalf("stateless bypass: %s", body)
		}
	}
}
