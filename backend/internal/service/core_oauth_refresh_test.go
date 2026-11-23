package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func authorizeCoreOAuthFixture(t *testing.T, store *coreOAuthTestStore, protocol service.CoreOAuthProtocol) (*service.CoreOAuthService, *service.CoreOAuthProfile) {
	t.Helper()
	ctx := context.Background()
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "Refresh fixture"})
	require.NoError(t, err)
	attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)
	authorized, err := svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{
		AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code", ActorID: 42,
	})
	require.NoError(t, err)
	return svc, authorized
}

type coreOAuthRefreshObserver struct {
	*coreOAuthTestProtocol
	onRefresh func(service.CoreOAuthGrant)
}

func (p *coreOAuthRefreshObserver) Refresh(ctx context.Context, previous service.CoreOAuthGrant, transport service.CoreOAuthTransport) (*service.CoreOAuthGrant, error) {
	grant, err := p.coreOAuthTestProtocol.Refresh(ctx, previous, transport)
	p.onRefresh(previous)
	return grant, err
}

type coreOAuthCancellationAwareStore struct{ *coreOAuthTestStore }

func (s coreOAuthCancellationAwareStore) CompareAndSwapProfile(ctx context.Context, ref string, version int64, row service.CoreOAuthSealedProfile) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.coreOAuthTestStore.CompareAndSwapProfile(ctx, ref, version, row)
}

func TestCoreOAuthRefreshCommitsKnownRotationAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	var usedTokens []string
	protocol := &coreOAuthRefreshObserver{
		coreOAuthTestProtocol: coreOAuthProtocolFixture(),
		onRefresh: func(previous service.CoreOAuthGrant) {
			usedTokens = append(usedTokens, previous.RefreshToken)
			cancel()
		},
	}
	_, authorized := authorizeCoreOAuthFixture(t, store, protocol)
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	encryptor, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc, err := service.NewCoreOAuthService(coreOAuthCancellationAwareStore{store}, encryptor, protocols, nil)
	require.NoError(t, err)
	refreshed, err := svc.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)

	restarted := coreOAuthTestService(t, store, protocols)
	_, err = restarted.Refresh(context.Background(), refreshed.ProfileRef, refreshed.TokenRevision)
	require.NoError(t, err)
	require.Equal(t, []string{"synthetic-refresh-original", "synthetic-refresh-rotated"}, usedTokens)
}

func TestCoreOAuthRefreshPersistsRotatedGrantBeforeSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
		protocol := coreOAuthProtocolFixture()
		svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)
		time.Sleep(time.Hour)
		protocol.grant.ExpiresAt = time.Now().Add(time.Hour)

		snapshot, err := svc.CredentialSnapshot(ctx, authorized.ProfileRef)
		require.NoError(t, err)
		require.Equal(t, "synthetic-access-rotated", snapshot.AccessToken)
		require.Equal(t, authorized.TokenRevision+1, snapshot.Profile.TokenRevision)
		require.Equal(t, authorized.IdentityEpoch, snapshot.Profile.IdentityEpoch)
		require.True(t, snapshot.Profile.Authorized)

		restarted := coreOAuthTestService(t, store, nil)
		reread, err := restarted.CredentialSnapshot(ctx, authorized.ProfileRef)
		require.NoError(t, err)
		require.Equal(t, snapshot, reread)
		encoded, err := json.Marshal(snapshot)
		require.NoError(t, err)
		for _, secret := range []string{snapshot.AccessToken, "synthetic-refresh-rotated", "synthetic-id-token"} {
			require.NotContains(t, string(encoded), secret)
		}
	})
}

func TestCoreOAuthRefreshCASAllowsOneProviderCallAcrossInstances(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocol.refreshStarted = make(chan struct{})
	protocol.refreshRelease = make(chan struct{})
	svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)
	second := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})

	result := make(chan error, 1)
	go func() {
		_, err := svc.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
		result <- err
	}()
	release := sync.OnceFunc(func() { close(protocol.refreshRelease) })
	defer release()
	select {
	case <-protocol.refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach provider")
	}
	_, err := second.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
	require.ErrorIs(t, err, service.ErrCoreOAuthBusy)
	_, err = second.CredentialSnapshot(ctx, authorized.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthBusy)
	release()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not complete")
	}
	protocol.mu.Lock()
	defer protocol.mu.Unlock()
	require.Equal(t, 1, protocol.refreshes)
}

func TestCoreOAuthRefreshUnknownOutcomeFailsClosedAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocol.refreshStarted = make(chan struct{})
	protocol.refreshRelease = make(chan struct{})
	svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)

	result := make(chan error, 1)
	go func() {
		_, err := svc.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
		result <- err
	}()
	select {
	case <-protocol.refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not reach provider")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, service.ErrCoreOAuthRefreshUncertain)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish after cancellation")
	}
	ctx = context.Background()
	restarted := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	_, err := restarted.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
	require.ErrorIs(t, err, service.ErrCoreOAuthRefreshUncertain)
	_, err = restarted.CredentialSnapshot(ctx, authorized.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthRefreshUncertain)
	protocol.mu.Lock()
	refreshes := protocol.refreshes
	protocol.mu.Unlock()
	require.Equal(t, 1, refreshes)
	profile, err := restarted.GetProfile(ctx, authorized.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, "refresh_uncertain", profile.Status)
}

func TestCoreOAuthRecoverUnauthorizedOnlyRefreshesCurrentUpstream401(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)
	input := service.CoreOAuthUnauthorizedInput{ProfileRef: authorized.ProfileRef, IdentityEpoch: authorized.IdentityEpoch, FailedRevision: authorized.TokenRevision, StatusCode: 403, Origin: "upstream_oauth"}
	profile, err := svc.RecoverUnauthorized(ctx, input)
	require.NoError(t, err)
	require.Equal(t, authorized.TokenRevision, profile.TokenRevision)
	input.StatusCode = 429
	_, err = svc.RecoverUnauthorized(ctx, input)
	require.NoError(t, err)
	input.StatusCode = 401
	input.Origin = "entry"
	_, err = svc.RecoverUnauthorized(ctx, input)
	require.NoError(t, err)
	protocol.mu.Lock()
	require.Zero(t, protocol.refreshes)
	protocol.mu.Unlock()
	input.Origin = "upstream_oauth"
	profile, err = svc.RecoverUnauthorized(ctx, input)
	require.NoError(t, err)
	require.Equal(t, authorized.TokenRevision+1, profile.TokenRevision)
	profile, err = svc.RecoverUnauthorized(ctx, input)
	require.NoError(t, err)
	require.Equal(t, authorized.TokenRevision+1, profile.TokenRevision)
}

func TestCoreOAuthRefreshInvalidGrantRequiresOriginalAuthorization(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocol.refreshErr = &service.CoreOAuthProtocolError{Permanent: true}
	svc, authorized := authorizeCoreOAuthFixture(t, store, protocol)

	_, err := svc.Refresh(ctx, authorized.ProfileRef, authorized.TokenRevision)
	require.ErrorIs(t, err, service.ErrCoreOAuthAuthorizationRequired)
	restarted := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	_, err = restarted.CredentialSnapshot(ctx, authorized.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthAuthorizationRequired)
	profile, err := restarted.GetProfile(ctx, authorized.ProfileRef)
	require.NoError(t, err)
	require.True(t, profile.NeedsAuthorization)
}
