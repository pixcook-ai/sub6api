package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestStoredResponseMetadataRoundTripsLocally(t *testing.T) {
	req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","metadata":{"ticket":"local-42"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var upstream map[string]any
	if err := json.Unmarshal(req.UpstreamBody, &upstream); err != nil {
		t.Fatal(err)
	}
	if _, exists := upstream["metadata"]; exists {
		t.Fatal("client metadata must not reach upstream")
	}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := capture.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[],"metadata":{"internal":"never-public"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Metadata) != 1 || response.Metadata["ticket"] != "local-42" {
		t.Fatalf("metadata not preserved: %s", raw)
	}
	if string(db.terminal.PublicResponse) != string(raw) {
		t.Fatal("stored and created response differ")
	}
}

func TestStoredResponseMetadataMatchesStringMapAndUnicodeLimits(t *testing.T) {
	valid := map[string]any{strings.Repeat("你", 64): strings.Repeat("🌱", 512)}
	for index := 0; index < 15; index++ {
		valid[fmt.Sprintf("k%d", index)] = "v"
	}
	encode := func(metadata any) []byte {
		b, err := json.Marshal(map[string]any{"model": "fixture", "input": "x", "metadata": metadata})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := NormalizeStoredResponseRequest(encode(valid)); err != nil {
		t.Fatalf("valid Unicode metadata rejected: %v", err)
	}
	tooMany := map[string]any{}
	for i := 0; i < 17; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	for name, value := range map[string]any{"too-many": tooMany, "long-key": map[string]any{strings.Repeat("你", 65): "v"}, "long-value": map[string]any{"k": strings.Repeat("🌱", 513)}, "number": map[string]any{"k": 42}, "boolean": map[string]any{"k": true}, "nested": map[string]any{"k": map[string]any{}}, "array": []any{}, "string": "value"} {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeStoredResponseRequest(encode(value))
			var policy *managedfiles.ResponsesPreflightError
			if !errors.As(err, &policy) || policy.Status != 400 || policy.Type != "invalid_request_error" || policy.Param != "metadata" {
				t.Fatalf("invalid metadata lacks precise error: %v", err)
			}
		})
	}
	for name, value := range map[string]any{"null": nil, "empty": map[string]any{}} {
		t.Run(name, func(t *testing.T) {
			req, err := NormalizeStoredResponseRequest(encode(value))
			if err != nil {
				t.Fatal(err)
			}
			var up, params map[string]any
			json.Unmarshal(req.UpstreamBody, &up)
			json.Unmarshal(req.Parameters, &params)
			if _, ok := up["metadata"]; ok {
				t.Fatal("metadata went upstream")
			}
			_, exists := params["metadata"]
			if exists != (name == "empty") {
				t.Fatalf("null/empty presence wrong: %s", req.Parameters)
			}
		})
	}
}

func TestStoredResponseMetadataRejectsMutatedParametersBeforeReservation(t *testing.T) {
	req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","metadata":{"ticket":"original"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Parameters = json.RawMessage(`{"metadata":{"ticket":"mutated"}}`)
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	if _, err = svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"}); !errors.Is(err, ErrStoredResponseInvalid) || db.reservations != 0 {
		t.Fatalf("mutated parameters reserved: %v/%d", err, db.reservations)
	}
}

func TestStoredResponseMetadataPersistsAcrossSSETerminalAndInterruptedSnapshots(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","metadata":{"ticket":"local-42"}}`))
			if err != nil {
				t.Fatal(err)
			}
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			db := &captureTestDB{}
			svc, _ := NewStoredResponseService(db, cfg)
			capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := capture.CaptureEvent(context.Background(), "response.created", []byte(`{"type":"response.created","response":{"object":"response","status":"in_progress","output":[],"metadata":{"internal":"never-public"}}}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"ticket":"local-42"`) || strings.Contains(string(raw), "never-public") {
				t.Fatalf("created SSE metadata wrong: %s", raw)
			}
			if terminal {
				raw, err = capture.CaptureEvent(context.Background(), "response.completed", []byte(`{"type":"response.completed","response":{"object":"response","status":"completed","output":[]}}`))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), `"ticket":"local-42"`) {
					t.Fatalf("terminal SSE metadata lost: %s", raw)
				}
			} else if err = capture.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(db.terminal.PublicResponse), `"ticket":"local-42"`) || strings.Contains(string(db.terminal.PublicResponse), "never-public") {
				t.Fatalf("saved metadata wrong: %s", db.terminal.PublicResponse)
			}
		})
	}
}
