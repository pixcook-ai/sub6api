//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRequestProxyRefreshConfigSwitchRejectsOldTokenFallback(t *testing.T) {
	for _, mode := range []string{OpenAIProxyModeSingle, OpenAIProxyModeRequest} {
		for _, refreshNeeded := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "already_fresh", true: "refresh_due"}[refreshNeeded], func(t *testing.T) {
				saved := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
					Credentials: map[string]any{"access_token": "old-access", "refresh_token": "synthetic-rt", "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339)},
					Extra:       map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{1, 2}}}
				latest := *saved
				latest.Extra = map[string]any{OpenAIProxyModeKey: mode, OpenAIProxyIDsKey: []int64{2}}
				repo := &refreshAPIAccountRepo{account: &latest}
				executor := &refreshAPIExecutorStub{needsRefresh: refreshNeeded}
				api := NewOAuthRefreshAPI(repo, nil)
				provider := NewOpenAITokenProvider(repo, nil, nil)
				provider.SetRefreshAPI(api, executor)
				route := &openAIRequestProxyRoute{accountID: saved.ID, proxyID: 1, proxy: "http://selected.example:8080"}
				ctx := context.WithValue(context.Background(), openAIRequestProxyContextKey{}, route)
				token, err := provider.GetAccessToken(ctx, saved)
				require.Error(t, err)
				require.True(t, isOpenAIRequestProxyRoutingError(err))
				require.Empty(t, token, "an existing access token must not bypass routing rejection")
				require.Zero(t, executor.refreshCalls, "route rejection must precede token endpoint")
				require.Zero(t, repo.updateCalls)
			})
		}
	}
}
