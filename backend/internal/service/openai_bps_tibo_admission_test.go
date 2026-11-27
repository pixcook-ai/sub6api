package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIBPSTiboQualityIsolationPreservesManualPause(t *testing.T) {
	_, account := bpsFixture()
	account.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
	require.True(t, account.IsOpenAITiboQualityIsolated())
	require.False(t, account.IsSchedulable())
	require.True(t, account.Schedulable)
	account.Schedulable = false
	delete(account.Extra, OpenAITiboQualityIsolatedExtraKey)
	require.False(t, account.IsSchedulable(), "clearing isolation must preserve an administrator pause")
	account.Schedulable = true
	require.True(t, account.IsSchedulable())
}

func TestOpenAIBPSTiboFinalAdmissionReadsDurableIsolationWhenMonitorDisabled(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "new session"
		if bound {
			name = "bound session"
		}
		t.Run(name, func(t *testing.T) {
			svc, account := bpsFixture()
			marked := *account
			marked.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
			repo := &tiboQualityAdmissionRepo{accounts: map[int64]*Account{account.ID: &marked}}
			svc.accountRepo = repo
			off := false
			svc.cfg = &config.Config{}
			svc.cfg.Gateway.OpenAITiboRoute.Enabled = &off
			upstream := &bpsHTTPStub{body: bpsJSON(bpsResponse(bpsText("True"))), contentType: "application/json"}
			svc.httpUpstream = upstream
			c, rec := bpsContext(1, "/v1/responses")
			body := bpsRequestBody("task", false)
			require.NoError(t, svc.PrepareOpenAIBPSRouting(c, body))
			if bound {
				require.NoError(t, svc.bpsBind(c.Request.Context(), svc.bpsScope(c, body), account))
				require.NoError(t, svc.PrepareOpenAIBPSRouting(c, body))
			}
			before := append([]byte(nil), svc.cache.(*bpsMemoryStore).data[svc.bpsScope(c, body)+":session"]...)
			result, err := svc.Forward(c.Request.Context(), c, account, body)
			require.Nil(t, result)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, http.StatusServiceUnavailable, failover.ClientStatusCode)
			require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
			require.Equal(t, !bound, failover.ShouldRetryNextAccount())
			require.Zero(t, upstream.requests)
			require.Empty(t, rec.Body.String(), "leave the failover response to the handler")
			require.Equal(t, 1, repo.qualityReads)
			require.Zero(t, repo.fullReads)
			require.Equal(t, before, svc.cache.(*bpsMemoryStore).data[svc.bpsScope(c, body)+":session"], "quality rejection must preserve the existing conversation binding")
		})
	}
}

func TestOpenAIBPSTiboFinalAdmissionFailsClosedOnAuthorityFailure(t *testing.T) {
	svc, account := bpsFixture()
	svc.accountRepo = &tiboQualityAdmissionRepo{err: errors.New("database unavailable")}
	upstream := &bpsHTTPStub{}
	svc.httpUpstream = upstream
	c, rec := bpsContext(1, "/v1/responses")
	result, err := svc.Forward(c.Request.Context(), c, account, bpsRequestBody("task", false))
	require.Nil(t, result)
	require.ErrorContains(t, err, "verify OpenAI quality isolation")
	require.Zero(t, upstream.requests)
	require.NotContains(t, rec.Body.String(), "response.completed")
}

func TestOpenAIBPSTiboSyntheticAdmissionExemptionIsAccountBound(t *testing.T) {
	for _, probeAccountID := range []int64{0, 7, 8} {
		t.Run(map[int64]string{0: "public request", 7: "target probe", 8: "another account probe"}[probeAccountID], func(t *testing.T) {
			svc, account := bpsFixture()
			account.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
			svc.accountRepo = &tiboQualityAdmissionRepo{accounts: map[int64]*Account{account.ID: account}}
			upstream := &bpsHTTPStub{body: bpsJSON(bpsResponse(bpsText("True"))), contentType: "application/json"}
			svc.httpUpstream = upstream
			c, _ := bpsContext(1, "/v1/responses")
			body := bpsRequestBody(openAITiboProbePrompt, false)
			// Neither the same question nor a client-supplied probe header grants access.
			c.Request.Header.Set("X-OpenAI-Tibo-Synthetic-Probe", "7")
			ctx := c.Request.Context()
			if probeAccountID != 0 {
				ctx = withOpenAITiboSyntheticProbe(ctx, probeAccountID)
				c.Request = c.Request.WithContext(ctx)
			}
			_, err := svc.Forward(ctx, c, account, body)
			if probeAccountID == account.ID {
				require.NoError(t, err)
				require.Equal(t, 1, upstream.requests)
			} else {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Zero(t, upstream.requests)
			}
		})
	}
}

func TestOpenAIBPSTiboSchedulerSkipsIsolatedStickyAndPreservesBinding(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, bound := range []bool{false, true} {
			t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced]+"/"+map[bool]string{false: "sticky", true: "bound"}[bound], func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				_, account := bpsFixture()
				account.Concurrency = 1
				account.GroupIDs = []int64{91}
				account.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
				other := *account
				other.ID = 8
				other.Priority = 10
				other.Extra = nil
				svc := newOpenAICompactionSchedulerTestService([]Account{*account, other}, advanced)
				svc.cache = &schedulerTestGatewayCache{sessionBindings: map[string]int64{svc.openAISessionCacheKey("tibo-session"): account.ID}}
				ctx := context.Background()
				binding := bpsRoutingContext{Scope: "test"}
				if bound {
					binding.Binding = &bpsSessionBinding{account.ID, account.GetCredential("chatgpt_account_id")}
					ctx = context.WithValue(ctx, bpsRoutingContextKey{}, binding)
				}
				group := int64(91)
				selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, "", "tibo-session", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses, false, false, false, PlatformOpenAIBPS)
				if bound {
					require.Error(t, err)
					require.Nil(t, selection, "a bound BPS conversation must never escape to another account")
					require.Equal(t, binding, ctx.Value(bpsRoutingContextKey{}))
				} else {
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, other.ID, selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				}
			})
		}
	}
}
