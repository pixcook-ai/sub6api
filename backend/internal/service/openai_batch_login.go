package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

const (
	batchLoginJobTTL    = 30 * time.Minute
	batchLoginMaxJobs   = 16
	batchLoginMaxActive = 2
)

type batchLoginWork struct {
	entry  BatchLoginEntry
	target string
}

type batchLoginSettings struct {
	ProxyID            *int64
	ProxyURL           string
	AccountConcurrency int
	Priority           int
	BPSGroupIDs        []int64
	CodexGroupIDs      []int64
	SaveCredentials    bool
}

type batchLoginJobState struct {
	mu         sync.Mutex
	id         string
	status     string
	items      []BatchLoginItem
	cancel     context.CancelFunc
	finishedAt time.Time
}

func (j *batchLoginJobState) snapshot() *BatchLoginJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	return &BatchLoginJob{ID: j.id, Status: j.status, Items: append([]BatchLoginItem(nil), j.items...)}
}

func (j *batchLoginJobState) update(idx int, status, code string, accountID int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.items[idx].Status, j.items[idx].Code, j.items[idx].AccountID = status, code, accountID
}

// OpenAIBatchLoginService runs the vendored toSub2 login runtime for a batch of
// "email----password----2FA" rows and creates accounts from the returned tokens.
// Passwords and 2FA secrets are never persisted or logged.
type OpenAIBatchLoginService struct {
	cfg       config.BatchLoginConfig
	admin     AdminService
	proxyRepo ProxyRepository
	oauth     *OpenAIOAuthService
	run       batchLoginRunFunc
	reauth    *OpenAIReauthService

	mu   sync.Mutex
	jobs map[string]*batchLoginJobState
}

func NewOpenAIBatchLoginService(cfg *config.Config, admin AdminService, proxyRepo ProxyRepository, oauth *OpenAIOAuthService) *OpenAIBatchLoginService {
	bl := cfg.BatchLogin
	if bl.MaxRows <= 0 {
		bl.MaxRows = 100
	}
	if bl.Concurrency <= 0 {
		bl.Concurrency = 3
	}
	return &OpenAIBatchLoginService{cfg: bl, admin: admin, proxyRepo: proxyRepo, oauth: oauth,
		run: newBatchLoginExecRunner(bl), jobs: make(map[string]*batchLoginJobState)}
}

// SetReauth injects the credential store used when a batch asks to keep logins.
func (s *OpenAIBatchLoginService) SetReauth(r *OpenAIReauthService) { s.reauth = r }

func (s *OpenAIBatchLoginService) Start(ctx context.Context, in *BatchLoginInput) (*BatchLoginJob, error) {
	if err := ValidateBatchLoginInput(in, s.cfg.MaxRows); err != nil {
		return nil, err
	}
	if _, err := os.Stat(batchLoginScript(s.cfg)); err != nil {
		return nil, infraerrors.ServiceUnavailable("BATCH_LOGIN_RUNTIME_UNAVAILABLE", "batch login runtime is not installed")
	}
	settings := batchLoginSettings{ProxyID: in.ProxyID, AccountConcurrency: in.AccountConcurrency, Priority: in.Priority,
		BPSGroupIDs: in.BPSGroupIDs, CodexGroupIDs: in.CodexGroupIDs, SaveCredentials: in.SaveCredentials}
	if in.ProxyID != nil && s.proxyRepo != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *in.ProxyID)
		if err != nil || proxy == nil {
			return nil, infraerrors.BadRequest("BATCH_LOGIN_PROXY_NOT_FOUND", "proxy not found")
		}
		settings.ProxyURL = proxy.URL()
	}

	work := make([]batchLoginWork, 0, len(in.Entries)*len(in.Targets))
	items := make([]BatchLoginItem, 0, cap(work))
	for _, e := range in.Entries {
		for _, t := range in.Targets {
			work = append(work, batchLoginWork{entry: e, target: t})
			items = append(items, BatchLoginItem{Email: e.Email, Target: t, Status: BatchLoginItemPending})
		}
	}

	jobCtx, cancel := context.WithCancel(context.Background())
	job := &batchLoginJobState{id: newBatchLoginID(), status: BatchLoginStatusRunning, items: items, cancel: cancel}
	if err := s.register(job); err != nil {
		cancel()
		return nil, err
	}
	go s.execute(jobCtx, job, work, settings)
	return job.snapshot(), nil
}

func (s *OpenAIBatchLoginService) Get(id string) (*BatchLoginJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if job, ok := s.jobs[id]; ok {
		return job.snapshot(), true
	}
	return nil, false
}

func (s *OpenAIBatchLoginService) Cancel(id string) bool {
	s.mu.Lock()
	job, ok := s.jobs[id]
	s.mu.Unlock()
	if ok {
		job.cancel()
	}
	return ok
}

func (s *OpenAIBatchLoginService) register(job *batchLoginJobState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	active := 0
	for _, j := range s.jobs {
		j.mu.Lock()
		if j.status == BatchLoginStatusRunning {
			active++
		}
		j.mu.Unlock()
	}
	if active >= batchLoginMaxActive || len(s.jobs) >= batchLoginMaxJobs {
		return infraerrors.TooManyRequests("BATCH_LOGIN_BUSY", "too many batch login jobs, retry later")
	}
	s.jobs[job.id] = job
	return nil
}

func (s *OpenAIBatchLoginService) pruneLocked() {
	for id, j := range s.jobs {
		j.mu.Lock()
		expired := !j.finishedAt.IsZero() && time.Since(j.finishedAt) > batchLoginJobTTL
		j.mu.Unlock()
		if expired {
			delete(s.jobs, id)
		}
	}
}

