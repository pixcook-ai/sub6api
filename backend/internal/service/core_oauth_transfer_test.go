package service_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
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

type coreTransferFixture struct {
	store      *coreOAuthTestStore
	core       *service.CoreOAuthService
	protocol   *coreOAuthTestProtocol
	in         service.CoreOAuthHandoffInvitationInput
	invitation *service.CoreOAuthHandoffInvitation
}

func newCoreTransferFixture(t *testing.T) *coreTransferFixture {
	t.Helper()
	store := &coreOAuthTestStore{rows: map[string]service.CoreOAuthSealedProfile{}}
	protocol := coreOAuthProtocolFixture()
	core := coreOAuthTestService(t, store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: protocol})
	p, err := core.CreateProfile(context.Background(), service.CoreOAuthCreateInput{Provider: service.CoreOAuthProviderExcel, Name: "transfer target"})
	require.NoError(t, err)
	in := service.CoreOAuthHandoffInvitationInput{OperationID: "11111111-1111-4111-8111-111111111111", ProfileRef: p.ProfileRef,
		SourceProfileRef: "oauth_33333333-3333-4333-8333-333333333333", Provider: service.CoreOAuthProviderExcel, Subject: protocol.grant.Subject,
		WorkspaceID: protocol.grant.WorkspaceID, IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 7,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute), ActorID: 42}
	invitation, err := core.CreateHandoffInvitation(context.Background(), p.ProfileRef, in)
	require.NoError(t, err)
	return &coreTransferFixture{store, core, protocol, in, invitation}
}

func (f *coreTransferFixture) restart(t *testing.T) *service.CoreOAuthService {
	t.Helper()
	return coreOAuthTestService(t, f.store, map[string]service.CoreOAuthProtocol{service.CoreOAuthProviderExcel: f.protocol})
}

func (f *coreTransferFixture) plaintext() map[string]any {
	g := f.protocol.grant
	return map[string]any{"contract": service.CoreOAuthHandoffTransferContract, "operation_id": f.in.OperationID,
		"profile_ref": f.in.ProfileRef, "source_profile_ref": f.in.SourceProfileRef, "provider": f.in.Provider, "subject": f.in.Subject,
		"workspace_id": f.in.WorkspaceID, "identity_epoch": f.in.IdentityEpoch, "token_revision": f.in.TokenRevision, "source_quiesced": true,
		"publication_fences": []any{map[string]any{"account_id": 17, "fenced": true}},
		"grant": map[string]any{"access_token": g.AccessToken, "refresh_token": g.RefreshToken, "id_token": g.IDToken, "client_id": g.ClientID,
			"subject": g.Subject, "workspace_id": g.WorkspaceID, "email": g.Email, "expires_at": g.ExpiresAt.Format(time.RFC3339Nano)}}
}

func coreSealTransfer(t *testing.T, invitation *service.CoreOAuthHandoffInvitation, payload any, raw ...[]byte) service.CoreOAuthHandoffEnvelope {
	t.Helper()
	var data []byte
	var err error
	if len(raw) > 0 {
		data = raw[0]
	} else {
		data, err = json.Marshal(payload)
		require.NoError(t, err)
	}
	der, err := base64.RawURLEncoding.DecodeString(invitation.PublicKey)
	require.NoError(t, err)
	key, err := x509.ParsePKIXPublicKey(der)
	require.NoError(t, err)
	symmetric := make([]byte, 32)
	_, err = rand.Read(symmetric)
	require.NoError(t, err)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key.(*rsa.PublicKey), symmetric, nil)
	require.NoError(t, err)
	block, err := aes.NewCipher(symmetric)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, 12)
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	aad := []byte(service.CoreOAuthHandoffTransferContract + "\n" + invitation.ProfileRef + "\n" + invitation.OperationID)
	sealed := gcm.Seal(nil, nonce, data, aad)
	return service.CoreOAuthHandoffEnvelope{OperationID: invitation.OperationID, WrappedKey: base64.RawURLEncoding.EncodeToString(wrapped),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce), Ciphertext: base64.RawURLEncoding.EncodeToString(sealed)}
}

func coreTransferAcceptingVerifier(context.Context, service.CoreOAuthHandoffVerification) error {
	return nil
}

