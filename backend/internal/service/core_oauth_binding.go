package service

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

func coreOAuthIdentityText(value string) bool {
	return value != "" && len(value) <= 1024 && value == strings.TrimSpace(value) && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}
func coreOAuthSecretText(value string) bool {
	return value != "" && len(value) <= 65536 && value == strings.TrimSpace(value) && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}
func coreOAuthClientID(provider string) string {
	if provider == CoreOAuthProviderExcel {
		return openai.BPSExcelClientID
	}
	if provider == CoreOAuthProviderCodex {
		return openai.ClientID
	}
	return ""
}

// CoreOAuthAccountBindingForSnapshot exposes only the identity and token version.
// Runtime accounts receive the AT separately; RT never crosses this boundary.
func CoreOAuthAccountBindingForSnapshot(snapshot *CoreOAuthCredentialSnapshot) (*CoreOAuthAccountBinding, error) {
	if snapshot == nil || !snapshot.Profile.Authorized || snapshot.Profile.ExpiresAt == nil || !snapshot.Profile.ExpiresAt.After(time.Now()) || !coreOAuthSecretText(snapshot.AccessToken) {
		return nil, ErrCoreOAuthAuthorizationRequired
	}
	profile := snapshot.Profile
	binding := &CoreOAuthAccountBinding{Contract: CoreOAuthContract, ProfileRef: profile.ProfileRef, Provider: profile.Provider, Subject: profile.Subject, WorkspaceID: profile.WorkspaceID, IdentityEpoch: profile.IdentityEpoch, TokenRevision: profile.TokenRevision, ExpiresAt: profile.ExpiresAt.UTC().Format(time.RFC3339Nano)}
	if !coreOAuthBindingValid(binding) || profile.ClientID != coreOAuthClientID(profile.Provider) {
		return nil, ErrCoreOAuthConflict
	}
	return binding, nil
}

func coreOAuthBindingValid(binding *CoreOAuthAccountBinding) bool {
	if binding == nil || binding.Contract != CoreOAuthContract || !coreOAuthRef(binding.ProfileRef) || !coreOAuthProvider(binding.Provider) || !coreOAuthIdentityText(binding.Subject) || !coreOAuthIdentityText(binding.WorkspaceID) || !coreOAuthRef(binding.IdentityEpoch) || binding.TokenRevision < 1 {
		return false
	}
	expiry, err := time.Parse(time.RFC3339Nano, binding.ExpiresAt)
	return err == nil && !expiry.IsZero()
}

// CoreOAuthAccountBindingFromAccount validates a core runtime snapshot, not proof
// that a profile is still current. Callers must also read the core service.
func CoreOAuthAccountBindingFromAccount(account *Account) (*CoreOAuthAccountBinding, error) {
	if account == nil || ManagedOAuthDeclared(account) || account.IsCredentialShadow() {
		return nil, ErrCoreOAuthConflict
	}
	_, hasRefreshToken := account.Credentials["refresh_token"]
	_, hasIDToken := account.Credentials["id_token"]
	if hasRefreshToken || hasIDToken {
		return nil, ErrCoreOAuthConflict
	}
	raw, exists := account.Extra[CoreOAuthAccountExtraKey]
	if !exists {
		return nil, ErrCoreOAuthInvalid
	}
	data, err := json.Marshal(raw)
	if err != nil || len(data) > 16384 {
		return nil, ErrCoreOAuthInvalid
	}
	var binding CoreOAuthAccountBinding
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&binding) != nil || !coreOAuthBindingValid(&binding) {
		return nil, ErrCoreOAuthInvalid
	}
	if binding.Provider == CoreOAuthProviderExcel {
		// BPS runtime accounts authenticate to the inference API with their own
		// API key. The OAuth access token is delivered to the worker separately.
		// Keep only the non-secret core_oauth binding marker on this account.
		_, hasAccessToken := account.Credentials["access_token"]
		if account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey || hasAccessToken {
			return nil, ErrCoreOAuthConflict
		}
		return &binding, nil
	}
	expiry, err := time.Parse(time.RFC3339Nano, account.GetCredential("expires_at"))
	bindingExpiry, _ := time.Parse(time.RFC3339Nano, binding.ExpiresAt)
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.GetCredential("chatgpt_account_id") != binding.WorkspaceID || account.GetCredential("client_id") != coreOAuthClientID(binding.Provider) || !coreOAuthSecretText(account.GetCredential("access_token")) || err != nil || !expiry.Equal(bindingExpiry) {
		return nil, ErrCoreOAuthConflict
	}
	return &binding, nil
}
