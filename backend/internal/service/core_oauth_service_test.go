package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

// This is the persistent database boundary, shared by independent core instances.
type coreOAuthTestStore struct {
	mu           sync.Mutex
	rows         map[string]service.CoreOAuthSealedProfile
	failCASAfter int
}

func (m *coreOAuthTestStore) ReadProfile(_ context.Context, ref string) (*service.CoreOAuthSealedProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.rows[ref]
	if !ok {
		return nil, service.ErrCoreOAuthProfileNotFound
	}
	return &v, nil
}
func (m *coreOAuthTestStore) ListProfiles(context.Context) ([]service.CoreOAuthSealedProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []service.CoreOAuthSealedProfile{}
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}
func (m *coreOAuthTestStore) CreateProfile(_ context.Context, r service.CoreOAuthSealedProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[r.ProfileRef]; ok {
		return service.ErrCoreOAuthConflict
	}
	m.rows[r.ProfileRef] = r
	return nil
}
func (m *coreOAuthTestStore) CompareAndSwapProfile(_ context.Context, ref string, old int64, r service.CoreOAuthSealedProfile) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCASAfter > 0 {
		m.failCASAfter--
		if m.failCASAfter == 0 {
			return false, service.ErrCoreOAuthStorage
		}
	}
	if m.rows[ref].Version != old {
		return false, nil
	}
	m.rows[ref] = r
	return true, nil
}
func coreOAuthTestService(t *testing.T, store *coreOAuthTestStore, protocols map[string]service.CoreOAuthProtocol) *service.CoreOAuthService {
	t.Helper()
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	enc, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	svc, err := service.NewCoreOAuthService(store, enc, protocols, nil)
	require.NoError(t, err)
	return svc
}

func TestCoreOAuthCreateIndependentProfiles(t *testing.T) {
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	svc := coreOAuthTestService(t, store, nil)
	profile, err := svc.CreateProfile(context.Background(), service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Excel workspace"})
	require.NoError(t, err)
	require.Equal(t, "empty", profile.Status)
	require.True(t, profile.NeedsAuthorization)
	require.False(t, profile.Authorized)
	raw, err := json.Marshal(profile)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "refresh_token")
	require.Len(t, store.rows, 1)
	restarted := coreOAuthTestService(t, store, nil)
	reread, err := restarted.GetProfile(context.Background(), profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, profile, reread)
	listed, err := restarted.ListProfiles(context.Background())
	require.NoError(t, err)
	require.Equal(t, []service.CoreOAuthProfile{*profile}, listed)
}

func TestCoreOAuthRejectsTrailingJSONInSealedProfile(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	svc := coreOAuthTestService(t, store, nil)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Excel workspace"})
	require.NoError(t, err)

	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	enc, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	row := store.rows[profile.ProfileRef]
	plaintext, err := enc.Decrypt(row.Ciphertext)
	require.NoError(t, err)
	row.Ciphertext, err = enc.Encrypt(plaintext + ` {"unexpected":true}`)
	require.NoError(t, err)
	store.rows[profile.ProfileRef] = row

	_, err = svc.GetProfile(ctx, profile.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
}

type coreOAuthTestProtocol struct {
	mu             sync.Mutex
	refreshes      int
	refreshErr     error
	exchangeErr    error
	exchanges      int
	refreshStarted chan struct{}
	refreshRelease chan struct{}
	grant          service.CoreOAuthGrant
}

func (p *coreOAuthTestProtocol) Begin(context.Context, service.CoreOAuthTransport) (*service.CoreOAuthProtocolAttempt, error) {
	return &service.CoreOAuthProtocolAttempt{AuthorizationURL: "https://auth.openai.com/api/accounts/authorize?state=synthetic-state", RedirectURI: "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback", ExpiresAt: time.Now().Add(10 * time.Minute), State: "synthetic"}, nil
}
func (p *coreOAuthTestProtocol) ExchangeCallback(context.Context, *service.CoreOAuthProtocolAttempt, string, service.CoreOAuthIdentity, service.CoreOAuthTransport) (*service.CoreOAuthGrant, error) {
	p.exchanges++
	if p.exchangeErr != nil {
		return nil, p.exchangeErr
	}
	v := p.grant
	return &v, nil
}

func TestCoreOAuthAuthorizationUnknownExchangeRequiresNewAttemptAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)
	ref := authorized.ProfileRef
	attempt, err := svc.StartAuthorization(ctx, ref, 42)
	require.NoError(t, err)
	require.True(t, attempt.Profile.Authorized)
	protocol.exchangeErr = context.DeadlineExceeded
	input := service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42}
	_, err = svc.CompleteAuthorization(ctx, ref, input)
	require.ErrorIs(t, err, service.ErrCoreOAuthAuthorizationRequired)

	restarted := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	view, err := restarted.GetProfile(ctx, ref)
	require.NoError(t, err)
	require.False(t, view.Authorized)
	require.True(t, view.NeedsAuthorization)
	require.Equal(t, "failed", view.Attempt.Status)
	_, err = restarted.CredentialSnapshot(ctx, ref)
	require.ErrorIs(t, err, service.ErrCoreOAuthAuthorizationRequired)
	_, err = restarted.CompleteAuthorization(ctx, ref, input)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.Equal(t, 2, protocol.exchanges)

	protocol.exchangeErr = nil
	replacement, err := restarted.StartAuthorization(ctx, ref, 42)
	require.NoError(t, err)
	input.AttemptID = replacement.AttemptID
	view, err = restarted.CompleteAuthorization(ctx, ref, input)
	require.NoError(t, err)
	require.True(t, view.Authorized)
	require.Equal(t, authorized.TokenRevision+1, view.TokenRevision)
}

