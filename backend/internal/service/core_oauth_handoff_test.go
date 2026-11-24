package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthHandoffPersistsClosedReceiverReceipt(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	svc := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "handoff target"})
	require.NoError(t, err)
	input := service.CoreOAuthHandoffInput{
		OperationID: "11111111-1111-4111-8111-111111111111", Provider: service.CoreOAuthProviderExcel,
		ProfileRef: profile.ProfileRef, SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Subject: "source-subject", WorkspaceID: "source-workspace",
		IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute), ProtectedPayload: strings.Repeat("x", 128), ActorID: 42,
	}

	receipt, err := svc.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	require.Equal(t, "pending", receipt.Status)
	require.False(t, receipt.TransferSupported)
	require.Equal(t, input.OperationID, receipt.OperationID)

	retry, err := svc.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	wrongPayload := input
	wrongPayload.ProtectedPayload += "changed"
	_, err = svc.BeginHandoff(ctx, profile.ProfileRef, wrongPayload)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	_, err = svc.GetHandoff(ctx, profile.ProfileRef, input.OperationID, 99)
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)

	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), input.ProtectedPayload)
	require.NotContains(t, string(encoded), "refresh_token")

	restarted := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	persisted, err := restarted.GetHandoff(ctx, profile.ProfileRef, input.OperationID, input.ActorID)
	require.NoError(t, err)
	require.Equal(t, receipt, persisted)
	_, err = restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	_, err = restarted.Refresh(ctx, profile.ProfileRef, 0)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	_, err = restarted.StartAuthorization(ctx, profile.ProfileRef, input.ActorID)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	view, err := restarted.GetProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.False(t, view.Authorized)
	require.Equal(t, "handoff_pending", view.Status)
}

func TestCoreOAuthHandoffCASFailureDoesNotAccept(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	svc := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: coreOAuthProtocolFixture()})
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "handoff target"})
	require.NoError(t, err)
	store.mu.Lock()
	store.failCASAfter = 1
	store.mu.Unlock()
	_, err = svc.BeginHandoff(ctx, profile.ProfileRef, service.CoreOAuthHandoffInput{
		OperationID: "11111111-1111-4111-8111-111111111111", Provider: service.CoreOAuthProviderExcel,
		ProfileRef: profile.ProfileRef, SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Subject: "source-subject", WorkspaceID: "source-workspace",
		IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute), ProtectedPayload: strings.Repeat("x", 128), ActorID: 42,
	})
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
	_, err = svc.GetHandoff(ctx, profile.ProfileRef, "11111111-1111-4111-8111-111111111111", 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	view, err := svc.GetProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.False(t, view.Authorized)
	require.NotEqual(t, "handoff_pending", view.Status)
}

func TestCoreOAuthHandoffAbandonmentPersistsAndAllowsFreshAuthorization(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	protocols := map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol}
	svc := coreOAuthTestService(t, store, protocols)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "handoff target"})
	require.NoError(t, err)
	input := service.CoreOAuthHandoffInput{
		OperationID: "11111111-1111-4111-8111-111111111111", Provider: service.CoreOAuthProviderExcel,
		ProfileRef: profile.ProfileRef, SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Subject: "source-subject", WorkspaceID: "source-workspace",
		IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute), ProtectedPayload: strings.Repeat("x", 128), ActorID: 42,
	}
	_, err = svc.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	_, err = svc.AbandonHandoff(ctx, profile.ProfileRef, input.OperationID, 99)
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
	_, err = svc.AbandonHandoff(ctx, profile.ProfileRef, "44444444-4444-4444-8444-444444444444", 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	store.failCASAfter = 1
	_, err = svc.AbandonHandoff(ctx, profile.ProfileRef, input.OperationID, 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
	_, err = svc.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	receipt, err := svc.AbandonHandoff(ctx, profile.ProfileRef, input.OperationID, 42)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	enc, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	plaintext, err := enc.Decrypt(store.rows[profile.ProfileRef].Ciphertext)
	require.NoError(t, err)
	require.NotContains(t, plaintext, input.ProtectedPayload)
	require.Equal(t, "abandoned", receipt.Status)
	require.False(t, receipt.TransferSupported)
	restarted := coreOAuthTestService(t, store, protocols)
	retry, err := restarted.AbandonHandoff(ctx, profile.ProfileRef, input.OperationID, 42)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	replay, err := restarted.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	require.Equal(t, "abandoned", replay.Status)
	_, err = restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthAuthorizationRequired)
	attempt, err := restarted.StartAuthorization(ctx, profile.ProfileRef, 42)
	require.NoError(t, err)
	authorized, err := restarted.CompleteAuthorization(ctx, profile.ProfileRef, service.CoreOAuthCompleteInput{
		ActorID: 42, AttemptID: attempt.AttemptID, CallbackURL: attempt.RedirectURI + "?code=fresh-fixture",
	})
	require.NoError(t, err)
	require.True(t, authorized.Authorized)
	require.Equal(t, int64(1), authorized.TokenRevision)
	require.NotEqual(t, input.IdentityEpoch, authorized.IdentityEpoch)
	require.Equal(t, protocol.grant.Subject, authorized.Subject)
	restarted = coreOAuthTestService(t, store, protocols)
	snapshot, err := restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, authorized.IdentityEpoch, snapshot.Profile.IdentityEpoch)
	retry, err = restarted.GetHandoff(ctx, profile.ProfileRef, input.OperationID, 42)
	require.NoError(t, err)
	require.Equal(t, "abandoned", retry.Status)
}

func TestCoreOAuthHandoffExpiryRemainsLockedAndCanBeAbandoned(t *testing.T) {
	ctx := context.Background()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	svc := coreOAuthTestService(t, store, nil)
	profile, err := svc.CreateProfile(ctx, service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "expiry"})
	require.NoError(t, err)
	input := service.CoreOAuthHandoffInput{
		OperationID: "11111111-1111-4111-8111-111111111111", Provider: service.CoreOAuthProviderExcel,
		ProfileRef: profile.ProfileRef, SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Subject: "subject", WorkspaceID: "workspace",
		IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
		ExpiresAt: time.Now().UTC().Add(time.Minute), ProtectedPayload: strings.Repeat("x", 128), ActorID: 42,
	}
	_, err = svc.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	enc, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	row := store.rows[profile.ProfileRef]
	plaintext, err := enc.Decrypt(row.Ciphertext)
	require.NoError(t, err)
	var sealed map[string]any
	require.NoError(t, json.Unmarshal([]byte(plaintext), &sealed))
	input.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	sealed["Handoff"].(map[string]any)["ExpiresAt"] = input.ExpiresAt.Format(time.RFC3339Nano)
	encoded, err := json.Marshal(sealed)
	require.NoError(t, err)
	row.Ciphertext, err = enc.Encrypt(string(encoded))
	require.NoError(t, err)
	store.rows[profile.ProfileRef] = row
	restarted := coreOAuthTestService(t, store, nil)
	_, err = restarted.CredentialSnapshot(ctx, profile.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffExpired)
	receipt, err := restarted.BeginHandoff(ctx, profile.ProfileRef, input)
	require.NoError(t, err)
	require.Equal(t, "pending", receipt.Status)
	receipt, err = restarted.AbandonHandoff(ctx, profile.ProfileRef, input.OperationID, 42)
	require.NoError(t, err)
	require.Equal(t, "abandoned", receipt.Status)
}
