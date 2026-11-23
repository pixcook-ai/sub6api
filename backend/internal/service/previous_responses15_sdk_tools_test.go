//go:build recovery15native

package service

import (
	"context"
	"testing"
)

func TestStoredNative15SDKStrictToolAdmission(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","store":true,"stream":false,"input":"sub6-fixture-tool-chain:javascript-json","tools":[{"type":"function","name":"fixture_read_record","description":"Read synthetic records","strict":true,"parameters":{"type":"object","properties":{"path":{"type":"string"},"options":{"type":"object","properties":{"limit":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}}},"required":["limit","tags"],"additionalProperties":false}},"required":["path","options"],"additionalProperties":false}}]}`)
	req, err := NormalizeStoredResponseNativeRequest15(body)
	if err != nil {
		t.Fatalf("normalize SDK: %v", err)
	}
	if _, _, err = expandedNativeFileRequest15(req.UpstreamBody, nil); err != nil {
		t.Fatalf("expand SDK input: %v", err)
	}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	repo := &native15BeginRepository{}
	svc, _ := NewStoredResponseService(repo, cfg)
	admission := native15AdmissionFixture(req)
	admission.maxBytes = 1 << 20
	if _, err = svc.BeginNative15(context.Background(), req, admission, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("reserve SDK: %v", err)
	}
}
