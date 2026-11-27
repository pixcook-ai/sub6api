package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The real CLI's /context command consumes count_tokens responses through the
// actual service. Authentication middleware, OAuth and production upstreams are
// outside this local contract; both account modes use synthetic API keys.
func TestClaudeCLIConsumesGatewayCountTokens(t *testing.T) {
	binary := os.Getenv("CLAUDE_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("set CLAUDE_CLI_TEST_BINARY to run the local count_tokens CLI contract")
	}
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"standard", "apikey_passthrough"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var unexpectedRequests atomic.Int64
			var mu sync.Mutex
			var exchanges []struct {
				inbound, outbound claudeCLIRequestReference
				inboundBetaQuery  string
				outboundBetaQuery string
				upstreamKey       string
				status            int
				requestID         string
			}
			router := gin.New()
			router.NoRoute(func(c *gin.Context) {
				unexpectedRequests.Add(1)
				c.Status(http.StatusNotFound)
			})
			router.POST("/v1/messages/count_tokens", func(c *gin.Context) {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("ForwardCountTokens panicked: %v", recovered)
						cancel()
					}
				}()
				body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20))
				if err != nil {
					t.Errorf("read CLI count request: %v", err)
					cancel()
					return
				}
				parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
				if err != nil {
					t.Errorf("parse CLI count request: %v", err)
					cancel()
					return
				}
				upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": {"application/json"},
						"Request-Id":   {"req_count_cli_contract"},
					},
					Body: io.NopCloser(strings.NewReader(`{"input_tokens":17}`)),
				}}
				cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
				svc := &GatewayService{
					cfg:                  cfg,
					responseHeaderFilter: compileResponseHeaderFilter(cfg),
					httpUpstream:         upstream,
					rateLimitService:     &RateLimitService{},
				}
				account := newAnthropicAPIKeyAccountForTest()
				account.Extra["anthropic_passthrough"] = mode == "apikey_passthrough"
				if err := svc.ForwardCountTokens(c.Request.Context(), c, account, parsed); err != nil {
					t.Errorf("ForwardCountTokens failed: %v", err)
					cancel()
					return
				}
				if upstream.lastReq == nil {
					t.Error("ForwardCountTokens did not call the synthetic upstream")
					cancel()
					return
				}
				mu.Lock()
				exchanges = append(exchanges, struct {
					inbound, outbound claudeCLIRequestReference
					inboundBetaQuery  string
					outboundBetaQuery string
					upstreamKey       string
					status            int
					requestID         string
				}{
					captureClaudeCLIReference(c.Request, body), captureClaudeCLIReference(upstream.lastReq, upstream.lastBody),
					c.Request.URL.Query().Get("beta"), upstream.lastReq.URL.Query().Get("beta"),
					getHeaderRaw(upstream.lastReq.Header, "x-api-key"), c.Writer.Status(), c.Writer.Header().Get("Request-Id"),
				})
				mu.Unlock()
			})
			server := httptest.NewServer(router)
			defer server.Close()
			rejectProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer rejectProxy.Close()
			cmd := exec.CommandContext(ctx, binary,
				"--bare", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
				"--tools", "", "--allowedTools", "", "--permission-mode", "dontAsk", "--no-session-persistence",
				"--model", "claude-sonnet-4-5-20250929", "--output-format", "json", "--system-prompt", "Reply briefly.", "-p", "/context")
			cmd.Dir = t.TempDir()
			cmd.WaitDelay = 2 * time.Second
			for _, key := range []string{"PATH", "HOME", "TMPDIR", "SystemRoot"} {
				if value, exists := os.LookupEnv(key); exists {
					cmd.Env = append(cmd.Env, key+"="+value)
				}
			}
			cmd.Env = append(cmd.Env,
				"CLAUDE_CONFIG_DIR="+t.TempDir(), "ANTHROPIC_API_KEY=local-contract-key", "ANTHROPIC_BASE_URL="+server.URL,
				"HTTP_PROXY="+rejectProxy.URL, "HTTPS_PROXY="+rejectProxy.URL, "ALL_PROXY="+rejectProxy.URL,
				"NO_PROXY=127.0.0.1,localhost", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "TERM=dumb")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			started := time.Now()
			err := cmd.Run()
			server.Close()
			require.NoError(t, ctx.Err(), "CLI must finish within 20 seconds")
			require.NoError(t, err, "CLI failed: %s", stderr.String())
			var result struct {
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
			require.False(t, result.IsError)
			require.Contains(t, result.Result, "**Tokens:** 34 / 200k")
			require.Contains(t, result.Result, "| System prompt | 17 |")
			require.Contains(t, result.Result, "| Messages | 17 |")
			require.Zero(t, unexpectedRequests.Load(), "/context must not call /v1/messages or an unrelated local endpoint")
			mu.Lock()
			defer mu.Unlock()
			require.NotEmpty(t, exchanges, "CLI must use count_tokens, not a local estimate")
			for _, exchange := range exchanges {
				require.Equal(t, http.MethodPost, exchange.inbound.Method)
				require.Equal(t, "/v1/messages/count_tokens", exchange.inbound.Endpoint)
				require.Equal(t, "true", exchange.inboundBetaQuery)
				require.Contains(t, exchange.inbound.Headers["anthropic-beta"], "token-counting-2024-11-01")
				require.NotEmpty(t, exchange.inbound.CLIVersion)
				require.Equal(t, http.MethodPost, exchange.outbound.Method)
				require.Equal(t, exchange.inbound.Endpoint, exchange.outbound.Endpoint)
				require.Equal(t, "true", exchange.outboundBetaQuery)
				require.Equal(t, "upstream-anthropic-key", exchange.upstreamKey)
				require.Equal(t, http.StatusOK, exchange.status)
				require.Equal(t, "req_count_cli_contract", exchange.requestID)
			}
			t.Logf("mode=%s elapsed=%s count_tokens_requests=%d messages_requests=0 CLI rendered system=17 messages=17 total=34", mode, time.Since(started), len(exchanges))
		})
	}
}
