package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStoredResponseStatelessCaptureKeepsOnePublicIdentity(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, err := NewStoredResponseService(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture-v1"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := capture.CaptureEvent(context.Background(), "response.created", []byte(`{"type":"response.created","response":{"id":"resp_upstream","object":"response","status":"in_progress","model":"fixture","output":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := capture.CaptureEvent(context.Background(), "response.completed", []byte(`{"type":"response.completed","response":{"id":"resp_upstream","object":"response","status":"completed","model":"fixture","output":[{"type":"message","id":"item_keep","role":"assistant","content":[{"type":"output_text","text":"resp_upstream call_keep","annotations":[]}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var first, last struct {
		Response struct {
			ID     string
			Store  bool
			Output []struct{ ID string }
		}
	}
	if err = json.Unmarshal(created, &first); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(terminal, &last); err != nil {
		t.Fatal(err)
	}
	if first.Response.ID == "resp_upstream" || first.Response.ID != last.Response.ID || last.Response.Store || last.Response.Output[0].ID != "item_keep" {
		t.Fatalf("unstable identity: %s %s", created, terminal)
	}
	if _, err = svc.Get(context.Background(), StoredResponseOwner{1, 2, 3}, last.Response.ID); err != ErrStoredResponseNotFound {
		t.Fatalf("stateless lookup: %v", err)
	}
}

func TestStoredResponseRejectsMismatchedTerminalBeforeCapture(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, _ := NewStoredResponseService(nil, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","store":false}`))
	c, _ := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	_, err := c.CaptureEvent(context.Background(), "response.completed", []byte(`{"type":"response.completed","response":{"id":"resp_old","object":"response","status":"failed","output":[]}}`))
	if err == nil {
		t.Fatal("accepted mismatched terminal")
	}
	_, err = c.CaptureEvent(context.Background(), "response.failed", []byte(`{"type":"response.failed","response":{"id":"resp_old","object":"response","status":"failed","output":[],"error":{"code":"later"}}}`))
	if err != nil {
		t.Fatalf("invalid event incorrectly committed terminal: %v", err)
	}
}

func TestStoredResponseRejectsUnsupportedTerminalReferenceBeforeCommit(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CaptureEvent(context.Background(), "response.completed", []byte(`{"type":"response.completed","response_id":"native_unknown_reference","response":{"object":"response","status":"completed","output":[]}}`))
	if !errors.Is(err, ErrStoredResponseUnsupported) {
		t.Fatalf("unsupported terminal reference: %v", err)
	}
	if db.terminal != nil {
		t.Fatal("invalid terminal event was persisted before validation completed")
	}
	if err = c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.terminal.State != "failed" {
		t.Fatalf("invalid event blocked interrupted cleanup: %s", db.terminal.State)
	}
}

func TestStoredResponseStorageRejectsPrivateOutputFields(t *testing.T) {
	for _, raw := range []string{
		`{"object":"response","status":"completed","output":[],"native_replay":{"secret":"hidden"}}`,
		`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","private":"hidden"}]}]}`,
		`{"object":"response","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"fn","arguments":"{}"}]}`,
		`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[{"type":"file_citation","file_id":"file_1"}]}]}]}`,
	} {
		cfg := DefaultStoredResponseConfig()
		cfg.Enabled = true
		svc, _ := NewStoredResponseService(&captureTestDB{}, cfg)
		req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
		c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.CaptureJSON(context.Background(), []byte(raw)); err == nil {
			t.Errorf("accepted private output: %s", raw)
		}
	}
}

func TestStoredResponseRejectsNestedValuesInScalarFieldsBeforePersistence(t *testing.T) {
	for _, fields := range []string{
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","logprobs":{"native_replay":"secret"}}]}]`,
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","refusal":{"native_replay":"secret"}}]}]`,
		`"output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no","text":{"native_replay":"secret"}}]}]`,
		`"output":[{"type":"message","id":{"native_replay":"secret"},"role":"assistant","content":[]}]`,
		`"output":[{"type":"message","status":{"native_replay":"secret"},"role":"assistant","content":[]}]`,
		`"output":[],"temperature":{"native_replay":"secret"}`,
		`"output":[],"service_tier":{"native_replay":"secret"}`,
		`"output":[],"tool_choice":{"native_replay":"secret"}`,
		`"output":[],"background":{"native_replay":"secret"}`,
		`"output":[],"max_output_tokens":{"native_replay":"secret"}`,
	} {
		for _, stream := range []bool{false, true} {
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			db := &captureTestDB{}
			svc, _ := NewStoredResponseService(db, cfg)
			req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
			c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if stream {
				_, err = c.CaptureEvent(context.Background(), "response.in_progress", []byte(`{"type":"response.in_progress","response":{"object":"response","status":"in_progress",`+fields+`}}`))
			} else {
				_, err = c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed",`+fields+`}`))
			}
			if err == nil {
				t.Errorf("stream=%t accepted unsafe public content: %s", stream, fields)
			}
			if db.terminal != nil {
				t.Errorf("stream=%t persisted unsafe public content: %s", stream, fields)
			}
		}
	}
}

// Minimal database boundary adapter for deterministic capture failure tests.
// PostgreSQL transaction/authorization acceptance belongs to real-PG tests.
type captureTestDB struct {
	terminal     *StoredResponse
	finishErr    error
	reservations int
}

func (d *captureTestDB) Reserve(_ context.Context, r *StoredResponse, _ StoredResponseLimits, _ time.Duration, _ time.Duration) error {
	d.reservations++
	r.Version = 1
	return nil
}

func TestStoredResponseRejectsKnownMinimumCapacityBeforeReservation(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	body, _ := json.Marshal(map[string]any{"model": "fixture", "input": "x", "instructions": strings.Repeat("i", 600<<10)})
	req, err := NormalizeStoredResponseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("oversized known minimum: %v", err)
	}
	if db.reservations != 0 {
		t.Fatal("impossible response reserved durable storage")
	}
}

