package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

const (
	openAIReauthMaxFailures  = 3
	openAIReauthBaseCooldown = 15 * time.Minute
	openAIReauthMaxCooldown  = 24 * time.Hour
	openAIReauthMaxInflight  = 2
)

// OpenAIReauthService stores encrypted login credentials and uses them to
// re-authorize an account after its refresh token became permanently invalid.
// Secrets are decrypted only for the moment a login subprocess is started.
type OpenAIReauthService struct {
	repo        OpenAILoginCredentialRepository
	enc         SecretEncryptor
	admin       AdminService
	accountRepo AccountRepository
	batch       *OpenAIBatchLoginService

	mu       sync.Mutex
	inflight map[int64]struct{}
	sem      chan struct{}
}

func NewOpenAIReauthService(repo OpenAILoginCredentialRepository, enc SecretEncryptor, admin AdminService, accountRepo AccountRepository, batch *OpenAIBatchLoginService) *OpenAIReauthService {
	return &OpenAIReauthService{repo: repo, enc: enc, admin: admin, accountRepo: accountRepo, batch: batch,
		inflight: make(map[int64]struct{}), sem: make(chan struct{}, openAIReauthMaxInflight)}
}

// SaveCredential encrypts and stores the login credential of an account.
func (s *OpenAIReauthService) SaveCredential(ctx context.Context, accountID int64, target string, e BatchLoginEntry) error {
	if s == nil || s.repo == nil || s.enc == nil {
		return infraerrors.ServiceUnavailable("REAUTH_UNAVAILABLE", "credential storage is unavailable")
	}
	pw, err := s.enc.Encrypt(e.Password)
	if err != nil {
		return err
	}
	totp, err := s.enc.Encrypt(e.TOTPSecret)
	if err != nil {
		return err
	}
	return s.repo.Upsert(ctx, &OpenAILoginCredentialRecord{AccountID: accountID, LoginEmail: e.Email,
		Target: target, PasswordCiphertext: pw, TOTPCiphertext: totp})
}

func (s *OpenAIReauthService) HasCredential(ctx context.Context, accountID int64) (bool, error) {
	if s == nil || s.repo == nil {
		return false, nil
	}
	return s.repo.Exists(ctx, accountID)
}

func (s *OpenAIReauthService) DeleteCredential(ctx context.Context, accountID int64) error {
	if s == nil || s.repo == nil {
		return nil
	}
	return s.repo.Delete(ctx, accountID)
}

func openAIReauthCooldown(failCount int) time.Duration {
	d := openAIReauthBaseCooldown
	for i := 1; i < failCount; i++ {
		d *= 2
		if d >= openAIReauthMaxCooldown {
			return openAIReauthMaxCooldown
		}
	}
	return d
}

// OnRefreshPermanentFailure is the hook for a permanently failed refresh.
// It returns immediately; the login runs in the background.
func (s *OpenAIReauthService) OnRefreshPermanentFailure(account *Account) {
	if s == nil || account == nil || s.repo == nil || s.batch == nil {
		return
	}
	if account.Type != AccountTypeOAuth || (account.Platform != PlatformOpenAI && account.Platform != PlatformOpenAIBPS) {
		return
	}
	id := account.ID
	platform := account.Platform
	s.mu.Lock()
	if _, busy := s.inflight[id]; busy {
		s.mu.Unlock()
		return
	}
	s.inflight[id] = struct{}{}
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
		}()
		s.sem <- struct{}{}
		defer func() { <-s.sem }()
		s.reauthorize(context.Background(), id, platform)
	}()
}

