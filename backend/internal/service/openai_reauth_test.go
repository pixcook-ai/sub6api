package service

import (
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCredRepo struct {
	mu  sync.Mutex
	rec *OpenAILoginCredentialRecord
}

func (f *fakeCredRepo) Upsert(ctx context.Context, rec *OpenAILoginCredentialRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *rec
	f.rec = &c
	return nil
}

func (f *fakeCredRepo) Get(ctx context.Context, id int64) (*OpenAILoginCredentialRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rec == nil {
		return nil, nil
	}
	c := *f.rec
	return &c, nil
}

func (f *fakeCredRepo) RecordAttempt(ctx context.Context, id int64, success bool, code string, next *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if success {
		f.rec.FailCount, f.rec.NextAttemptAt, f.rec.LastCode = 0, nil, ""
		return nil
	}
	f.rec.FailCount++
	f.rec.NextAttemptAt, f.rec.LastCode = next, code
	return nil
}

func (f *fakeCredRepo) Delete(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec = nil
	return nil
}

func (f *fakeCredRepo) Exists(ctx context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec != nil, nil
}

type fakeB64Enc struct{}

func (fakeB64Enc) Encrypt(p string) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(p)), nil
}

func (fakeB64Enc) Decrypt(c string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(c)
	return string(b), err
}

type fakeReauthAdmin struct {
	AdminService
	account *Account
	updated *UpdateAccountInput
}

func (f *fakeReauthAdmin) GetAccount(ctx context.Context, id int64) (*Account, error) {
	return f.account, nil
}

func (f *fakeReauthAdmin) UpdateAccount(ctx context.Context, id int64, in *UpdateAccountInput) (*Account, error) {
	f.updated = in
	return f.account, nil
}

type fakeClearErrRepo struct {
	AccountRepository
	cleared int
}

func (f *fakeClearErrRepo) ClearError(ctx context.Context, id int64) error {
	f.cleared++
	return nil
}

func newReauthTest(run batchLoginRunFunc) (*OpenAIReauthService, *fakeCredRepo, *fakeReauthAdmin, *fakeClearErrRepo) {
	repo := &fakeCredRepo{}
	admin := &fakeReauthAdmin{account: &Account{ID: 1, Platform: PlatformOpenAIBPS, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "ws-keep"}}}
	clear := &fakeClearErrRepo{}
	svc := NewOpenAIReauthService(repo, fakeB64Enc{}, admin, clear, &OpenAIBatchLoginService{run: run})
	return svc, repo, admin, clear
}

func saveTestCredential(t *testing.T, svc *OpenAIReauthService) {
	t.Helper()
	e := BatchLoginEntry{Email: "a@x.co", Password: "pw-secret", TOTPSecret: "JBSWY3DPEHPK3PXP"}
	if err := svc.SaveCredential(context.Background(), 1, BatchLoginTargetBPS, e); err != nil {
		t.Fatal(err)
	}
}

func TestReauthStoresOnlyCiphertext(t *testing.T) {
	svc, repo, _, _ := newReauthTest(nil)
	saveTestCredential(t, svc)
	if strings.Contains(repo.rec.PasswordCiphertext, "pw-secret") || strings.Contains(repo.rec.TOTPCiphertext, "JBSWY3DP") {
		t.Fatal("plaintext stored")
	}
	if ok, _ := svc.HasCredential(context.Background(), 1); !ok {
		t.Fatal("credential not reported")
	}
	_ = svc.DeleteCredential(context.Background(), 1)
	if ok, _ := svc.HasCredential(context.Background(), 1); ok {
		t.Fatal("credential not deleted")
	}
}

