package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestAccountUsageBPSRequestProxyNeverUsesSingleExit(t *testing.T) {
	var selectedHits, outsideHits atomic.Int32
	selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectedHits.Add(1)
		require.True(t, r.URL.IsAbs())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`))
	}))
	defer selected.Close()
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outsideHits.Add(1)
		http.Error(w, "outside", http.StatusBadGateway)
	}))
	defer outside.Close()
	chosen := quotaMaintenanceProxy(t, 4101, selected.URL)
	old := quotaMaintenanceProxy(t, 4102, outside.URL)
	account := bpsUsageAccount()
	account.ID = 4100
	account.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "workspace"}
	account.ProxyID, account.Proxy = &old.ID, old
	account.Extra = map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{chosen.ID}}
	svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{
		proxyRepo:            &quotaMaintenanceProxyRepo{proxies: map[int64]*Proxy{chosen.ID: chosen, old.ID: old}},
		privacyClientFactory: quotaMaintenanceFactory(selected.URL, outside.URL),
	}}
	snapshot, err := svc.fetchOpenAIBPSUsage(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, 12.0, snapshot.FiveHour.Utilization)
	require.EqualValues(t, 1, selectedHits.Load())
	require.Zero(t, outsideHits.Load())
	require.Equal(t, old.ID, *account.ProxyID)
}

func TestAccountUsageCodexRequestProxyNeverUsesSingleExit(t *testing.T) {
	var selectedHits, outsideHits atomic.Int32
	server := func(count *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count.Add(1)
			require.Equal(t, http.MethodConnect, r.Method, "the HTTPS probe must reach the selected loopback proxy")
			http.Error(w, "fixture rejects tunnel; no upstream connection", http.StatusBadGateway)
		}))
	}
	selected := server(&selectedHits)
	defer selected.Close()
	outside := server(&outsideHits)
	defer outside.Close()
	chosen := quotaMaintenanceProxy(t, 4201, selected.URL)
	old := quotaMaintenanceProxy(t, 4202, outside.URL)
	account := &Account{ID: 4200, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "test-token"}, ProxyID: &old.ID, Proxy: old,
		Extra: map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{chosen.ID}},
	}
	svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{proxyRepo: &quotaMaintenanceProxyRepo{proxies: map[int64]*Proxy{chosen.ID: chosen, old.ID: old}}}}
	_, err := svc.probeOpenAICodexSnapshot(context.Background(), account)
	require.Error(t, err, "the local proxy intentionally rejects CONNECT")
	require.Positive(t, selectedHits.Load())
	require.Zero(t, outsideHits.Load())
	require.Equal(t, old.ID, *account.ProxyID)
}

func TestAccountUsageRequestProxyUnavailableDoesNotSend(t *testing.T) {
	for _, bps := range []bool{false, true} {
		name := "codex"
		if bps {
			name = "bps"
		}
		t.Run(name, func(t *testing.T) {
			var outsideHits atomic.Int32
			outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				outsideHits.Add(1)
				http.Error(w, "outside", http.StatusBadGateway)
			}))
			defer outside.Close()
			old := quotaMaintenanceProxy(t, 4302, outside.URL)
			account := &Account{ID: 4300, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "workspace"}, ProxyID: &old.ID, Proxy: old,
				Extra: map[string]any{OpenAIProxyModeKey: OpenAIProxyModeRequest, OpenAIProxyIDsKey: []int64{4399}},
			}
			if bps {
				account.Platform = PlatformOpenAIBPS
			}
			factoryCalls := 0
			svc := &AccountUsageService{openAIQuotaService: &OpenAIQuotaService{
				proxyRepo:            &quotaMaintenanceProxyRepo{proxies: map[int64]*Proxy{old.ID: old}},
				privacyClientFactory: func(string) (*req.Client, error) { factoryCalls++; return nil, errors.New("unexpected client") },
			}}
			if bps {
				usage, err := svc.getOpenAIBPSUsage(context.Background(), account, true)
				require.NoError(t, err)
				require.Equal(t, "quota_unavailable", usage.ErrorCode)
				require.True(t, usage.FiveHour.QuotaUnknown)
			} else {
				_, err := svc.probeOpenAICodexSnapshot(context.Background(), account)
				require.Error(t, err)
			}
			require.Zero(t, factoryCalls)
			require.Zero(t, outsideHits.Load())
		})
	}
}