func (s *OpenAIReauthService) reauthorize(ctx context.Context, accountID int64, platform string) {
	rec, err := s.repo.Get(ctx, accountID)
	if err != nil || rec == nil {
		return
	}
	if rec.FailCount >= openAIReauthMaxFailures {
		return
	}
	if rec.NextAttemptAt != nil && time.Now().Before(*rec.NextAttemptAt) {
		return
	}
	fail := func(code string) {
		next := time.Now().Add(openAIReauthCooldown(rec.FailCount + 1))
		_ = s.repo.RecordAttempt(ctx, accountID, false, code, &next)
		slog.Warn("openai_reauth_failed", "account_id", accountID, "code", code, "fail_count", rec.FailCount+1)
	}
	password, err := s.enc.Decrypt(rec.PasswordCiphertext)
	if err != nil {
		fail("decrypt_failed")
		return
	}
	totp, err := s.enc.Decrypt(rec.TOTPCiphertext)
	if err != nil {
		fail("decrypt_failed")
		return
	}
	target, provider := BatchLoginTargetCodex, "codex"
	if platform == PlatformOpenAIBPS {
		target, provider = BatchLoginTargetBPS, "bps_excel"
	}
	account, err := s.admin.GetAccount(ctx, accountID)
	if err != nil || account == nil {
		fail("account_lookup_failed")
		return
	}
	if account.ID != accountID || account.Platform != platform || account.Type != AccountTypeOAuth {
		fail("account_changed")
		return
	}
	ctx, release, routeErr := beginOpenAIRequestProxyWithRepository(ctx, nil, account, s.batch.proxyRepo)
	if routeErr != nil {
		fail("request_proxy_unavailable")
		return
	}
	defer release()
	// The vendored runner is local and accepts exactly one explicit proxy URL
	// for the complete login; no remote worker can replace that egress.
	proxyURL, routeErr := resolveOpenAIMaintenanceProxy(ctx, account, s.batch.proxyRepo)
	if routeErr != nil {
		fail("request_proxy_unavailable")
		return
	}
	if !account.OpenAIRequestProxyEnabled() && account.ProxyID != nil && s.batch.proxyRepo != nil {
		if p, perr := s.batch.proxyRepo.GetByID(ctx, *account.ProxyID); perr == nil && p != nil {
			proxyURL = p.URL()
		}
	}
	if account.OpenAIRequestProxyEnabled() {
		// Recheck the saved group after acquisition and before handing passwords
		// or TOTP secrets to the subprocess. A mode change never selects single.
		latest, lookupErr := s.admin.GetAccount(ctx, accountID)
		if lookupErr != nil || latest == nil || latest.ID != accountID || latest.Platform != account.Platform || latest.Type != account.Type {
			fail("account_changed")
			return
		}
		if !sameOpenAIOAuthProxyGroup(account.OpenAIRequestProxyIDs(), latest.OpenAIRequestProxyIDs()) {
			fail("request_proxy_changed")
			return
		}
		proxyURL, routeErr = resolveOpenAIMaintenanceProxy(ctx, latest, s.batch.proxyRepo)
		if routeErr != nil {
			fail("request_proxy_unavailable")
			return
		}
	}
	entry := BatchLoginEntry{Email: rec.LoginEmail, Password: password, TOTPSecret: totp}
	tokens, code := s.batch.run(ctx, provider, entry, proxyURL, nil)
	entry.Password, entry.TOTPSecret = "", ""
	if tokens == nil {
		fail(code)
		return
	}
	creds, code := s.batch.buildCredentials(target, provider, rec.LoginEmail, tokens)
	if creds == nil {
		fail(code)
		return
	}
	// Keep the operator-selected workspace of a BPS account across re-authorization.
	if existing := account.GetCredential("chatgpt_account_id"); existing != "" && platform == PlatformOpenAIBPS {
		creds["chatgpt_account_id"] = existing
	}
	if _, err := s.admin.UpdateAccount(ctx, accountID, &UpdateAccountInput{Credentials: creds}); err != nil {
		fail("update_failed")
		return
	}
	if s.accountRepo != nil {
		if err := s.accountRepo.ClearError(ctx, accountID); err != nil {
			slog.Warn("openai_reauth_clear_error_failed", "account_id", accountID, "error", err)
		}
	}
	_ = s.repo.RecordAttempt(ctx, accountID, true, "", nil)
	slog.Info("openai_reauth_succeeded", "account_id", accountID, "target", target)
}
