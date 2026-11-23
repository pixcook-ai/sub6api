//go:build recovery15native

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestStoredNativePolicy15MatchesFilesAtBeginReservationAndTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  bool
		policy string
		valid  bool
	}{
		{"legacy text", false, managedfiles.NativeResponsesPolicy15, true},
		{"text rejects attachments policy", false, managedfiles.NativeResponsesAttachmentPolicy15, false},
		{"text rejects unknown policy", false, "unknown", false},
		{"attachments", true, managedfiles.NativeResponsesAttachmentPolicy15, true},
		{"attachments rejects text policy", true, managedfiles.NativeResponsesPolicy15, false},
		{"attachments rejects unknown policy", true, "unknown", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"public","input":"question"}`)
			var snapshot *managedfiles.NativeAttachmentSnapshot15
			if tc.files {
				turn := nativeFileShapeTurn15(t)
				body, _ = json.Marshal(map[string]any{"model": "public", "input": turn["input"]})
				raw, _ := json.Marshal(turn["attachments"])
				decoded, err := managedfiles.DecodeNativeAttachmentSnapshot15(raw)
				if err != nil {
					t.Fatal(err)
				}
				snapshot = &decoded
			}
			request, err := NormalizeStoredResponseNativeRequest15(body)
			if err != nil {
				t.Fatal(err)
			}
			admission := native15AdmissionFixture(request)
			admission.maxBytes = 16384
			admission.policyVersion = tc.policy
			if tc.files {
				admission.preflightRequest.Files = &managedfiles.NativeAttachmentAdmission15{Snapshot: *snapshot}
				admission.attachmentSnapshot = snapshot
			}
			id := "resp_local_" + strings.Repeat("a", 32)
			_, err = admission.terminalBinding15(id)
			if (err == nil) != tc.valid {
				t.Fatalf("terminal policy valid=%v: %v", tc.valid, err)
			}
			cfg := DefaultStoredResponseConfig()
			cfg.Enabled = true
			repo := &native15BeginRepository{}
			svc, err := NewStoredResponseService(repo, cfg)
			if err != nil {
				t.Fatal(err)
			}
			capture, err := svc.BeginNative15(context.Background(), request, admission, func(context.Context) error { return nil })
			if !tc.valid {
				if err == nil || capture != nil || repo.reservations != 0 {
					t.Fatalf("mismatched policy reserved: %v", err)
				}
				return
			}
			if err != nil || capture == nil || repo.record == nil {
				t.Fatalf("matching policy failed Begin: %v", err)
			}
			if _, err = ValidateStoredResponseNativeReservation15(repo.record, time.Now(), cfg.Limits.MaxResponseBytes); err != nil {
				t.Fatalf("matching reservation policy rejected: %v", err)
			}
			repo.record.nativeAdmission15.policyVersion = "unknown"
			if _, err = ValidateStoredResponseNativeReservation15(repo.record, time.Now(), cfg.Limits.MaxResponseBytes); err == nil {
				t.Fatal("reservation accepted mismatched policy")
			}
		})
	}
}
