//go:build recovery15native

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

type native15BeginRepository struct {
	captureTestDB
	record *StoredResponse
}

func (r *native15BeginRepository) Reserve(ctx context.Context, record *StoredResponse, limits StoredResponseLimits, retention, lease time.Duration) error {
	copy := *record
	r.record = &copy
	return r.captureTestDB.Reserve(ctx, record, limits, retention, lease)
}

func TestPreviousResponseNative15BeginPreservesCurrentCatalogAndDelta(t *testing.T) {
	req, err := NormalizeStoredResponseNativeRequest15([]byte(`{"model":"public","input":"question","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"instructions":"current only"}`))
	if err != nil {
		t.Fatal(err)
	}
	admission := native15AdmissionFixture(req)
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	repo := &native15BeginRepository{}
	svc, _ := NewStoredResponseService(repo, cfg)
	checks := 0
	capture, err := svc.BeginNative15(context.Background(), req, admission, func(context.Context) error { checks++; return nil })
	if err != nil || capture == nil || checks != 2 || repo.record == nil || repo.record.RecoveryVersion != 2 {
		t.Fatalf("Begin: %d %v", checks, err)
	}
	if string(repo.record.Input) != string(req.Input) || string(repo.record.Parameters) != string(req.Parameters) {
		t.Fatal("current delta/catalog changed while reserving")
	}
	admission.policyVersion = "bps-responses-puretext-v1"
	if _, err = svc.BeginNative15(context.Background(), req, admission, func(context.Context) error { return nil }); err == nil || repo.reservations != 1 {
		t.Fatal("puretext admission promoted to native")
	}
}

func TestPreviousResponseNative15BeginRefusesImpossibleBudgetBeforeReserve(t *testing.T) {
	req, err := NormalizeStoredResponseNativeRequest15([]byte(`{"model":"public","input":"` + strings.Repeat("x", 100) + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	cfg.Limits.MaxResponseBytes = 1000
	repo := &native15BeginRepository{}
	svc, _ := NewStoredResponseService(repo, cfg)
	if _, err = svc.BeginNative15(context.Background(), req, native15AdmissionFixture(req), func(context.Context) error { return nil }); !errors.Is(err, ErrStoredResponseTooLarge) || repo.reservations != 0 {
		t.Fatalf("impossible budget: %v", err)
	}
}

func TestPreviousResponseNative15BeginRechecksExpiryAfterAuthorizationWait(t *testing.T) {
	req, err := NormalizeStoredResponseNativeRequest15([]byte(`{"model":"public","input":"question"}`))
	if err != nil {
		t.Fatal(err)
	}
	admission := native15AdmissionFixture(req)
	admission.expiresAt = time.Now().Add(100 * time.Millisecond)
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	repo := &native15BeginRepository{}
	svc, _ := NewStoredResponseService(repo, cfg)
	checks := 0
	_, err = svc.BeginNative15(context.Background(), req, admission, func(context.Context) error {
		checks++
		if checks == 2 {
			time.Sleep(150 * time.Millisecond)
		}
		return nil
	})
	if !errors.Is(err, ErrStoredResponseLease) || repo.reservations != 0 || checks != 2 {
		t.Fatalf("expired after authorize: %d %v", checks, err)
	}
}

func native15AdmissionFixture(req *StoredResponseRequest) *storedResponseNativeAdmission15 {
	return &storedResponseNativeAdmission15{owner: StoredResponseOwner{1, 2, 3}, maxBytes: 1000, policyVersion: managedfiles.NativeResponsesPolicy15, publicModel: req.Model, resolvedModel: "resolved", accountID: 4, backendID: "backend", subjectDigest: strings.Repeat("a", 64), credentialGeneration: "generation", admissionID: "responses-" + strings.Repeat("f", 64), admissionBindingSHA256: strings.Repeat("b", 64), nonce: strings.Repeat("d", 64), clientRequestSHA256: storedNativeDigest15(req.UpstreamBody), inputSHA256: storedNativeDigest15(req.Input), parametersSHA256: storedNativeDigest15(req.Parameters), requestWireSHA256: strings.Repeat("c", 64), nativeWireSHA256: strings.Repeat("e", 64), expiresAt: time.Now().Add(20 * time.Second)}
}
