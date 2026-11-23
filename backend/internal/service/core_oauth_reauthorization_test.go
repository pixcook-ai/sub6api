package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

type coreOAuthReauthorizationProtocol struct {
	*coreOAuthTestProtocol
	provider string
}

func (p *coreOAuthReauthorizationProtocol) Begin(ctx context.Context, transport service.CoreOAuthTransport) (*service.CoreOAuthProtocolAttempt, error) {
	attempt, err := p.coreOAuthTestProtocol.Begin(ctx, transport)
	if p.provider == service.CoreOAuthProviderCodex {
		attempt.AuthorizationURL = "https://auth.openai.com/oauth/authorize?state=synthetic-state"
		attempt.RedirectURI = openai.DefaultRedirectURI
	}
	return attempt, err
}

func TestCoreOAuthReauthorizationPreservesIdentityEpochAndMonotonicRevision(t *testing.T) {
	for _, provider := range []string{service.CoreOAuthProviderExcel, service.CoreOAuthProviderCodex} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
			protocol := &coreOAuthReauthorizationProtocol{coreOAuthTestProtocol: coreOAuthProtocolFixture(), provider: provider}
			if provider == service.CoreOAuthProviderCodex {
				protocol.grant.ClientID = openai.ClientID
			}
			protocols := map[string]service.CoreOAuthProtocol{provider: protocol}
			svc := coreOAuthTestService(t, store, protocols)
			profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: provider, Name: "Reauthorization fixture"})
			require.NoError(t, err)
			complete := func() *service.CoreOAuthProfile {
				attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 42)
				require.NoError(t, err)
				value, err := svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code"})
				require.NoError(t, err)
				return value
			}
			original := complete()
			before, err := svc.CredentialSnapshot(ctx, profile.ProfileRef)
			require.NoError(t, err)
			bound, err := service.CoreOAuthAccountBindingForSnapshot(before)
			require.NoError(t, err)
			protocol.grant.AccessToken = "synthetic-access-reauthorized"
			protocol.grant.RefreshToken = "synthetic-refresh-reauthorized"
			reauthorized := complete()
			require.Equal(t, original.IdentityEpoch, reauthorized.IdentityEpoch, "same identity must keep existing immutable deployment/session bindings usable")
			require.Equal(t, original.TokenRevision+1, reauthorized.TokenRevision)
			restarted := coreOAuthTestService(t, store, protocols)
			after, err := restarted.CredentialSnapshot(ctx, profile.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, protocol.grant.AccessToken, after.AccessToken)
			rebound, err := service.CoreOAuthAccountBindingForSnapshot(after)
			require.NoError(t, err)
			require.Equal(t, bound.IdentityEpoch, rebound.IdentityEpoch)
			require.Equal(t, bound.Subject, rebound.Subject)
			require.Equal(t, bound.WorkspaceID, rebound.WorkspaceID)
			require.Equal(t, bound.Provider, rebound.Provider)
		})
	}
}