func TestCoreOAuthTransferInvitationPersistsBarrierAndActorBinding(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	require.Equal(t, service.CoreOAuthHandoffTransferContract, f.invitation.Contract)
	core := f.restart(t)
	retry, err := core.CreateHandoffInvitation(ctx, f.in.ProfileRef, f.in)
	require.NoError(t, err)
	require.Equal(t, f.invitation, retry)
	wrong := f.in
	wrong.ActorID = 99
	_, err = core.CreateHandoffInvitation(ctx, f.in.ProfileRef, wrong)
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
	wrong = f.in
	wrong.TokenRevision++
	_, err = core.CreateHandoffInvitation(ctx, f.in.ProfileRef, wrong)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	receipt, err := core.GetHandoff(ctx, f.in.ProfileRef, f.in.OperationID, 42)
	require.NoError(t, err)
	require.Equal(t, "pending", receipt.Status)
	require.False(t, receipt.TransferSupported)
	_, err = core.StartAuthorization(ctx, f.in.ProfileRef, 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	_, err = core.Refresh(ctx, f.in.ProfileRef, 0)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	_, err = core.CredentialSnapshot(ctx, f.in.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	_, err = core.RecoverUnauthorized(ctx, service.CoreOAuthUnauthorizedInput{ProfileRef: f.in.ProfileRef})
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	view, err := core.GetProfile(ctx, f.in.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, "handoff_pending", view.Status)
	require.False(t, view.Authorized)
}

func TestCoreOAuthTransferCommitsBeforeSnapshotAndReconcilesLostResponse(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	envelope := coreSealTransfer(t, f.invitation, f.plaintext())
	verifications := 0
	verifier := func(_ context.Context, in service.CoreOAuthHandoffVerification) error {
		verifications++
		require.Equal(t, *f.invitation, in.Invitation)
		require.True(t, in.SourceQuiesced)
		require.Len(t, in.PublicationFences, 1)
		_, err := f.core.CredentialSnapshot(ctx, f.in.ProfileRef)
		require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
		return nil
	}
	receipt, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, verifier)
	require.NoError(t, err)
	require.Equal(t, "accepted", receipt.Status)
	require.True(t, receipt.TransferSupported)
	require.Equal(t, int64(7), receipt.TokenRevision)
	core := f.restart(t)
	retry, err := core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, verifier)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	require.Equal(t, 1, verifications)
	snapshot, err := core.CredentialSnapshot(ctx, f.in.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, f.protocol.grant.AccessToken, snapshot.AccessToken)
	require.Equal(t, f.in.IdentityEpoch, snapshot.Profile.IdentityEpoch)
	require.Equal(t, int64(7), snapshot.Profile.TokenRevision)
	wrong := coreSealTransfer(t, f.invitation, f.plaintext())
	_, err = core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, wrong, verifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	_, err = core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 99, envelope, verifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthAttemptForbidden)
	_, err = core.AbandonHandoff(ctx, f.in.ProfileRef, f.in.OperationID, 42)
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-")
	require.NotContains(t, string(encoded), "private")
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("ab", 32)
	enc, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	cleartext, err := enc.Decrypt(f.store.rows[f.in.ProfileRef].Ciphertext)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal([]byte(cleartext), &stored))
	require.Empty(t, stored["Handoff"].(map[string]any)["TransferPrivateKey"])
	require.NotContains(t, f.store.rows[f.in.ProfileRef].Ciphertext, f.protocol.grant.RefreshToken)
}

func TestCoreOAuthTransferRequiresLiveVerifierAndQuiescence(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	envelope := coreSealTransfer(t, f.invitation, f.plaintext())
	_, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, nil)
	require.ErrorIs(t, err, service.ErrCoreOAuthUnavailable)
	_, err = f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, func(context.Context, service.CoreOAuthHandoffVerification) error {
		return errors.New("upstream credential secret")
	})
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.NotContains(t, err.Error(), "secret")
	value := f.plaintext()
	value["source_quiesced"] = false
	_, err = f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, coreSealTransfer(t, f.invitation, value), coreTransferAcceptingVerifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthInvalid)
	_, err = f.core.CredentialSnapshot(ctx, f.in.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
}

func TestCoreOAuthTransferRejectsIdentityTamperingAndAmbiguousPayload(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	for _, field := range []string{"contract", "operation_id", "profile_ref", "source_profile_ref", "provider", "subject", "workspace_id", "identity_epoch", "token_revision"} {
		t.Run(field, func(t *testing.T) {
			value := f.plaintext()
			if field == "token_revision" {
				value[field] = 8
			} else {
				value[field] = "wrong"
			}
			calls := 0
			_, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, coreSealTransfer(t, f.invitation, value), func(context.Context, service.CoreOAuthHandoffVerification) error { calls++; return nil })
			require.ErrorIs(t, err, service.ErrCoreOAuthInvalid)
			require.Zero(t, calls)
		})
	}
	for _, change := range []func(map[string]any){
		func(p map[string]any) { p["grant"].(map[string]any)["subject"] = "wrong" },
		func(p map[string]any) { p["grant"].(map[string]any)["client_id"] = "wrong" },
		func(p map[string]any) {
			p["grant"].(map[string]any)["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
		},
		func(p map[string]any) { p["publication_fences"] = nil },
		func(p map[string]any) { p["publication_fences"] = []any{"invalid"} },
		func(p map[string]any) { p["unexpected"] = true },
		func(p map[string]any) { delete(p["grant"].(map[string]any), "email") },
	} {
		p := f.plaintext()
		change(p)
		_, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, coreSealTransfer(t, f.invitation, p), coreTransferAcceptingVerifier)
		require.ErrorIs(t, err, service.ErrCoreOAuthInvalid)
	}
	raw, err := json.Marshal(f.plaintext())
	require.NoError(t, err)
	duplicate := append([]byte(`{"source_quiesced":false,`), raw[1:]...)
	_, err = f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, coreSealTransfer(t, f.invitation, nil, duplicate), coreTransferAcceptingVerifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthInvalid)
	sealed := coreSealTransfer(t, f.invitation, f.plaintext())
	bits, err := base64.RawURLEncoding.DecodeString(sealed.Ciphertext)
	require.NoError(t, err)
	bits[len(bits)-1] ^= 1
	sealed.Ciphertext = base64.RawURLEncoding.EncodeToString(bits)
	_, err = f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, sealed, coreTransferAcceptingVerifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthInvalid)
}