func TestStoredResponseKnownMinimumCapacityBoundary(t *testing.T) {
	// Worked public JSON: 320-byte interrupted object (43-byte ID, 10-digit
	// timestamp, fixture model, client instructions), 3-byte input, 25-byte params.
	for _, budget := range []int64{347, 348} {
		cfg := DefaultStoredResponseConfig()
		cfg.Enabled, cfg.Limits.MaxResponseBytes = true, budget
		db := &captureTestDB{}
		svc, _ := NewStoredResponseService(db, cfg)
		req, err := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","instructions":"client"}`))
		if err != nil {
			t.Fatal(err)
		}
		c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
		if budget == 347 {
			if !errors.Is(err, ErrStoredResponseTooLarge) || db.reservations != 0 {
				t.Fatalf("budget347 admission: %v, reservations=%d", err, db.reservations)
			}
			continue
		}
		if err != nil {
			t.Fatalf("budget348 admission: %v", err)
		}
		if err = c.Abort(context.Background()); err != nil {
			t.Fatalf("budget348 interrupted commit: %v", err)
		}
	}
}

func TestStoredResponsePartialCapacityIncludesClientInstructions(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	body, _ := json.Marshal(map[string]any{"model": "fixture", "input": "x", "instructions": strings.Repeat("i", 350<<10)})
	req, err := NormalizeStoredResponseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": strings.Repeat("t", 400<<10)})
	if _, err = c.CaptureEvent(context.Background(), "response.output_text.delta", delta); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("oversized partial snapshot: %v", err)
	}
	if err = c.Abort(context.Background()); err != nil {
		t.Fatalf("oversized progress prevented bounded cleanup: %v", err)
	}
	var saved struct{ Output []any }
	if err = json.Unmarshal(db.terminal.PublicResponse, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Output) != 0 {
		t.Fatal("rejected oversized snapshot changed retained facts")
	}
}
func (d *captureTestDB) GetForOwner(context.Context, int64, int64, string) (*StoredResponse, error) {
	return nil, ErrStoredResponseNotFound
}
func (d *captureTestDB) GetManagementModel(context.Context, int64, int64, string) (string, error) {
	return "", ErrStoredResponseNotFound
}
func (d *captureTestDB) Finalize(_ context.Context, _ string, _ string, _ int64, v StoredResponseTerminal) (*StoredResponse, error) {
	if d.finishErr != nil {
		return nil, d.finishErr
	}
	d.terminal = &StoredResponse{PublicResponse: v.PublicResponse, State: v.State}
	return d.terminal, nil
}
func (d *captureTestDB) Renew(context.Context, string, string, int64, time.Duration) error {
	return nil
}
func (d *captureTestDB) Delete(context.Context, int64, int64, string) error { return nil }
func (d *captureTestDB) Sweep(context.Context, int) (int, error)            { return 0, nil }

