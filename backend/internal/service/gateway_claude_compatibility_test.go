//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClaudeOAuthCompatibilityPreservesAttributionAtReceiver(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
	account := &Account{ID: 812301, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	original := []byte(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.283.abc; cc_entrypoint=cli; cc_gateway=custom; cch=00000; future_tag=original-value;"},{"type":"text","text":"Keep these instructions","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"original-identity","future":true},"messages":[{"role":"user","content":"hello 👋"}]}`)
	svc := &GatewayService{cfg: &config.Config{}}
	req, _, err := svc.buildUpstreamRequest(context.Background(), c, account, original, "synthetic-token", "oauth", "claude-haiku-4-5", false, false)
	require.NoError(t, err)
	req.URL, err = url.Parse(server.URL)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	wire := <-received
	require.Contains(t, string(wire), "cc_gateway=custom;")
	require.Contains(t, string(wire), "cch=00000;")
	require.Contains(t, string(wire), "future_tag=original-value;")
	require.Equal(t, "Keep these instructions", gjson.GetBytes(wire, "system.1.text").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(wire, "system.1.cache_control.type").String())
	require.JSONEq(t, gjson.GetBytes(original, "messages").Raw, gjson.GetBytes(wire, "messages").Raw)
	require.JSONEq(t, gjson.GetBytes(original, "metadata").Raw, gjson.GetBytes(wire, "metadata").Raw)
}

func TestClaudeOAuthCompatibilityObservesFinalRequestWithoutChangingHeaderPolicy(t *testing.T) {
	for _, endpoint := range []string{"messages", "count_tokens"} {
		for _, mimic := range []bool{false, true} {
			name := endpoint
			if mimic {
				name += "/mimic"
			}
			t.Run(name, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				c.Request.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
				c.Request.Header.Set("X-Claude-Code-Wiggly-Dove", "git-origin-private")
				c.Request.Header.Set("X-Anthropic-Additional-Protection", "opaque-private")
				c.Request.Header.Set("X-Claude-Code-Agent-Id", "keep-agent")
				account := &Account{ID: 812302, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
				original := []byte(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.283.abc; cc_entrypoint=cli; cc_gateway=do-not-log-origin; cch=00000; future_secret=do-not-log-value;"},{"type":"text","text":"Keep instructions"}],"metadata":{"user_id":"device-private","future":true},"messages":[{"role":"user","content":"hello 👋"}]}`)
				svc := &GatewayService{cfg: &config.Config{}}
				var req *http.Request
				var wire []byte
				var err error
				if endpoint == "messages" {
					req, wire, err = svc.buildUpstreamRequest(context.Background(), c, account, original, "synthetic", "oauth", "claude-haiku-4-5", false, mimic)
				} else {
					req, wire, err = svc.buildCountTokensRequest(context.Background(), c, account, original, "synthetic", "oauth", "claude-haiku-4-5", mimic)
				}
				require.NoError(t, err)
				defer req.Body.Close()
				require.Contains(t, string(wire), "cc_gateway=do-not-log-origin")
				require.Contains(t, string(wire), "future_secret=do-not-log-value")
				require.True(t, claudeCompatibilityHeaderPresent(req.Header, "x-anthropic-additional-protection"))
				require.False(t, claudeCompatibilityHeaderPresent(req.Header, "x-claude-code-wiggly-dove"))
				require.Equal(t, "keep-agent", getHeaderRaw(req.Header, "X-Claude-Code-Agent-Id"))
				require.JSONEq(t, gjson.GetBytes(original, "metadata").Raw, gjson.GetBytes(wire, "metadata").Raw)
				observation := GetClaudeOAuthCompatibilityDiagnostic(account.ID)
				require.NotNil(t, observation)
				require.Equal(t, endpoint, observation.Endpoint)
				require.Equal(t, 1, observation.IncomingAttributionBlocks)
				require.Equal(t, 1, observation.OutgoingAttributionBlocks)
				require.Equal(t, "transparent", observation.Mode)
				require.True(t, observation.CCGatewayPresent)
				require.True(t, observation.CCHPresent)
				require.Equal(t, 1, observation.UnknownAttributionFields)
				require.True(t, observation.WigglyPresent)
				require.False(t, observation.WigglyForwarded)
				require.Contains(t, observation.Reasons, "wiggly_dropped_by_allowlist")
				require.Equal(t, "unverified", observation.IntegrityStatus)
				encoded, err := json.Marshal(observation)
				require.NoError(t, err)
				for _, secret := range []string{"git-origin-private", "opaque-private", "do-not-log-origin", "do-not-log-value", "device-private"} {
					require.NotContains(t, string(encoded), secret)
				}
			})
		}
	}
}

func TestClaudeOAuthCompatibilityDoesNotRejectOrRemoveUnusualAttribution(t *testing.T) {
	for _, text := range []string{"x-anthropic-billing-header preserve this instruction", "x-anthropic-billing-header: cc_version=2.1.283.abc; Preserve these instructions", "x-anthropic-billing-header: future=value;\nKeep mixed instructions"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		body, err := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "system": []any{map[string]any{"type": "text", "text": text, "future": true}}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
		require.NoError(t, err)
		account := &Account{ID: 812303, Platform: PlatformAnthropic, Type: AccountTypeOAuth}
		svc := &GatewayService{cfg: &config.Config{}}
		req, wire, err := svc.buildUpstreamRequest(context.Background(), c, account, body, "synthetic", "oauth", "claude-haiku-4-5", false, false)
		require.NoError(t, err)
		req.Body.Close()
		require.Equal(t, text, gjson.GetBytes(wire, "system.0.text").String())
		require.True(t, gjson.GetBytes(wire, "system.0.future").Bool())
		observed := GetClaudeOAuthCompatibilityDiagnostic(account.ID)
		require.NotNil(t, observed)
		require.Contains(t, observed.Reasons, "noncanonical_attribution")
	}
}

