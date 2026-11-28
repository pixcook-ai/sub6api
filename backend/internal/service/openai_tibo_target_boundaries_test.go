package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tiboPrewarmIsolationRepo struct {
	AccountRepository
	conn *openAIWSCaptureConn
}

func (r *tiboPrewarmIsolationRepo) GetOpenAITiboQualityIsolation(context.Context, int64) (bool, error) {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()
	return len(r.conn.writes) > 0, nil
}

func TestTiboPrivateProbeDoesNotBorrowBusinessTicket(t *testing.T) {
	account := ticketTestAccount(41)
	account.Extra = map[string]any{openAICodexTicketExtraKey("gpt-6-astra"): verifiedTicket(account, "gpt-6-astra", "business-state", "__oailb=business")}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	headers := http.Header{}
	ticket, err := svc.applyOpenAICodexTicketWithGeneration(withOpenAITiboSyntheticProbe(context.Background(), account.ID), account, "gpt-6-astra", headers)
	require.NoError(t, err)
	require.Nil(t, ticket)
	require.Empty(t, headers.Get(openAICodexTurnStateHeader))
	require.Empty(t, headers.Get("Cookie"))
	ordinary := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, "gpt-6-astra", ordinary))
	require.Equal(t, "business-state", ordinary.Get(openAICodexTurnStateHeader))
}

func TestTiboIsolationDuringWSPrewarmStopsBusinessSend(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("session_id", "session-prewarm")

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1

	captureConn := &openAIWSCaptureConn{
		events: [][]byte{
			[]byte(`{"type":"response.completed","response":{"id":"resp_prewarm_1","model":"gpt-5.1","usage":{"input_tokens":0,"output_tokens":0}}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_main_1","model":"gpt-5.1","usage":{"input_tokens":4,"output_tokens":2}}}`),
		},
	}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}

	account := &Account{
		ID:          59,
		Name:        "openai-prewarm",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
		},
	}

	svc.accountRepo = &tiboPrewarmIsolationRepo{conn: captureConn}

	body := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
	require.Len(t, captureConn.writes, 1, "only the prewarm may be sent after isolation")
}

func TestTiboWSHTTPBridgePreservesFirstTurnContinuationOnIsolation(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-test"}}
	isolated := *account
	isolated.Extra = map[string]any{OpenAITiboQualityIsolatedExtraKey: true}
	upstream := &httpUpstreamRecorder{}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream,
		accountRepo: &tiboQualityAdmissionRepo{accounts: map[int64]*Account{41: &isolated}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_bound","input":"continue"}`)
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "sk-test", payload, len(payload), "gpt-5.1", "", "", "", "", 1, func([]byte) error { return nil })
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.ShouldRetryNextAccount())
	require.Empty(t, upstream.bodies)
}
