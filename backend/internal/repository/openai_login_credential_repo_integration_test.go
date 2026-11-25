//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func createLoginCredentialTestAccount(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	client := testEntClient(t)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	acc := &service.Account{
		Name:        fmt.Sprintf("login-cred-%d", time.Now().UnixNano()),
		Platform:    service.PlatformOpenAIBPS,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: false,
		Credentials: map[string]any{"access_token": "at"},
		Extra:       map[string]any{},
	}
	require.NoError(t, repo.CreateWithAccountGroups(ctx, acc, nil))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", acc.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", acc.ID)
	})
	return acc.ID
}

func TestOpenAILoginCredentialRepository(t *testing.T) {
	ctx := context.Background()
	repo := NewOpenAILoginCredentialRepository(integrationDB)
	id := createLoginCredentialTestAccount(t, ctx)

	// Missing record.
	rec, err := repo.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, rec)
	exists, err := repo.Exists(ctx, id)
	require.NoError(t, err)
	require.False(t, exists)

	// Insert.
	require.NoError(t, repo.Upsert(ctx, &service.OpenAILoginCredentialRecord{
		AccountID: id, LoginEmail: "a@x.co", Target: "bps", PasswordCiphertext: "pw1", TOTPCiphertext: "totp1",
	}))
	rec, err = repo.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, "a@x.co", rec.LoginEmail)
	require.Equal(t, "bps", rec.Target)
	require.Equal(t, "pw1", rec.PasswordCiphertext)
	require.Equal(t, 0, rec.FailCount)
	require.Nil(t, rec.NextAttemptAt)

	// Failures increment the counter and store the cooldown and code.
	next := time.Now().Add(15 * time.Minute).UTC()
	require.NoError(t, repo.RecordAttempt(ctx, id, false, "login_failed", &next))
	require.NoError(t, repo.RecordAttempt(ctx, id, false, "login_timeout", &next))
	rec, err = repo.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 2, rec.FailCount)
	require.Equal(t, "login_timeout", rec.LastCode)
	require.NotNil(t, rec.NextAttemptAt)
	require.WithinDuration(t, next, *rec.NextAttemptAt, time.Second)
	require.NotNil(t, rec.LastAttemptAt)

	// Upsert replaces the secret and resets the failure state.
	require.NoError(t, repo.Upsert(ctx, &service.OpenAILoginCredentialRecord{
		AccountID: id, LoginEmail: "a@x.co", Target: "codex", PasswordCiphertext: "pw2", TOTPCiphertext: "totp2",
	}))
	rec, err = repo.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "pw2", rec.PasswordCiphertext)
	require.Equal(t, "codex", rec.Target)
	require.Equal(t, 0, rec.FailCount)
	require.Nil(t, rec.NextAttemptAt)
	require.Equal(t, "", rec.LastCode)

	// Success clears the failure state.
	require.NoError(t, repo.RecordAttempt(ctx, id, false, "login_failed", &next))
	require.NoError(t, repo.RecordAttempt(ctx, id, true, "", nil))
	rec, err = repo.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 0, rec.FailCount)
	require.Nil(t, rec.NextAttemptAt)

	// Target is constrained.
	err = repo.Upsert(ctx, &service.OpenAILoginCredentialRecord{
		AccountID: id, LoginEmail: "a@x.co", Target: "other", PasswordCiphertext: "x", TOTPCiphertext: "y",
	})
	require.Error(t, err)

	// Delete.
	require.NoError(t, repo.Delete(ctx, id))
	exists, err = repo.Exists(ctx, id)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestOpenAILoginCredentialCascadesOnAccountDelete(t *testing.T) {
	ctx := context.Background()
	repo := NewOpenAILoginCredentialRepository(integrationDB)
	id := createLoginCredentialTestAccount(t, ctx)
	require.NoError(t, repo.Upsert(ctx, &service.OpenAILoginCredentialRecord{
		AccountID: id, LoginEmail: "b@x.co", Target: "bps", PasswordCiphertext: "pw", TOTPCiphertext: "totp",
	}))
	_, err := integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", id)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", id)
	require.NoError(t, err)
	exists, err := repo.Exists(ctx, id)
	require.NoError(t, err)
	require.False(t, exists)
}
