package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

type fakeBatchAdmin struct {
	AdminService
	mu       sync.Mutex
	existing []Account
	created  []*CreateAccountInput
}

func (f *fakeBatchAdmin) ListAccounts(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode string, sortBy, sortOrder string) ([]Account, int64, error) {
	var out []Account
	for _, a := range f.existing {
		if strings.EqualFold(a.Name, search) {
			out = append(out, a)
		}
	}
	return out, int64(len(out)), nil
}

func (f *fakeBatchAdmin) CreateAccount(ctx context.Context, in *CreateAccountInput) (*Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	return &Account{ID: int64(len(f.created))}, nil
}

func newBatchLoginTestService(t *testing.T, admin AdminService, run batchLoginRunFunc, concurrency int) *OpenAIBatchLoginService {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run-login.mjs"), []byte("//"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &OpenAIBatchLoginService{
		cfg:   config.BatchLoginConfig{ToolsDir: dir, MaxRows: 100, Concurrency: concurrency},
		admin: admin, run: run, jobs: map[string]*batchLoginJobState{},
	}
}

func waitBatchLoginJob(t *testing.T, svc *OpenAIBatchLoginService, id string) *BatchLoginJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if job, ok := svc.Get(id); ok && job.Status != BatchLoginStatusRunning {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func bpsRecord(email string) *batchLoginRecord {
	return &batchLoginRecord{Provider: "bps_excel", Email: email, WorkspaceID: "ws1", ClientID: openai.ExcelClientID,
		AccessToken: "at", RefreshToken: "rt", IDToken: "it", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
}

func entries(emails ...string) []BatchLoginEntry {
	out := make([]BatchLoginEntry, 0, len(emails))
	for _, e := range emails {
		out = append(out, BatchLoginEntry{Email: e, Password: "pw-secret", TOTPSecret: "JBSWY3DPEHPK3PXP"})
	}
	return out
}

func TestValidateBatchLoginInput(t *testing.T) {
	ok := &BatchLoginInput{Entries: entries("A@Example.com"), Targets: []string{"bps", "bps", "codex"}}
	if err := ValidateBatchLoginInput(ok, 100); err != nil {
		t.Fatal(err)
	}
	if ok.Entries[0].Email != "a@example.com" || len(ok.Targets) != 2 {
		t.Fatalf("not normalized: %+v", ok)
	}
	bad := []*BatchLoginInput{
		{Entries: nil, Targets: []string{"bps"}},
		{Entries: entries("a@b.co", "A@b.co"), Targets: []string{"bps"}},
		{Entries: entries("notanemail"), Targets: []string{"bps"}},
		{Entries: []BatchLoginEntry{{Email: "a@b.co", Password: "x", TOTPSecret: "not base32!"}}, Targets: []string{"bps"}},
		{Entries: entries("a@b.co"), Targets: nil},
		{Entries: entries("a@b.co"), Targets: []string{"other"}},
	}
	for i, in := range bad {
		if err := ValidateBatchLoginInput(in, 100); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
	if err := ValidateBatchLoginInput(&BatchLoginInput{Entries: entries("a@b.co", "c@d.co"), Targets: []string{"bps"}}, 1); err == nil {
		t.Fatal("row limit not enforced")
	}
}

func TestBatchLoginCreatesSkipsAndFails(t *testing.T) {
	admin := &fakeBatchAdmin{existing: []Account{{Name: "dup@example.com"}}}
	run := func(ctx context.Context, provider string, e BatchLoginEntry, proxy string) (*batchLoginRecord, string) {
		switch e.Email {
		case "bad@example.com":
			return nil, "login_failed"
		case "mismatch@example.com":
			return bpsRecord("other@example.com"), ""
		case "client@example.com":
			r := bpsRecord(e.Email)
			r.ClientID = "wrong"
			return r, ""
		}
		return bpsRecord(e.Email), ""
	}
	svc := newBatchLoginTestService(t, admin, run, 2)
	job, err := svc.Start(context.Background(), &BatchLoginInput{
		Entries: entries("ok@example.com", "dup@example.com", "bad@example.com", "mismatch@example.com", "client@example.com"),
		Targets: []string{"bps"}, BPSGroupIDs: []int64{7},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := waitBatchLoginJob(t, svc, job.ID)
	want := map[string][2]string{
		"ok@example.com": {BatchLoginItemCreated, ""}, "dup@example.com": {BatchLoginItemSkipped, "already_exists"},
		"bad@example.com": {BatchLoginItemFailed, "login_failed"}, "mismatch@example.com": {BatchLoginItemFailed, "email_mismatch"},
		"client@example.com": {BatchLoginItemFailed, "client_mismatch"},
	}
	for _, it := range done.Items {
		if got := [2]string{it.Status, it.Code}; got != want[it.Email] {
			t.Errorf("%s: got %v want %v", it.Email, got, want[it.Email])
		}
	}
	if len(admin.created) != 1 {
		t.Fatalf("created %d accounts", len(admin.created))
	}
	in := admin.created[0]
	if in.Platform != PlatformOpenAIBPS || in.Type != AccountTypeOAuth || in.Credentials["client_id"] != openai.ExcelClientID ||
		in.Credentials["chatgpt_account_id"] != "ws1" || len(in.GroupIDs) != 1 || in.GroupIDs[0] != 7 {
		t.Fatalf("unexpected create input: %+v", in)
	}
	raw, _ := json.Marshal(done)
	if strings.Contains(string(raw), "pw-secret") || strings.Contains(string(raw), "JBSWY3DP") {
		t.Fatal("job snapshot leaks secrets")
	}
}

func TestBatchLoginConcurrencyLimit(t *testing.T) {
	var cur, max int32
	run := func(ctx context.Context, provider string, e BatchLoginEntry, proxy string) (*batchLoginRecord, string) {
		n := atomic.AddInt32(&cur, 1)
		for {
			m := atomic.LoadInt32(&max)
			if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		return bpsRecord(e.Email), ""
	}
	svc := newBatchLoginTestService(t, &fakeBatchAdmin{}, run, 2)
	job, err := svc.Start(context.Background(), &BatchLoginInput{Entries: entries("a@x.co", "b@x.co", "c@x.co", "d@x.co", "e@x.co"), Targets: []string{"bps"}})
	if err != nil {
		t.Fatal(err)
	}
	waitBatchLoginJob(t, svc, job.ID)
	if max != 2 {
		t.Fatalf("max concurrency %d, want 2", max)
	}
}

func TestBatchLoginCancel(t *testing.T) {
	run := func(ctx context.Context, provider string, e BatchLoginEntry, proxy string) (*batchLoginRecord, string) {
		<-ctx.Done()
		return nil, "canceled"
	}
	svc := newBatchLoginTestService(t, &fakeBatchAdmin{}, run, 1)
	job, err := svc.Start(context.Background(), &BatchLoginInput{Entries: entries("a@x.co", "b@x.co", "c@x.co"), Targets: []string{"bps"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !svc.Cancel(job.ID) {
		t.Fatal("cancel failed")
	}
	done := waitBatchLoginJob(t, svc, job.ID)
	if done.Status != BatchLoginStatusCanceled {
		t.Fatalf("status %s", done.Status)
	}
	for _, it := range done.Items {
		if it.Status != BatchLoginItemCanceled {
			t.Fatalf("item %s status %s", it.Email, it.Status)
		}
	}
}

func TestParseBatchLoginOutput(t *testing.T) {
	rec, code := parseBatchLoginOutput([]byte("noise\n{\"ok\":true,\"record\":{\"email\":\"a@b.co\"}}\n"), nil)
	if rec == nil || code != "" || rec.Email != "a@b.co" {
		t.Fatalf("got %+v %q", rec, code)
	}
	if _, code := parseBatchLoginOutput([]byte("{\"ok\":false,\"code\":\"login_timeout\"}\n"), nil); code != "login_timeout" {
		t.Fatalf("code %q", code)
	}
	if _, code := parseBatchLoginOutput([]byte("{\"ok\":false,\"code\":\"codex_chatgpt_preflight_risk_control\"}\n"), nil); code != "codex_chatgpt_preflight_risk_control" {
		t.Fatalf("staged code %q", code)
	}
	if _, code := parseBatchLoginOutput([]byte("{\"ok\":false,\"code\":\"Bad Code With Secret pw\"}"), nil); code != "login_failed" {
		t.Fatalf("unsafe code leaked: %q", code)
	}
	if _, code := parseBatchLoginOutput(nil, nil); code != "login_failed" {
		t.Fatalf("empty output code %q", code)
	}
}