func (d *captureTestDB) GetForKeyOwner(context.Context, int64, int64, int64, string) (*StoredResponse, error) {
	return nil, ErrStoredResponseNotFound
}
func (d *captureTestDB) GetManagementModelForKeyOwner(context.Context, int64, int64, int64, string) (string, error) {
	return "", ErrStoredResponseNotFound
}
func (d *captureTestDB) DeleteForKeyOwner(context.Context, int64, int64, int64, string) error {
	return ErrStoredResponseNotFound
}

func TestStoredResponseStoredCaptureDoesNotEchoInjectedInstructions(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","instructions":"client only"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","model":"private_model","instructions":"internal secret","output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v["instructions"] != "client only" || v["model"] != "fixture" {
		t.Fatalf("upstream context leaked: %s", raw)
	}
}

func TestStoredResponseCommitFailureDoesNotReturnCompleted(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{finishErr: errors.New("synthetic database unavailable")}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`))
	if err == nil || raw != nil {
		t.Fatalf("returned success on commit failure: %s %v", raw, err)
	}
	db.finishErr = nil
	if err = c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.terminal == nil || db.terminal.State != "failed" {
		t.Fatal("interrupted state not recoverable")
	}
}

func TestStoredResponseTerminalStatesPersistWithOneIdentity(t *testing.T) {
	for _, state := range []string{"completed", "incomplete", "failed"} {
		t.Run(state, func(t *testing.T) {
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			db := &captureTestDB{}
			svc, _ := NewStoredResponseService(db, cfg)
			req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
			c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"object":"response","status":"` + state + `","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
			got, err := c.CaptureJSON(context.Background(), body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(db.terminal.PublicResponse) {
				t.Fatal("stored object differs from returned object")
			}
			same, err := c.CaptureJSON(context.Background(), body)
			if err != nil || string(same) != string(got) {
				t.Fatalf("duplicate terminal: %s %v", same, err)
			}
		})
	}
}

func TestStoredResponseRejectsProgressAfterTerminal(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, _ := NewStoredResponseService(nil, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x","store":false}`))
	c, _ := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if _, err := c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CaptureEvent(context.Background(), "response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"late"}`)); err == nil {
		t.Fatal("accepted late delta")
	}
}

