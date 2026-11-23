package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type previousResponse15CaptureRepository struct {
	captureTestDB
	captured StoredResponseTerminal
}

func (r *previousResponse15CaptureRepository) Finalize(ctx context.Context, id, token string, version int64, terminal StoredResponseTerminal) (*StoredResponse, error) {
	r.captured = terminal
	return r.captureTestDB.Finalize(ctx, id, token, version, terminal)
}
func TestPreviousResponse15CaptureSavesPrivateMaterialBeforeCompleted(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &previousResponse15CaptureRepository{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"question","instructions":"round-only"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "test"})
	if err != nil {
		t.Fatal(err)
	}
	output, err := c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(db.captured.Recovery) == 0 {
		t.Fatal("completed released without private material")
	}
	if err := ValidateStoredResponseTextRecovery15(req.Input, output, db.captured.Recovery, cfg.Limits.MaxResponseBytes); err != nil {
		t.Fatal(err)
	}
	var public map[string]json.RawMessage
	json.Unmarshal(output, &public)
	if public["recovery"] != nil || public["private_recovery"] != nil {
		t.Fatal("private material leaked")
	}
	db = &previousResponse15CaptureRepository{}
	db.finishErr = errors.New("private write failed")
	svc, _ = NewStoredResponseService(db, cfg)
	c, err = svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "test"})
	if err != nil {
		t.Fatal(err)
	}
	output, err = c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[]}`))
	if err == nil || output != nil || len(db.captured.Recovery) == 0 {
		t.Fatal("failed atomic commit released completion or omitted recovery")
	}
}

func TestPreviousResponse15RecoveryBudgetFailureRetainsObservedUsage(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	cfg.Limits.MaxResponseBytes = 500
	db := &previousResponse15CaptureRepository{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"question"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "test"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"object": "response", "status": "completed", "usage": map[string]int{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "a moderately long answer repeated so public and private combined exceed a bounded reservation"}}}}})
	if _, err = c.CaptureJSON(context.Background(), raw); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("private budget: %v", err)
	}
	if err = c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved struct{ Usage map[string]int }
	if err = json.Unmarshal(db.captured.PublicResponse, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Usage["total_tokens"] != 10 {
		t.Fatal("lost observed usage on private budget failure")
	}
}
