//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// This opt-in contract runs an isolated real CLI through GatewayHandler.Messages
// and GatewayService with synthetic credentials and an in-memory upstream. It
// does not verify production authentication, OAuth, or telemetry delivery.
func TestClaudeCLIConsumesMessagesHandlerHTTPError(t *testing.T) {
	binary := os.Getenv("CLAUDE_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("set CLAUDE_CLI_TEST_BINARY to run the local real CLI error contract")
	}
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		status    int
		errorType string
		retry     bool
	}{
		{"rate_limit_429", http.StatusTooManyRequests, "rate_limit_error", false},
		{"overloaded_529", 529, "overloaded_error", false},
		{"rate_limit_429_retry_then_success", http.StatusTooManyRequests, "rate_limit_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var requests, upstreamCalls atomic.Int64
			_, handler := newAnthropicMessagesErrorGatewayFixture(t, nil, 1, func(_ *http.Request, _ int64) *http.Response {
				upstreamCalls.Add(1)
				if requests.Load() == 1 {
					return &http.Response{
						StatusCode: tc.status,
						Header: http.Header{
							"Content-Type":   {"application/json"},
							"Request-Id":     {"req_cli_http_error"},
							"Retry-After":    {"1"},
							"X-Should-Retry": {fmt.Sprint(tc.retry)},
						},
						Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"Synthetic contract failure"},"request_id":"req_cli_http_error"}`, tc.errorType))),
					}
				}
				// Complete an allowed retry promptly. An unexpected retry in the
				// false cases also finishes here, then fails the request-count check.
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(claudeCLIHTTPErrorRecoveryStream)),
				}
			})
			var mu sync.Mutex
			var firstStatus int
			var firstHeaders http.Header
			var arrivals [2]time.Time
			var firstResponseAt time.Time
			router := gin.New()
			router.POST("/v1/messages", func(c *gin.Context) {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("real Messages handler panicked: %v", recovered)
						cancel()
					}
				}()
				n := requests.Add(1)
				arrived := time.Now()
				if n <= int64(len(arrivals)) {
					mu.Lock()
					arrivals[n-1] = arrived
					mu.Unlock()
				}
				handler(c)
				if n == 1 {
					mu.Lock()
					firstStatus, firstHeaders = c.Writer.Status(), c.Writer.Header().Clone()
					firstResponseAt = time.Now()
					mu.Unlock()
				}
			})
			server := httptest.NewServer(router)
			defer server.Close()
			rejectProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer rejectProxy.Close()

			cmd := exec.CommandContext(ctx, binary,
				"--bare", "--setting-sources", "", "--strict-mcp-config",
				"--mcp-config", `{"mcpServers":{}}`, "--tools", "", "--allowedTools", "",
				"--permission-mode", "dontAsk", "--no-session-persistence",
				"--model", "claude-sonnet-4-5-20250929", "--output-format", "json",
				"--system-prompt", "Reply briefly.", "-p", "Return the local protocol fixture response.")
			cmd.Dir = t.TempDir()
			cmd.WaitDelay = 2 * time.Second
			for _, key := range []string{"PATH", "HOME", "TMPDIR", "SystemRoot"} {
				if value, exists := os.LookupEnv(key); exists {
					cmd.Env = append(cmd.Env, key+"="+value)
				}
			}
			cmd.Env = append(cmd.Env,
				"CLAUDE_CONFIG_DIR="+t.TempDir(), "ANTHROPIC_API_KEY=local-contract-key",
				"ANTHROPIC_BASE_URL="+server.URL,
				"HTTP_PROXY="+rejectProxy.URL, "HTTPS_PROXY="+rejectProxy.URL, "ALL_PROXY="+rejectProxy.URL,
				"NO_PROXY=127.0.0.1,localhost", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
				"DISABLE_AUTOUPDATER=1", "TERM=dumb")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			started := time.Now()
			runErr := cmd.Run()
			server.Close()
			require.NoError(t, ctx.Err(), "CLI must finish within 45 seconds: %s", stderr.String())
			var result struct {
				Result         string `json:"result"`
				IsError        bool   `json:"is_error"`
				APIErrorStatus *int   `json:"api_error_status"`
			}
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &result), "CLI error=%v stderr=%s", runErr, stderr.String())
			apiStatus := 0
			if result.APIErrorStatus != nil {
				apiStatus = *result.APIErrorStatus
			}
			t.Logf("status=%d elapsed=%s gateway_requests=%d upstream_calls=%d process_error=%v is_error=%v api_error_status=%v result=%q",
				tc.status, time.Since(started), requests.Load(), upstreamCalls.Load(), runErr, result.IsError, apiStatus, result.Result)
			if tc.retry {
				require.NoError(t, runErr, "CLI failed: %s", stderr.String())
				require.False(t, result.IsError)
				require.Zero(t, apiStatus)
				require.Equal(t, "CLAUDE_GATEWAY_CONTRACT_OK", result.Result)
				require.Equal(t, int64(2), requests.Load(), "CLI must retry the first 429 and consume the successful stream")
				require.Equal(t, int64(2), upstreamCalls.Load())
			} else {
				var exitError *exec.ExitError
				require.ErrorAs(t, runErr, &exitError)
				require.Equal(t, 1, exitError.ExitCode())
				require.True(t, result.IsError)
				require.Equal(t, tc.status, apiStatus, "CLI must recognize the actual HTTP error status")
				require.Equal(t, int64(1), requests.Load(), "CLI must honor x-should-retry:false")
				require.Equal(t, int64(1), upstreamCalls.Load(), "the single eligible account must produce the terminal error")
				require.NotContains(t, result.Result, "CLAUDE_GATEWAY_CONTRACT_OK")
			}
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, tc.status, firstStatus)
			require.Equal(t, "req_cli_http_error", firstHeaders.Get("Request-Id"))
			require.Equal(t, "1", firstHeaders.Get("Retry-After"))
			require.Equal(t, fmt.Sprint(tc.retry), firstHeaders.Get("X-Should-Retry"))
			if tc.retry {
				arrivalInterval := arrivals[1].Sub(arrivals[0])
				retryDelay := arrivals[1].Sub(firstResponseAt)
				t.Logf("retry arrival_interval=%s delay_after_response=%s", arrivalInterval, retryDelay)
				require.GreaterOrEqual(t, arrivalInterval, 900*time.Millisecond)
				require.GreaterOrEqual(t, retryDelay, 900*time.Millisecond, "CLI must respect Retry-After:1 with timing tolerance")
			}
		})
	}
}

const claudeCLIHTTPErrorRecoveryStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_contract_recovery","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"CLAUDE_GATEWAY_CONTRACT_OK"}}` + "\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