func newBatchLoginID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *OpenAIBatchLoginService) execute(ctx context.Context, job *batchLoginJobState, work []batchLoginWork, settings batchLoginSettings) {
	defer func() {
		for i := range work { // drop secrets as soon as the job ends
			work[i].entry.Password, work[i].entry.TOTPSecret = "", ""
		}
	}()
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
dispatch:
	for idx := range work {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			s.processItem(ctx, job, i, work[i], settings)
		}(idx)
	}
	wg.Wait()

	job.mu.Lock()
	defer job.mu.Unlock()
	for i := range job.items {
		if job.items[i].Status == BatchLoginItemPending {
			job.items[i].Status = BatchLoginItemCanceled
		}
	}
	job.status = BatchLoginStatusDone
	if ctx.Err() != nil {
		job.status = BatchLoginStatusCanceled
	}
	job.finishedAt = time.Now()
	job.cancel()
}

func batchLoginTargetInfo(target string, st batchLoginSettings) (platform, provider string, groups []int64) {
	if target == BatchLoginTargetBPS {
		return PlatformOpenAIBPS, "bps_excel", st.BPSGroupIDs
	}
	return PlatformOpenAI, "codex", st.CodexGroupIDs
}

func (s *OpenAIBatchLoginService) processItem(ctx context.Context, job *batchLoginJobState, idx int, w batchLoginWork, st batchLoginSettings) {
	job.update(idx, BatchLoginItemRunning, "", 0)
	platform, provider, groups := batchLoginTargetInfo(w.target, st)
	exists, err := s.accountExists(ctx, platform, w.entry.Email)
	if err != nil {
		job.update(idx, BatchLoginItemFailed, "lookup_failed", 0)
		return
	}
	if exists {
		job.update(idx, BatchLoginItemSkipped, "already_exists", 0)
		return
	}
	rec, code := s.run(ctx, provider, w.entry, st.ProxyURL)
	if rec == nil {
		if code == "canceled" {
			job.update(idx, BatchLoginItemCanceled, "", 0)
		} else {
			job.update(idx, BatchLoginItemFailed, code, 0)
		}
		return
	}
	creds, code := s.buildCredentials(w.target, provider, w.entry.Email, rec)
	if creds == nil {
		job.update(idx, BatchLoginItemFailed, code, 0)
		return
	}
	// A finished login must not be lost because the job was canceled meanwhile.
	account, err := s.admin.CreateAccount(context.WithoutCancel(ctx), &CreateAccountInput{
		Name: w.entry.Email, Platform: platform, Type: AccountTypeOAuth, Credentials: creds,
		ProxyID: st.ProxyID, Concurrency: st.AccountConcurrency, Priority: st.Priority, GroupIDs: groups,
	})
	if err != nil || account == nil {
		slog.Warn("batch_login_create_failed", "target", w.target, "error", err)
		job.update(idx, BatchLoginItemFailed, "create_failed", 0)
		return
	}
	if st.SaveCredentials && s.reauth != nil {
		if err := s.reauth.SaveCredential(context.WithoutCancel(ctx), account.ID, w.target, w.entry); err != nil {
			slog.Warn("batch_login_save_credential_failed", "account_id", account.ID, "error", err)
		}
	}
	job.update(idx, BatchLoginItemCreated, "", account.ID)
}

// accountExists matches by account name (the search filter) and then by the
// stored credential email, because the search does not cover credentials.
func (s *OpenAIBatchLoginService) accountExists(ctx context.Context, platform, email string) (bool, error) {
	accounts, _, err := s.admin.ListAccounts(ctx, 1, 50, platform, AccountTypeOAuth, "", email, 0, "", "", "")
	if err != nil {
		return false, err
	}
	for i := range accounts {
		if strings.EqualFold(accounts[i].Name, email) || strings.EqualFold(accounts[i].GetCredential("email"), email) {
			return true, nil
		}
	}
	return false, nil
}

func (s *OpenAIBatchLoginService) buildCredentials(target, provider, email string, rec *batchLoginRecord) (map[string]any, string) {
	if rec.Provider != provider {
		return nil, "provider_mismatch"
	}
	if !strings.EqualFold(rec.Email, email) {
		return nil, "email_mismatch"
	}
	if rec.AccessToken == "" || rec.RefreshToken == "" || rec.WorkspaceID == "" {
		return nil, "incomplete_tokens"
	}
	expires, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil {
		return nil, "invalid_expiry"
	}
	if target == BatchLoginTargetBPS {
		if rec.ClientID != openai.ExcelClientID {
			return nil, "client_mismatch"
		}
		creds := map[string]any{"access_token": rec.AccessToken, "refresh_token": rec.RefreshToken,
			"expires_at": expires.UTC().Format(time.RFC3339), "chatgpt_account_id": rec.WorkspaceID,
			"client_id": rec.ClientID, "email": email}
		if rec.IDToken != "" {
			creds["id_token"] = rec.IDToken
		}
		return creds, ""
	}
	info := &OpenAITokenInfo{AccessToken: rec.AccessToken, RefreshToken: rec.RefreshToken, IDToken: rec.IDToken,
		ExpiresAt: expires.Unix(), Email: email, ChatGPTAccountID: rec.WorkspaceID, ClientID: rec.ClientID}
	if s.oauth == nil {
		return nil, "runtime_unavailable"
	}
	return s.oauth.BuildAccountCredentials(info), ""
}
