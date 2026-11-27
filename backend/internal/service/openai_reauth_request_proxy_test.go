package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIReauthRequestProxyUsesSelectedExitAndReleases(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformOpenAIBPS} {
		t.Run(platform, func(t *testing.T) {
			var sent string
			var scoped context.Context
			runner := func(ctx context.Context, _ string, _ BatchLoginEntry, proxy string, _ func(string)) (*batchLoginRecord, string) {
				sent, scoped = proxy, ctx
				valid, err := ValidateOpenAIRequestProxyEgress(ctx, 1, proxy)
				require.True(t, valid)
				require.NoError(t, err)
				return nil, "synthetic_stop"
			}
			svc, _, admin, _ := newReauthTest(runner)
			saveTestCredential(t, svc)
			admin.account.Platform = platform
			legacy := int64(90)
			admin.account.ProxyID = &legacy
			admin.account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{11}}
			selected := &Proxy{ID: 11, Protocol: "http", Host: "selected.example", Port: 8080, Status: StatusActive}
			svc.batch.proxyRepo = &openAIMaintenanceProxyRepo{proxies: map[int64]*Proxy{11: selected, 90: {ID: 90, Protocol: "http", Host: "legacy.example", Port: 8080, Status: StatusActive}}}
			svc.reauthorize(context.Background(), 1, platform)
			require.Equal(t, selected.URL(), sent)
			require.Nil(t, admin.updated)
			valid, err := ValidateOpenAIRequestProxyEgress(scoped, 1, sent)
			require.True(t, valid)
			require.Error(t, err, "login completion must release the logical request lease")
		})
	}
}

func TestOpenAIReauthRequestProxyUnavailableNeverStartsLogin(t *testing.T) {
	for _, state := range []string{"missing", "inactive", "expired", "no_repository"} {
		t.Run(state, func(t *testing.T) {
			calls := 0
			svc, repo, admin, _ := newReauthTest(func(context.Context, string, BatchLoginEntry, string, func(string)) (*batchLoginRecord, string) {
				calls++
				return nil, "unexpected_login"
			})
			saveTestCredential(t, svc)
			legacy := int64(90)
			admin.account.ProxyID = &legacy
			admin.account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{11}}
			selected := &Proxy{ID: 11, Protocol: "http", Host: "selected.example", Port: 8080, Status: StatusActive}
			if state == "inactive" {
				selected.Status = StatusDisabled
			}
			if state == "expired" {
				expired := time.Now().Add(-time.Minute)
				selected.ExpiresAt = &expired
			}
			proxies := map[int64]*Proxy{90: {ID: 90, Protocol: "http", Host: "legacy.example", Port: 8080, Status: StatusActive}}
			if state != "missing" {
				proxies[11] = selected
			}
			if state != "no_repository" {
				svc.batch.proxyRepo = &openAIMaintenanceProxyRepo{proxies: proxies}
			}
			svc.reauthorize(context.Background(), 1, PlatformOpenAIBPS)
			require.Zero(t, calls)
			require.Nil(t, admin.updated)
			require.Equal(t, "request_proxy_unavailable", repo.rec.LastCode)
		})
	}
}

type changedRequestProxyReauthAdmin struct {
	*fakeReauthAdmin
	calls int
}

func (a *changedRequestProxyReauthAdmin) GetAccount(_ context.Context, _ int64) (*Account, error) {
	a.calls++
	if a.calls == 1 {
		return a.account, nil
	}
	latest := *a.account
	latest.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeSingle}
	return &latest, nil
}
func TestOpenAIReauthRequestProxyConfigSwitchNeverStartsLogin(t *testing.T) {
	calls := 0
	svc, repo, admin, _ := newReauthTest(func(context.Context, string, BatchLoginEntry, string, func(string)) (*batchLoginRecord, string) {
		calls++
		return nil, "unexpected_login"
	})
	saveTestCredential(t, svc)
	legacy := int64(90)
	admin.account.ProxyID = &legacy
	admin.account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{11}}
	svc.admin = &changedRequestProxyReauthAdmin{fakeReauthAdmin: admin}
	svc.batch.proxyRepo = &openAIMaintenanceProxyRepo{proxies: map[int64]*Proxy{11: {ID: 11, Protocol: "http", Host: "selected.example", Port: 8080, Status: StatusActive}}}
	svc.reauthorize(context.Background(), 1, PlatformOpenAIBPS)
	require.Zero(t, calls)
	require.Nil(t, admin.updated)
	require.Contains(t, []string{"request_proxy_changed", "request_proxy_unavailable"}, repo.rec.LastCode)
}
