package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type legacyStoredOwnerRepository struct{ StoredResponseRepository }

func (legacyStoredOwnerRepository) GetForOwner(context.Context, int64, int64, string) (*StoredResponse, error) {
	panic("legacy owner-only lookup must not authorize resources")
}
func (legacyStoredOwnerRepository) GetManagementModel(context.Context, int64, int64, string) (string, error) {
	panic("legacy owner-only model lookup must not authorize resources")
}
func (legacyStoredOwnerRepository) Delete(context.Context, int64, int64, string) error {
	panic("legacy owner-only delete must not authorize resources")
}

func TestStoredResourcesRequireStrictKeyOwnerRepository(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	svc, err := NewStoredResponseService(legacyStoredOwnerRepository{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := StoredResponseOwner{1, 2, 3}
	if _, err := svc.Get(context.Background(), owner, "resp_local_00000000000000000000000000000001"); !errors.Is(err, ErrBPSRequestAuthorizationUnavailable) {
		t.Fatal(err)
	}
	if _, err := svc.GetManagementModel(context.Background(), owner, "resp_local_00000000000000000000000000000001"); !errors.Is(err, ErrBPSRequestAuthorizationUnavailable) {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), owner, "resp_local_00000000000000000000000000000001"); !errors.Is(err, ErrBPSRequestAuthorizationUnavailable) {
		t.Fatal(err)
	}
}

func TestStoredResourcesAndTextHistoryRejectDifferentAPIKey(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	id := "resp_local_00000000000000000000000000000001"
	material, err := BuildStoredResponseTextRecovery15([]byte("\"old\""), []byte("{\"object\":\"response\",\"status\":\"completed\",\"output\":[]}"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	repo := &previousResponse15Repo{records: map[string]*StoredResponse{id: {
		PublicID: id, UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Model: "fixture",
		State: "completed", ExecutionState: "terminal", ExpiresAt: time.Now().Add(time.Hour),
		RecoveryVersion: 1, Recovery: material,
	}}}
	svc, err := NewStoredResponseService(repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wrong := StoredResponseOwner{1, 9, 3}
	if _, err := svc.Get(context.Background(), wrong, id); !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal(err)
	}
	if _, err := svc.GetManagementModel(context.Background(), wrong, id); !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), wrong, id); !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal(err)
	}
	calls := 0
	if _, _, err := svc.ResolvePreviousResponseText15(context.Background(), wrong, id, []byte("{\"model\":\"fixture\",\"input\":\"new\"}"), func(context.Context, StoredResponseOwner, *StoredResponse) error { calls++; return nil }); !errors.Is(err, ErrStoredResponseNotFound) {
		t.Fatal(err)
	}
	if calls != 0 || repo.records[id] == nil {
		t.Fatal("different API key reached authorizer or deleted another key's resource")
	}
}
