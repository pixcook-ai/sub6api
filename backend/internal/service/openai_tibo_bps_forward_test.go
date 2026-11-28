package service

import (
	"context"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsTiboForwardFixture(verdict openAITiboVerdict, enabled bool) (*OpenAIGatewayService, *Account, *bpsHTTPStub) {
	svc, account := bpsFixture()
	svc.cfg = &config.Config{}
	svc.cfg.Gateway.OpenAITiboRoute.Enabled = &enabled
	upstream := &bpsHTTPStub{contentType: "application/json", body: bpsJSON(bpsResponse(bpsText("business response")))}
	svc.httpUpstream = upstream
	now := time.Now()
	state := svc.openAITiboAccountState(account.ID)
	state.identity = OpenAITiboProbeIdentity(account)
	state.http = openAITiboRouteState{verdict: verdict, checkedAt: now, lastSampleAt: now, nextProbeAt: now.Add(time.Hour)}
	return svc, account, upstream
}

func TestTiboBPSForwardRouteDegradedTriStatePreservesWireAndBinding(t *testing.T) {
	for _, tt := range []struct {
		name    string
		verdict openAITiboVerdict
		enabled bool
		compact bool
		trigger bool
		want    *bool
	}{
		{name: "degraded", verdict: openAITiboDegraded, enabled: true, want: bpsTiboBool(true)},
		{name: "healthy", verdict: openAITiboHealthy, enabled: true, want: bpsTiboBool(false)},
		{name: "unknown", verdict: openAITiboUnknown, enabled: true, want: bpsTiboBool(false)},
		{name: "disabled", verdict: openAITiboDegraded, enabled: false},
		{name: "compact endpoint", verdict: openAITiboDegraded, enabled: true, compact: true},
		{name: "compact trigger", verdict: openAITiboDegraded, enabled: true, trigger: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, account, upstream := bpsTiboForwardFixture(tt.verdict, tt.enabled)
			path := "/v1/responses"
			if tt.compact {
				path += "/compact"
			}
			c, _ := bpsContext(101, path)
			input := any("business request")
			if tt.trigger {
				input = []any{bpsMessage("user", "business request"), map[string]any{"type": "compaction_trigger"}}
			}
			body := bpsRequestBody(input, false)
			require.NoError(t, svc.PrepareOpenAIBPSRouting(c, body))
			scope := svc.bpsScope(c, body)
			require.NoError(t, svc.bpsBind(c.Request.Context(), scope, account))
			require.NoError(t, svc.PrepareOpenAIBPSRouting(c, body))
			store, ok := svc.cache.(*bpsMemoryStore)
			require.True(t, ok)
			before := append([]byte(nil), store.data[scope+":session"]...)
			result, err := svc.Forward(c.Request.Context(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, tt.want, result.RouteDegraded)
			require.Equal(t, 1, upstream.requests, "the business path must not recursively probe an already sampled account")
			require.Equal(t, OpenAIBPSResponsesURL, upstream.request.URL.String())
			require.Equal(t, "Bearer test-secret", upstream.request.Header.Get("Authorization"))
			require.Equal(t, "workspace-1", upstream.request.Header.Get("chatgpt-account-id"))
			require.Empty(t, upstream.request.Header.Get("originator"))
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.requestBody, "model").String())
			require.Equal(t, before, store.data[scope+":session"])
			if tt.want != nil {
				require.Equal(t, openAITiboRouteHTTP, svc.loadOpenAITiboPin(account.ID, scope, time.Now()), "quality bookkeeping uses the tenant-scoped BPS conversation")
			}
		})
	}
}

func bpsTiboBool(value bool) *bool { return &value }

func TestTiboBPSForwardObservesSuccessfulModelMismatch(t *testing.T) {
	svc, account, upstream := bpsTiboForwardFixture(openAITiboHealthy, true)
	repo := &tiboAccountRepo{account: cloneTiboAccount(account), updates: map[int64]map[string]any{}}
	svc.accountRepo = repo
	response := bpsResponse(bpsText("business response"))
	response["model"] = "gpt-6-luna"
	upstream.body = bpsJSON(response)
	// Reserve the existing hourly budget so this check observes the passive
	// result without launching its asynchronous confirmation probe.
	state := svc.loadOpenAITiboAccountState(account.ID)
	for range svc.openAITiboRouteConfig().maxProbesPerHour {
		state.http.probes = append(state.http.probes, time.Now())
	}
	c, _ := bpsContext(101, "/v1/responses")
	result, err := svc.Forward(c.Request.Context(), c, account, bpsRequestBody("business request", false))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, bpsTiboBool(false), result.RouteDegraded, "the flag describes the route quality when this attempt started")
	require.Equal(t, "gpt-6-luna", result.UpstreamResponseModel)
	require.Equal(t, openAITiboDegraded, svc.openAITiboEffective(account.ID, openAITiboRouteHTTP, time.Now()))
	stored, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	record, ok := parseOpenAITiboVerdictRecord(stored.Extra[openAITiboVerdictExtraKey(openAITiboRouteHTTP)])
	require.True(t, ok)
	require.Equal(t, openAITiboDegraded, record.Verdict)
	require.Equal(t, 1, upstream.requests)
}

func TestTiboBPSForwardDoesNotObserveFailedResponseModelMismatch(t *testing.T) {
	svc, account, upstream := bpsTiboForwardFixture(openAITiboHealthy, true)
	response := bpsResponse(bpsText("incomplete response"))
	response["model"], response["status"] = "gpt-6-luna", "incomplete"
	upstream.body = bpsJSON(response)
	c, _ := bpsContext(101, "/v1/responses")
	result, err := svc.Forward(c.Request.Context(), c, account, bpsRequestBody("business request", false))
	require.Error(t, err)
	require.NotNil(t, result, "preserve usage on a failed BPS response")
	require.Equal(t, bpsTiboBool(false), result.RouteDegraded)
	require.Equal(t, openAITiboHealthy, svc.openAITiboEffective(account.ID, openAITiboRouteHTTP, time.Now()), "failed business output is not passive downgrade evidence")
	require.Equal(t, 1, upstream.requests)
}