func TestCoreOAuthExchangeCommitFailureFailsClosedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Excel"})
	require.NoError(t, err)
	attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)

	store.mu.Lock()
	store.failCASAfter = 2 // Persist exchanging, then fail the grant commit.
	store.mu.Unlock()
	_, err = svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{
		AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42,
	})
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
	require.Equal(t, 1, protocol.exchanges)

	restarted := coreOAuthTestService(t, store, protocols)
	view, err := restarted.GetProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.False(t, view.Authorized)
	require.Equal(t, "exchanging", view.Attempt.Status)
	_, err = restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthBusy)
	_, err = restarted.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{
		AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42,
	})
	require.ErrorIs(t, err, service.ErrCoreOAuthBusy)
	require.Equal(t, 1, protocol.exchanges, "persisted exchange fence must prevent reusing the callback")
}

func TestCoreOAuthAuthorizationPersistsOnlySafeRegionRejectionCode(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	svc, profile := authorizeCoreOAuthFixture(t, store, protocol)
	attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)
	protocol.exchangeErr = &service.CoreOAuthProtocolError{Permanent: true, SafeCode: "CORE_OAUTH_REGION_UNSUPPORTED"}
	_, err = svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{
		AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42,
	})
	require.ErrorIs(t, err, service.ErrCoreOAuthRegionUnsupported)

	restarted := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	view, err := restarted.GetProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, service.ErrCoreOAuthRegionUnsupported.Reason, view.LastErrorCode)
	require.Equal(t, "failed", view.Attempt.Status)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "synthetic-provider-secret")
	require.NotContains(t, string(raw), "unsupported_country_region_territory")
}

func TestCoreOAuthAuthorizationRestartExpiresPendingAttemptAndCancelIsActorBound(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Excel"})
	require.NoError(t, err)
	ref := profile.ProfileRef
	attempt, err := svc.StartAuthorization(ctx, ref, 42)
	require.NoError(t, err)
	restarted := coreOAuthTestService(t, store, protocols)
	_, err = restarted.CompleteAuthorization(ctx, ref, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42})
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptExpired)
	view, err := restarted.GetProfile(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "expired", view.Attempt.Status)
	_, err = restarted.CancelAuthorization(ctx, ref, attempt.AttemptID, 42)
	require.NoError(t, err)
	view, err = restarted.GetProfile(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, "canceled", view.Attempt.Status)

	attempt, err = restarted.StartAuthorization(ctx, ref, 42)
	require.NoError(t, err)
	_, err = restarted.CancelAuthorization(ctx, ref, attempt.AttemptID, 43)
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
	view, err = restarted.CancelAuthorization(ctx, ref, attempt.AttemptID, 42)
	require.NoError(t, err)
	require.Equal(t, "canceled", view.Attempt.Status)
	_, err = restarted.CompleteAuthorization(ctx, ref, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fixture", ActorID: 42})
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.Zero(t, protocol.exchanges)
}
func (p *coreOAuthTestProtocol) Refresh(ctx context.Context, old service.CoreOAuthGrant, _ service.CoreOAuthTransport) (*service.CoreOAuthGrant, error) {
	p.mu.Lock()
	p.refreshes++
	p.mu.Unlock()
	if p.refreshStarted != nil {
		close(p.refreshStarted)
	}
	if p.refreshRelease != nil {
		select {
		case <-p.refreshRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.refreshErr != nil {
		return nil, p.refreshErr
	}
	v := p.grant
	v.AccessToken = "synthetic-access-rotated"
	v.RefreshToken = "synthetic-refresh-rotated"
	return &v, nil
}
func coreOAuthProtocolFixture() *coreOAuthTestProtocol {
	return &coreOAuthTestProtocol{grant: service.CoreOAuthGrant{AccessToken: "synthetic-access-original", RefreshToken: "synthetic-refresh-original", IDToken: "synthetic-id-token", ClientID: "app_fnr0pYvVwwFDocDumLG3H2Bp", Subject: "user-fixture", WorkspaceID: "workspace-fixture", Email: "fixture@example.invalid", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
}

func TestCoreOAuthAuthorizationPersistsAndExposesOnlySafeState(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Excel"})
	require.NoError(t, err)
	attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 12)
	require.NoError(t, err)
	callback := attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code"
	_, err = svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: callback, ActorID: 13})
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
	authorized, err := svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: callback, ActorID: 12})
	require.NoError(t, err)
	require.Equal(t, "authorized", authorized.Status)
	require.Equal(t, int64(1), authorized.TokenRevision)
	require.True(t, authorized.Authorized)
	restarted := coreOAuthTestService(t, store, protocols)
	after, err := restarted.GetProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, authorized, after)
	snapshot, err := restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, "synthetic-access-original", snapshot.AccessToken)
	public, err := json.Marshal(snapshot)
	require.NoError(t, err)
	for _, secret := range []string{"synthetic-access-original", "synthetic-refresh-original", "synthetic-id-token", "synthetic-code"} {
		require.NotContains(t, string(public), secret)
	}
	_, err = svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, CallbackURL: callback, ActorID: 12})
	require.Error(t, err)
}