func TestCoreOAuthReauthorizationCannotReplaceBoundIdentity(t *testing.T) {
	for _, identity := range []string{"subject", "workspace", "client"} {
		t.Run(identity, func(t *testing.T) {
			ctx := context.Background()
			store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
			protocol := coreOAuthProtocolFixture()
			svc, original := authorizeCoreOAuthFixture(t, store, protocol)
			switch identity {
			case "subject":
				protocol.grant.Subject = "another-subject"
			case "workspace":
				protocol.grant.WorkspaceID = "another-workspace"
			case "client":
				protocol.grant.ClientID = openai.ClientID
			}
			attempt, err := svc.StartAuthorization(ctx, original.ProfileRef, 42)
			require.NoError(t, err)
			_, err = svc.CompleteAuthorization(ctx, original.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code"})
			require.Error(t, err)
			current, err := svc.GetProfile(ctx, original.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, original.Subject, current.Subject)
			require.Equal(t, original.WorkspaceID, current.WorkspaceID)
			require.Equal(t, original.ClientID, current.ClientID)
			require.Equal(t, original.IdentityEpoch, current.IdentityEpoch)
			require.Equal(t, original.TokenRevision, current.TokenRevision)
		})
	}
}

type coreOAuthStateTokenClient struct {
	calls int
}

func (c *coreOAuthStateTokenClient) ExchangeCode(context.Context, string, string, string, string, string) (*openai.TokenResponse, error) {
	c.calls++
	claims, _ := json.Marshal(map[string]any{"sub": "synthetic-subject", "email": "fixture@example.invalid", "exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "synthetic-workspace"}})
	token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	return &openai.TokenResponse{AccessToken: token, RefreshToken: "synthetic-refresh-original", IDToken: token, ExpiresIn: 3600}, nil
}
func (*coreOAuthStateTokenClient) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	panic("unexpected refresh")
}
func (*coreOAuthStateTokenClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	panic("unexpected refresh")
}

func TestCoreOAuthCodexWrongStatePreservesExistingGrant(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	client := &coreOAuthStateTokenClient{}
	native := service.NewOpenAIOAuthService(nil, client)
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderCodex: service.NewCoreOAuthCodexProtocol(native)}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderCodex, Name: "Codex state fixture"})
	require.NoError(t, err)
	attempt, err := svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)
	authorize, err := url.Parse(attempt.AuthorizationURL)
	require.NoError(t, err)
	original, err := svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=synthetic-code&state=" + url.QueryEscape(authorize.Query().Get("state"))})
	require.NoError(t, err)
	before, err := svc.CredentialSnapshot(ctx, profile.ProfileRef)
	require.NoError(t, err)
	attempt, err = svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)
	require.True(t, attempt.Profile.Authorized)
	_, err = svc.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=synthetic-code&state=wrong-state"})
	require.Equal(t, 1, client.calls, "invalid state must never reach the external token endpoint")
	require.ErrorIs(t, err, service.ErrCoreOAuthProviderRejected)
	restarted := coreOAuthTestService(t, store, protocols)
	after, err := restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, before.AccessToken, after.AccessToken)
	require.Equal(t, original.TokenRevision, after.Profile.TokenRevision)
	require.Equal(t, original.IdentityEpoch, after.Profile.IdentityEpoch)
	require.True(t, after.Profile.Authorized)
	require.False(t, after.Profile.NeedsAuthorization)
}

func TestCoreOAuthAuthorizationStateWithoutLocalPKCEPreservesGrantAndOwnerAttempt(t *testing.T) {
	for _, read := range []string{"get", "list"} {
		t.Run(read, func(t *testing.T) {
			ctx := context.Background()
			store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
			protocol := coreOAuthProtocolFixture()
			owner, authorized := authorizeCoreOAuthFixture(t, store, protocol)
			attempt, err := owner.StartAuthorization(ctx, authorized.ProfileRef, 42)
			require.NoError(t, err)
			require.True(t, attempt.Profile.Authorized)
			other := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
			var observed *service.CoreOAuthProfile
			if read == "get" {
				observed, err = other.GetProfile(ctx, authorized.ProfileRef)
			} else {
				var profiles []service.CoreOAuthProfile
				profiles, err = other.ListProfiles(ctx)
				require.NoError(t, err)
				require.Len(t, profiles, 1)
				observed = &profiles[0]
			}
			require.NoError(t, err)
			require.NotNil(t, observed.Attempt)
			require.Equal(t, "expired", observed.Attempt.Status, "an instance without the process-local PKCE cannot offer this attempt as completable")
			require.Equal(t, "authorized", observed.Status)
			require.True(t, observed.Authorized)
			require.False(t, observed.NeedsAuthorization)
			snapshot, err := other.CredentialSnapshot(ctx, authorized.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, "synthetic-access-original", snapshot.AccessToken)
			require.Equal(t, authorized.IdentityEpoch, snapshot.Profile.IdentityEpoch)
			current, err := owner.GetProfile(ctx, authorized.ProfileRef)
			require.NoError(t, err)
			require.Equal(t, "pending", current.Attempt.Status, "a read on another instance must not expire the owner's persisted transaction")
			_, err = owner.CompleteAuthorization(ctx, authorized.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 43, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code"})
			require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
			completed, err := owner.CompleteAuthorization(ctx, authorized.ProfileRef, service.CoreOAuthCompleteInput{ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?state=synthetic-state&code=synthetic-code"})
			require.NoError(t, err)
			require.True(t, completed.Authorized)
			require.Equal(t, authorized.TokenRevision+1, completed.TokenRevision)
		})
	}
}