func TestStoredResponseStoppingLeaseAllowsTerminalCommit(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, _ := NewStoredResponseService(&captureTestDB{}, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := c.KeepLease(context.Background())
	if err = stop(); err != nil {
		t.Fatal(err)
	}
	if err = ctx.Err(); err != nil {
		t.Fatalf("normal lease stop invalidates terminal commit context: %v", err)
	}
}

func TestStoredResponseStorageRejectsPrivateNestedFields(t *testing.T) {
	for _, suffix := range []string{
		`,"usage":{"input_tokens":2,"native_replay":"secret"}`,
		`,"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":1,"token":"secret"}}`,
		`,"metadata":{"upstream_auth":"secret"}`,
		`,"reasoning":{"encrypted_content":"secret"}`,
		`,"text":{"format":{"type":"text"},"native_instructions":"secret"}`,
	} {
		cfg := DefaultStoredResponseConfig()
		cfg.Enabled = true
		svc, _ := NewStoredResponseService(&captureTestDB{}, cfg)
		req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
		c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[]`+suffix+`}`)); err == nil {
			t.Errorf("accepted private nested response field: %s", suffix)
		}
	}
}

func TestStoredResponseCapturesMinimalFailureDiagnostic(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, _ := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	got, err := c.CaptureJSON(context.Background(), []byte(`{"error":{"type":"server_error","code":"basispoints_protocol_error","message":"Basispoints returned an invalid response stream."},"response":{"status":"failed","id":"resp_native","usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15}}}`))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err = json.Unmarshal(got, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["error"].(map[string]any)["code"] != "basispoints_protocol_error" || db.terminal.State != "failed" {
		t.Fatalf("lost safe failure: %s", got)
	}
}

func TestStoredResponseAbortRetainsObservedPartialTextAndUsage(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, _ := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	_, err := c.CaptureEvent(context.Background(), "response.in_progress", []byte(`{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"usage":{"input_tokens":11,"output_tokens":0,"total_tokens":11}}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CaptureEvent(context.Background(), "response.output_text.delta", []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_1","delta":"partial hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err = json.Unmarshal(db.terminal.PublicResponse, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["usage"] == nil || len(obj["output"].([]any)) != 1 {
		t.Fatalf("lost observed facts: %s", db.terminal.PublicResponse)
	}
	part := obj["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["text"] != "partial hello" {
		t.Fatalf("lost partial text: %s", db.terminal.PublicResponse)
	}
}

func TestStoredResponseCommitFailureRetainsObservedTerminalUsage(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	db := &captureTestDB{finishErr: errors.New("synthetic unavailable")}
	svc, _ := NewStoredResponseService(db, cfg)
	req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
	c, _ := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
	_, err := c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`))
	if err == nil {
		t.Fatal("expected storage failure")
	}
	db.finishErr = nil
	if err = c.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	json.Unmarshal(db.terminal.PublicResponse, &saved)
	if saved["usage"] == nil {
		t.Fatal("observed usage was lost on storage failure")
	}
}

func TestStoredResponseAbortPreservesSparseUsageUpdates(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		initial, update, want string
		commitFailure         bool
	}{
		{name: "missing counters remain known", initial: `{"input_tokens":11,"output_tokens":4,"total_tokens":15}`, update: `{"input_tokens":11}`, want: `{"input_tokens":11,"output_tokens":4,"total_tokens":15}`},
		{name: "total only is known", initial: `null`, update: `{"total_tokens":15}`, want: `{"total_tokens":15}`},
		{name: "details remain known", initial: `{"input_tokens":11,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`, update: `{"input_tokens":12,"input_tokens_details":{}}`, want: `{"input_tokens":12,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}`},
		{name: "failed terminal commit retains prior counters", initial: `{"input_tokens":11,"output_tokens":4,"total_tokens":15}`, update: `{"input_tokens":11}`, want: `{"input_tokens":11,"output_tokens":4,"total_tokens":15}`, commitFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			db := &captureTestDB{}
			svc, _ := NewStoredResponseService(db, cfg)
			req, _ := NormalizeStoredResponseRequest([]byte(`{"model":"fixture","input":"x"}`))
			c, err := svc.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, req, StoredResponseBinding{AccountID: 4, Scope: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.CaptureEvent(context.Background(), "response.in_progress", []byte(`{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"usage":`+tc.initial+`}}`)); err != nil {
				t.Fatal(err)
			}
			if tc.commitFailure {
				db.finishErr = errors.New("synthetic unavailable")
				if _, err = c.CaptureJSON(context.Background(), []byte(`{"object":"response","status":"completed","output":[],"usage":`+tc.update+`}`)); err == nil {
					t.Fatal("expected storage failure")
				}
				db.finishErr = nil
			} else if _, err = c.CaptureEvent(context.Background(), "response.in_progress", []byte(`{"type":"response.in_progress","response":{"object":"response","status":"in_progress","output":[],"usage":`+tc.update+`}}`)); err != nil {
				t.Fatal(err)
			}
			if err = c.Abort(context.Background()); err != nil {
				t.Fatal(err)
			}
			var got struct {
				Usage json.RawMessage `json:"usage"`
			}
			if err = json.Unmarshal(db.terminal.PublicResponse, &got); err != nil {
				t.Fatal(err)
			}
			var want, actual any
			if err = json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(got.Usage, &actual); err != nil {
				t.Fatal(err)
			}
			canonicalWant, _ := json.Marshal(want)
			canonicalActual, _ := json.Marshal(actual)
			if string(canonicalActual) != string(canonicalWant) {
				t.Fatalf("usage = %s; want %s", canonicalActual, canonicalWant)
			}
		})
	}
}
