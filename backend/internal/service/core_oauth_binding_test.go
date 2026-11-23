package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCoreOAuthExcelBindingAcceptsAPIKeyRuntimeWithoutOAuthSecrets(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	account := &Account{
		ID:       41,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "inference-key",
		},
		Extra: map[string]any{
			CoreOAuthAccountExtraKey: CoreOAuthAccountBinding{
				Contract:      CoreOAuthContract,
				ProfileRef:    "11111111-1111-4111-8111-111111111111",
				Provider:      CoreOAuthProviderExcel,
				Subject:       "subject",
				WorkspaceID:   "workspace",
				IdentityEpoch: "22222222-2222-4222-8222-222222222222",
				TokenRevision: 3,
				ExpiresAt:     expires,
			},
		},
	}
	binding, err := CoreOAuthAccountBindingFromAccount(account)
	require.NoError(t, err)
	require.Equal(t, CoreOAuthProviderExcel, binding.Provider)
	for _, name := range []string{"access_token", "refresh_token", "id_token"} {
		t.Run(name, func(t *testing.T) {
			account.Credentials[name] = "synthetic-token"
			defer delete(account.Credentials, name)
			_, err := CoreOAuthAccountBindingFromAccount(account)
			require.ErrorIs(t, err, ErrCoreOAuthConflict)
		})
	}
}

func TestCoreOAuthExcelBindingRejectsOAuthAccountShape(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "oauth-access-token",
		},
		Extra: map[string]any{
			CoreOAuthAccountExtraKey: CoreOAuthAccountBinding{
				Contract:      CoreOAuthContract,
				ProfileRef:    "11111111-1111-4111-8111-111111111111",
				Provider:      CoreOAuthProviderExcel,
				Subject:       "subject",
				WorkspaceID:   "workspace",
				IdentityEpoch: "22222222-2222-4222-8222-222222222222",
				TokenRevision: 1,
				ExpiresAt:     time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
			},
		},
	}
	_, err := CoreOAuthAccountBindingFromAccount(account)
	require.ErrorIs(t, err, ErrCoreOAuthConflict)
}
