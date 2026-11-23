package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestStoredResponsePlanRetainsOriginalClientIntentAndTrustedOwner(t *testing.T) {
	body := []byte(`{"model":"client-model","input":"hello","instructions":"client instructions"}`)
	owner := StoredResponseOwner{UserID: 1, APIKeyID: 2, GroupID: 3}
	plan, err := NewStoredResponsePlan(owner, body)
	if err != nil {
		t.Fatal(err)
	}
	body[2] = 'x'
	if !plan.Request.Store || plan.Request.Model != "client-model" || plan.Owner != owner {
		t.Fatal("lost original request or trusted owner")
	}
	normalized, err := NormalizeStoredResponseRequest(plan.ClientRequest)
	if err != nil || normalized.Store {
		t.Fatalf("preflight intent must explicitly disable upstream storage: %v", err)
	}
	ctx := WithStoredResponsePlan(context.Background(), plan)
	if StoredResponsePlanFromContext(ctx) != plan || StoredResponsePlanFromContext(context.Background()) != nil {
		t.Fatal("plan must exist only in server context")
	}
}

func TestStoredResponseAttemptWithholdsTerminalUntilCommitAndRecoversUsage(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{finishErr: errors.New("storage unavailable")}
	svc, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NormalizeStoredResponseRequest([]byte(`{"model":"client-model","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, request, StoredResponseBinding{AccountID: 4, Scope: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cleanup := withStoredResponseCapture(context.Background(), capture)
	raw, err := captureStoredResponseJSON(ctx, []byte(`{"object":"response","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":2}}`))
	if err == nil || raw != nil {
		t.Fatal("terminal escaped before successful persistence")
	}
	db.finishErr = nil
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if db.terminal == nil || db.terminal.State != "failed" {
		t.Fatal("failed capture was not recoverable")
	}
	var saved struct {
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		}
	}
	if err = json.Unmarshal(db.terminal.PublicResponse, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Usage.Input != 7 || saved.Usage.Output != 2 {
		t.Fatal("observed usage was lost")
	}
}

func TestStoredResponseAttemptCapturesSSEBeforeReleasingTerminal(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	request, _ := NormalizeStoredResponseRequest([]byte(`{"model":"client-model","input":"hello"}`))
	capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, request, StoredResponseBinding{AccountID: 4, Scope: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cleanup := withStoredResponseCapture(context.Background(), capture)
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	created, err := captureStoredResponseEvent(ctx, "response.created", []byte(`{"type":"response.created","response":{"id":"resp_native","object":"response","status":"in_progress","output":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var first struct{ Response struct{ ID string } }
	if err = json.Unmarshal(created, &first); err != nil {
		t.Fatal(err)
	}
	if first.Response.ID != capture.PublicID() {
		t.Fatal("created event did not use local identity")
	}
	db.finishErr = errors.New("storage unavailable")
	terminal, err := captureStoredResponseEvent(ctx, "response.completed", []byte(`{"type":"response.completed","response":{"id":"resp_native","object":"response","status":"completed","output":[],"usage":{"input_tokens":7}}}`))
	if err == nil || terminal != nil {
		t.Fatal("terminal event escaped before successful persistence")
	}
	db.finishErr = nil
}