func TestClaudeOAuthCompatibilityOnlyObservesOAuthAccounts(t *testing.T) {
	for index, target := range []struct{ platform, accountType string }{
		{PlatformAnthropic, AccountTypeAPIKey},
		{PlatformAnthropic, AccountTypeSetupToken},
		{PlatformOpenAI, AccountTypeOAuth},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		account := &Account{ID: int64(812310 + index), Platform: target.platform, Type: target.accountType}
		original := []byte(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.283.abc; cc_gateway=original;"}],"messages":[{"role":"user","content":"hello"}]}`)
		svc := &GatewayService{cfg: &config.Config{}}
		req, wire, err := svc.buildUpstreamRequest(context.Background(), c, account, original, "synthetic", "apikey", "claude-haiku-4-5", false, false)
		require.NoError(t, err)
		req.Body.Close()
		require.Contains(t, string(wire), "cc_gateway=original;")
		require.Nil(t, GetClaudeOAuthCompatibilityDiagnostic(account.ID))
	}
}

func TestClaudeOAuthCompatibilityDebugHeadersAreRedacted(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "gateway-debug-*.log")
	require.NoError(t, err)
	defer file.Close()
	svc := &GatewayService{}
	svc.debugGatewayBodyFile.Store(file)
	headers := http.Header{"X-Claude-Code-Wiggly-Dove": {"private-repo-origin"}, "X-Anthropic-Additional-Protection": {"private-integrity-state"}, "X-Claude-Code-Agent-Id": {"visible-agent"}}
	svc.debugLogGatewaySnapshot("CLIENT_ORIGINAL", headers, []byte(`{"messages":[]}`), nil)
	output, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	require.NotContains(t, string(output), "private-repo-origin")
	require.NotContains(t, string(output), "private-integrity-state")
	require.Contains(t, string(output), "visible-agent")
	require.Contains(t, string(output), "[redacted]")
}

func TestClaudeOAuthCompatibilityObservationsExpireAreBoundedAndCopied(t *testing.T) {
	claudeOAuthCompatibilityObservations.Lock()
	original := claudeOAuthCompatibilityObservations.entries
	claudeOAuthCompatibilityObservations.entries = make(map[int64]claudeOAuthCompatibilityEntry)
	claudeOAuthCompatibilityObservations.Unlock()
	defer func() {
		claudeOAuthCompatibilityObservations.Lock()
		claudeOAuthCompatibilityObservations.entries = original
		claudeOAuthCompatibilityObservations.Unlock()
	}()
	storeClaudeOAuthCompatibilityDiagnostic(1, ClaudeOAuthCompatibilityDiagnostic{Reasons: []string{"unknown_attribution_fields"}})
	observed := GetClaudeOAuthCompatibilityDiagnostic(1)
	require.NotNil(t, observed)
	observed.Reasons[0] = "caller mutation"
	require.Equal(t, "unknown_attribution_fields", GetClaudeOAuthCompatibilityDiagnostic(1).Reasons[0])
	claudeOAuthCompatibilityObservations.Lock()
	entry := claudeOAuthCompatibilityObservations.entries[1]
	entry.expiresAt = time.Now().Add(-time.Second)
	claudeOAuthCompatibilityObservations.entries[1] = entry
	claudeOAuthCompatibilityObservations.Unlock()
	require.Nil(t, GetClaudeOAuthCompatibilityDiagnostic(1))
	for i := int64(1); i <= claudeOAuthCompatibilityCapacity+10; i++ {
		storeClaudeOAuthCompatibilityDiagnostic(i, ClaudeOAuthCompatibilityDiagnostic{})
	}
	claudeOAuthCompatibilityObservations.Lock()
	size := len(claudeOAuthCompatibilityObservations.entries)
	claudeOAuthCompatibilityObservations.Unlock()
	require.Equal(t, claudeOAuthCompatibilityCapacity, size)
}

func TestClaudeOAuthCompatibilityPreservesOriginalAttributionThroughPublicForward(t *testing.T) {
	for _, countTokens := range []bool{false, true} {
		name := "messages"
		responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":7}}`
		if countTokens {
			name = "count_tokens"
			responseBody = `{"input_tokens":12}`
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", "other-client/1.0")
			body := []byte(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.283.abc; cc_gateway=private-system-origin; future=original;"},{"type":"text","text":"Project instructions"}],"messages":[{"role":"user","content":"x-anthropic-billing-header: user-owned literal"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
			require.NoError(t, err)
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(responseBody))}}
			cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
			svc := &GatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream, rateLimitService: &RateLimitService{}, deferredService: &DeferredService{}}
			account := &Account{ID: 812320, Name: "compatibility-test", Platform: PlatformAnthropic, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic"}, Status: StatusActive, Schedulable: true}
			if countTokens {
				err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
			} else {
				_, err = svc.Forward(context.Background(), c, account, parsed)
			}
			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Contains(t, string(upstream.lastBody), "cc_gateway=private-system-origin;")
			require.Contains(t, string(upstream.lastBody), "future=original;")
			require.Contains(t, string(upstream.lastBody), "Project instructions")
			require.Contains(t, string(upstream.lastBody), "x-anthropic-billing-header: user-owned literal")
			observation := GetClaudeOAuthCompatibilityDiagnostic(account.ID)
			require.NotNil(t, observation)
			require.Equal(t, 1, observation.IncomingAttributionBlocks)
			require.True(t, observation.CCGatewayPresent)
			require.Equal(t, "transparent", observation.Mode)
		})
	}
}
