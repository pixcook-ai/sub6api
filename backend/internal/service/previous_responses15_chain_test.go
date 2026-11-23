package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

type previousResponse15Repo struct {
	StoredResponseRepository
	records map[string]*StoredResponse
}

func (r *previousResponse15Repo) GetForOwner(_ context.Context, user, group int64, id string) (*StoredResponse, error) {
	v := r.records[id]
	if v == nil || v.UserID != user || v.GroupID != group {
		return nil, ErrStoredResponseNotFound
	}
	copy := *v
	return &copy, nil
}

func (r *previousResponse15Repo) GetForKeyOwner(ctx context.Context, user, key, group int64, id string) (*StoredResponse, error) {
	row, err := r.GetForOwner(ctx, user, group, id)
	if err != nil {
		return nil, err
	}
	if row.APIKeyID != key {
		return nil, ErrStoredResponseNotFound
	}
	return row, nil
}
func (r *previousResponse15Repo) GetManagementModelForKeyOwner(ctx context.Context, user, key, group int64, id string) (string, error) {
	row, err := r.GetForKeyOwner(ctx, user, key, group, id)
	if err != nil {
		return "", err
	}
	return row.Model, nil
}
func (r *previousResponse15Repo) DeleteForKeyOwner(ctx context.Context, user, key, group int64, id string) error {
	_, err := r.GetForKeyOwner(ctx, user, key, group, id)
	if err != nil {
		return err
	}
	delete(r.records, id)
	return nil
}
func TestPreviousResponse15ChainFreshOwnerAndAuthorization(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	material, err := BuildStoredResponseTextRecovery15([]byte(`"old"`), []byte(`{"object":"response","status":"completed","output":[]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	repo := &previousResponse15Repo{records: map[string]*StoredResponse{"resp_local_00000000000000000000000000000001": {PublicID: "resp_local_00000000000000000000000000000001", UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Model: "fixture", State: "completed", ExecutionState: "terminal", ExpiresAt: time.Now().Add(time.Hour), RecoveryVersion: 1, Recovery: material}}}
	svc, _ := NewStoredResponseService(repo, cfg)
	checks := 0
	authorize := func(_ context.Context, owner StoredResponseOwner, record *StoredResponse) error {
		checks++
		if owner.APIKeyID != 2 || record.AccountID != 4 {
			t.Fatal("current owner/binding lost")
		}
		return nil
	}
	body, account, err := svc.ResolvePreviousResponseText15(context.Background(), StoredResponseOwner{1, 2, 3}, "resp_local_00000000000000000000000000000001", []byte(`{"model":"fixture","input":"new"}`), authorize)
	if err != nil || account != 4 || checks != 1 {
		t.Fatalf("resolve: %s %d %d %v", body, account, checks, err)
	}
	var value map[string]any
	json.Unmarshal(body, &value)
	if len(value["input"].([]any)) != 2 {
		t.Fatalf("unexpected expansion: %s", body)
	}
	for _, owner := range []StoredResponseOwner{{2, 2, 3}, {1, 2, 8}, {1, 0, 3}, {1, 9, 3}} {
		_, _, err := svc.ResolvePreviousResponseText15(context.Background(), owner, "resp_local_00000000000000000000000000000001", []byte(`{"model":"fixture","input":"new"}`), authorize)
		if err == nil {
			t.Fatal("cross-owner/group or missing current key authorized")
		}
	}
	_, _, err = svc.ResolvePreviousResponseText15(context.Background(), StoredResponseOwner{1, 2, 3}, "resp_local_00000000000000000000000000000001", []byte(`{"model":"fixture","input":"new"}`), func(context.Context, StoredResponseOwner, *StoredResponse) error {
		return ErrBPSRequestAuthorizationRevoked
	})
	if !errors.Is(err, ErrBPSRequestAuthorizationRevoked) {
		t.Fatalf("revocation: %v", err)
	}
	if _, _, err = svc.ResolvePreviousResponseText15(context.Background(), StoredResponseOwner{1, 2, 3}, "resp_local_00000000000000000000000000000001", []byte(`{"model":"fixture","input":"new"}`), nil); err == nil {
		t.Fatal("missing authorizer accepted")
	}
}

func TestPreviousResponse15ChainRejectsRevokedOrUnboundedHistory(t *testing.T) {
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	material, err := BuildStoredResponseTextRecovery15([]byte(`"old"`), []byte(`{"object":"response","status":"completed","output":[]}`), 4096)
	if err != nil {
		t.Fatal(err)
	}
	id := "resp_local_00000000000000000000000000000001"
	original := StoredResponse{PublicID: id, UserID: 1, APIKeyID: 2, GroupID: 3, AccountID: 4, Model: "fixture", State: "completed", ExecutionState: "terminal", ExpiresAt: time.Now().Add(time.Hour), RecoveryVersion: 1, Recovery: material}
	for _, tc := range []struct {
		name   string
		change func(*StoredResponse)
	}{
		{"expired", func(r *StoredResponse) { r.ExpiresAt = time.Now().Add(-time.Second) }},
		{"deleted", func(r *StoredResponse) { now := time.Now(); r.DeletedAt = &now }},
		{"model changed", func(r *StoredResponse) { r.Model = "other" }},
		{"not terminal", func(r *StoredResponse) { r.ExecutionState = "unknown" }},
		{"legacy", func(r *StoredResponse) { r.RecoveryVersion = 0 }},
		{"cycle", func(r *StoredResponse) { r.PreviousResponseID = id }},
		{"missing material", func(r *StoredResponse) { r.Recovery = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := original
			tc.change(&record)
			repo := &previousResponse15Repo{records: map[string]*StoredResponse{id: &record}}
			svc, _ := NewStoredResponseService(repo, cfg)
			if _, _, err := svc.ResolvePreviousResponseText15(context.Background(), StoredResponseOwner{1, 2, 3}, id, []byte(`{"model":"fixture","input":"new"}`), func(context.Context, StoredResponseOwner, *StoredResponse) error { return nil }); err == nil {
				t.Fatal("accepted unavailable history")
			}
		})
	}
	repo := &previousResponse15Repo{records: map[string]*StoredResponse{}}
	prior := ""
	for index := 1; index <= 33; index++ {
		record := original
		record.PublicID = fmt.Sprintf("resp_local_%032x", index)
		record.PreviousResponseID = prior
		repo.records[record.PublicID] = &record
		prior = record.PublicID
	}
	svc, _ := NewStoredResponseService(repo, cfg)
	if _, _, err := svc.ResolvePreviousResponseText15(context.Background(), StoredResponseOwner{1, 2, 3}, prior, []byte(`{"model":"fixture","input":"new"}`), func(context.Context, StoredResponseOwner, *StoredResponse) error { return nil }); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatalf("deep history: %v", err)
	}
}
