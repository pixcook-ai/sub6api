//go:build recovery15native

package service

import (
	"context"
	"errors"
	"testing"
)

func TestNativeFiles15RechecksIndependentFilesContractAfterWait(t *testing.T) {
	changes := map[string]func(*Account){
		"capability": func(a *Account) {
			a.Credentials["openai_capabilities"] = []string{"chat_completions", "responses", "bps_local_responses"}
		},
		"service_key": func(a *Account) { a.Credentials["bps_files_service_key"] = "changed-only-files" },
		"control_url": func(a *Account) { a.Credentials["bps_files_control_url"] = "https://different.example" },
		"backend":     func(a *Account) { a.Credentials["bps_files_backend_id"] = "different" },
		"generation":  func(a *Account) { a.Credentials["bps_files_credential_generation"] = "next" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			svc, _, record, accounts, _ := native15ParentFixture(t)
			accounts.current.Credentials["openai_capabilities"] = []string{"chat_completions", "responses", "bps_local_responses", "bps_files"}
			for _, field := range []string{"contract", "control_url", "service_key", "backend_id", "subject_fingerprint", "credential_generation"} {
				accounts.current.Credentials["bps_files_"+field] = accounts.current.Credentials["bps_responses_"+field]
			}
			accounts.current.Credentials["bps_files_contract"] = "bps-files/1"
			selected, err := cloneStoredNativeParentAccount15(accounts.current)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := BPSFilesConfigForAccount(selected)
			if err != nil {
				t.Fatal(err)
			}
			storedBefore, err := BPSStoredResponsesConfigForAccount(selected)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error { change(accounts.current); return nil })
			err = svc.authorizeNativeFileAccount15(ctx, StoredResponseOwner{record.UserID, record.APIKeyID, record.GroupID}, selected, expected, "public")
			if !errors.Is(err, ErrBPSRequestAuthorizationRevoked) {
				t.Fatalf("independent files revocation escaped %s: %v", name, err)
			}
			storedAfter, err := BPSStoredResponsesConfigForAccount(accounts.current)
			if err != nil || storedBefore != storedAfter {
				t.Fatal("test accidentally changed Responses contract")
			}
		})
	}
}