func TestReauthSuccessUpdatesAccountAndKeepsWorkspace(t *testing.T) {
	var gotPassword string
	run := func(ctx context.Context, provider string, e BatchLoginEntry, proxy string, onProgress func(string)) (*batchLoginRecord, string) {
		gotPassword = e.Password
		return bpsRecord("a@x.co"), ""
	}
	svc, repo, admin, clear := newReauthTest(run)
	saveTestCredential(t, svc)
	repo.rec.FailCount = 2
	svc.reauthorize(context.Background(), 1, PlatformOpenAIBPS)
	if gotPassword != "pw-secret" {
		t.Fatalf("password not decrypted for login: %q", gotPassword)
	}
	if admin.updated == nil || admin.updated.Credentials["chatgpt_account_id"] != "ws-keep" || admin.updated.Credentials["access_token"] != "at" {
		t.Fatalf("unexpected update: %+v", admin.updated)
	}
	if clear.cleared != 1 || repo.rec.FailCount != 0 {
		t.Fatalf("cleared=%d fail=%d", clear.cleared, repo.rec.FailCount)
	}
}

func TestReauthFailureCooldownAndLimit(t *testing.T) {
	calls := 0
	run := func(ctx context.Context, provider string, e BatchLoginEntry, proxy string, onProgress func(string)) (*batchLoginRecord, string) {
		calls++
		return nil, "login_failed"
	}
	svc, repo, admin, _ := newReauthTest(run)
	saveTestCredential(t, svc)
	ctx := context.Background()
	svc.reauthorize(ctx, 1, PlatformOpenAIBPS)
	if repo.rec.FailCount != 1 || repo.rec.NextAttemptAt == nil || !repo.rec.NextAttemptAt.After(time.Now()) || repo.rec.LastCode != "login_failed" {
		t.Fatalf("first failure: %+v", repo.rec)
	}
	svc.reauthorize(ctx, 1, PlatformOpenAIBPS) // still cooling down
	if calls != 1 {
		t.Fatalf("cooldown ignored, calls=%d", calls)
	}
	for i := 0; i < 4; i++ {
		repo.rec.NextAttemptAt = nil
		svc.reauthorize(ctx, 1, PlatformOpenAIBPS)
	}
	if calls != openAIReauthMaxFailures || repo.rec.FailCount != openAIReauthMaxFailures {
		t.Fatalf("limit not enforced: calls=%d fail=%d", calls, repo.rec.FailCount)
	}
	if admin.updated != nil {
		t.Fatal("account must not be updated on failure")
	}
}

func TestReauthCooldownGrowth(t *testing.T) {
	if openAIReauthCooldown(1) != 15*time.Minute || openAIReauthCooldown(2) != 30*time.Minute || openAIReauthCooldown(30) != openAIReauthMaxCooldown {
		t.Fatal("unexpected cooldown schedule")
	}
}

func TestReauthCredentialsMergeKeepsWorkspaceAndModels(t *testing.T) {
	existing := map[string]any{
		"access_token": "old", "refresh_token": "old-rt", "chatgpt_account_id": "ws-keep",
		"client_id": "cid-old", "model_mapping": map[string]any{"alias": "gpt-5.6-sol"},
	}
	svc, _, _, _ := newReauthTest(nil)
	creds, code := svc.batch.buildCredentials(BatchLoginTargetBPS, "bps_excel", "a@x.co", bpsRecord("a@x.co"))
	if creds == nil {
		t.Fatalf("build failed: %s", code)
	}
	creds["chatgpt_account_id"] = existing["chatgpt_account_id"]
	merged, err := NormalizeOpenAIBPSCredentials(AccountTypeOAuth, creds, existing)
	if err != nil {
		t.Fatal(err)
	}
	if merged["chatgpt_account_id"] != "ws-keep" || merged["access_token"] != "at" || merged["refresh_token"] != "rt" {
		t.Fatalf("unexpected merge: %+v", merged)
	}
	mm, _ := merged["model_mapping"].(map[string]any)
	if mm["alias"] != "gpt-5.6-sol" {
		t.Fatalf("model mapping lost: %+v", merged["model_mapping"])
	}
}
