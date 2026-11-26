//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAITiboProbeIdentityHasStableDigestAndIgnoresObservations(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-only"}}
	// Independent SHA256 fixture for the documented four-field identity JSON.
	expected := "31c5b581a0bb3d73edbbac07c0836c7edbce919a3a512e34b2ab32800782f4ee"
	require.Equal(t, expected, OpenAITiboProbeIdentity(account))
	account.Extra = map[string]any{"codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339Nano), OpenAITiboQualityIsolatedExtraKey: true}
	account.UpdatedAt = time.Now()
	account.Name = "administrator edit"
	require.Equal(t, expected, OpenAITiboProbeIdentity(account))
}

func TestOpenAITiboProbeIdentityChangesWithUpstreamBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"platform", func(a *Account) { a.Platform = PlatformAnthropic }},
		{"type", func(a *Account) { a.Type = AccountTypeSetupToken }},
		{"credentials", func(a *Account) { a.Credentials["access_token"] = "replacement-fixture-only" }},
		{"proxy binding", func(a *Account) { id := int64(317); a.ProxyID = &id }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-only"}}
			before := OpenAITiboProbeIdentity(account)
			tc.mutate(account)
			require.NotEqual(t, before, OpenAITiboProbeIdentity(account))
		})
	}
	first := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"b": "second", "a": "first"}}
	second := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"a": "first", "b": "second"}}
	require.Equal(t, OpenAITiboProbeIdentity(first), OpenAITiboProbeIdentity(second), "JSON object insertion order is not identity")
	malformed := &Account{Credentials: map[string]any{"invalid": make(chan struct{})}}
	require.Equal(t, "invalid", OpenAITiboProbeIdentity(malformed), "a malformed map cannot opt out of production identity fencing")
}