func TestCoreOAuthTransferCASFailureRemainsPendingAndCanRetry(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	envelope := coreSealTransfer(t, f.invitation, f.plaintext())
	f.store.mu.Lock()
	f.store.failCASAfter = 1
	f.store.mu.Unlock()
	_, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
	core := f.restart(t)
	_, err = core.CredentialSnapshot(ctx, f.in.ProfileRef)
	require.ErrorIs(t, err, service.ErrCoreOAuthHandoffPending)
	receipt, err := core.GetHandoff(ctx, f.in.ProfileRef, f.in.OperationID, 42)
	require.NoError(t, err)
	require.Equal(t, "pending", receipt.Status)
	receipt, err = core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
	require.NoError(t, err)
	require.Equal(t, "accepted", receipt.Status)
}

func TestCoreOAuthTransferExpiryAndAbandonmentNeverActivate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCoreTransferFixture(t)
		ctx := context.Background()
		envelope := coreSealTransfer(t, f.invitation, f.plaintext())
		time.Sleep(11 * time.Minute)
		_, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
		require.ErrorIs(t, err, service.ErrCoreOAuthHandoffExpired)
		_, err = f.restart(t).StartAuthorization(ctx, f.in.ProfileRef, 42)
		require.ErrorIs(t, err, service.ErrCoreOAuthHandoffExpired)
		receipt, err := f.core.AbandonHandoff(ctx, f.in.ProfileRef, f.in.OperationID, 42)
		require.NoError(t, err)
		require.Equal(t, "abandoned", receipt.Status)
		_, err = f.restart(t).AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
		require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
		_, err = f.restart(t).StartAuthorization(ctx, f.in.ProfileRef, 42)
		require.NoError(t, err)
	})
}

func TestCoreOAuthTransferAcceptedReceiptSurvivesRefreshAndReauthorization(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	envelope := coreSealTransfer(t, f.invitation, f.plaintext())
	accepted, err := f.core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
	require.NoError(t, err)
	core := f.restart(t)
	refreshed, err := core.Refresh(ctx, f.in.ProfileRef, 7)
	require.NoError(t, err)
	require.Equal(t, int64(8), refreshed.TokenRevision)
	core = f.restart(t)
	attempt, err := core.StartAuthorization(ctx, f.in.ProfileRef, 42)
	require.NoError(t, err)
	_, err = core.CancelAuthorization(ctx, f.in.ProfileRef, attempt.AttemptID, 42)
	require.NoError(t, err)
	attempt, err = core.StartAuthorization(ctx, f.in.ProfileRef, 42)
	require.NoError(t, err)
	profile, err := core.CompleteAuthorization(ctx, f.in.ProfileRef, service.CoreOAuthCompleteInput{AttemptID: attempt.AttemptID, ActorID: 42, CallbackURL: attempt.RedirectURI + "?code=fresh"})
	require.NoError(t, err)
	require.Equal(t, int64(9), profile.TokenRevision)
	require.Equal(t, f.in.IdentityEpoch, profile.IdentityEpoch)
	core = f.restart(t)
	snapshot, err := core.CredentialSnapshot(ctx, f.in.ProfileRef)
	require.NoError(t, err)
	require.True(t, snapshot.Profile.Authorized)
	receipt, err := core.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, coreTransferAcceptingVerifier)
	require.NoError(t, err)
	require.Equal(t, accepted, receipt)
}

func TestCoreOAuthTransferConcurrentActivationHasOneDurableGrant(t *testing.T) {
	f := newCoreTransferFixture(t)
	ctx := context.Background()
	envelope := coreSealTransfer(t, f.invitation, f.plaintext())
	other := f.restart(t)
	var ready sync.WaitGroup
	ready.Add(2)
	gate := make(chan struct{})
	verifier := func(context.Context, service.CoreOAuthHandoffVerification) error { ready.Done(); <-gate; return nil }
	results := make(chan error, 2)
	for _, core := range []*service.CoreOAuthService{f.core, other} {
		go func(c *service.CoreOAuthService) {
			_, err := c.AcceptProtectedHandoff(ctx, f.in.ProfileRef, 42, envelope, verifier)
			results <- err
		}(core)
	}
	ready.Wait()
	close(gate)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	snapshot, err := f.restart(t).CredentialSnapshot(ctx, f.in.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, int64(7), snapshot.Profile.TokenRevision)
}
