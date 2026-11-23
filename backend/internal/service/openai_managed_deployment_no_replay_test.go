package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Keep the gateway's real sending and response loops, with network traffic
// constrained to a local server even when OAuth selects its fixed origin.
type managedNoReplayHTTPUpstream struct {
	client *http.Client
	target *url.URL
}

func (u *managedNoReplayHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target := *req.URL
	target.Scheme, target.Host = u.target.Scheme, u.target.Host
	clone.URL, clone.Host = &target, u.target.Host
	return u.client.Do(clone)
}
func (u *managedNoReplayHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}
func managedNoReplayContext(c *gin.Context, managed bool) context.Context {
	ctx := c.Request.Context()
	if managed {
		ctx = WithManagedDeploymentSession(ctx, &ManagedDeploymentSession{
			DeploymentID: "test-deployment", GroupID: 7, UserID: 8, APIKeyID: 9,
			SessionKey: "session-one", RouteRevision: 1,
			Binding: ManagedDeploymentBinding{Provider: "bps", AccountID: 91, BackendID: "test-worker"},
		})
		c.Request = c.Request.WithContext(ctx)
	}
	return ctx
}
func managedNoReplayConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAICompactModel = "gpt-5.4"
	return cfg
}

func TestManagedDeploymentHTTPNeverReplaysDispatchedInference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const encryptedBody = `{"model":"gpt-5.1","stream":false,"instructions":"test","previous_response_id":"resp_original","input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[{"type":"summary_text","text":"keep me"}]},{"type":"item_reference","id":"item_original"},{"type":"input_text","text":"hello"}]}`
	const rejectedBody = `{"model":"gpt-5.5","stream":false,"instructions":"test","max_output_tokens":4096,"input":[{"type":"message","role":"user","content":"hello"}]}`
	const compactBody = `{"model":"gpt-5.5","stream":false,"instructions":"test","input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`
	scenarios := []struct {
		name                 string
		body                 string
		status               int
		failure              string
		compact, sse, stream bool
		passthrough          bool
		ordinaryAttempts     int
	}{
		{name: "invalid_encrypted_content", body: encryptedBody, status: 400, failure: `{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content could not be verified."}}`, ordinaryAttempts: 2},
		{name: "rejected_field", body: rejectedBody, status: 400, failure: `{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`, ordinaryAttempts: 2},
		{name: "compact_http", body: compactBody, status: 400, failure: `{"error":{"code":"context_length_exceeded","message":"context window exceeded"}}`, compact: true, ordinaryAttempts: 2},
		{name: "compact_sse_nonstream", body: compactBody, status: 200, failure: `{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"context window exceeded"}}}`, compact: true, sse: true, ordinaryAttempts: 2},
		{name: "compact_sse_stream", body: compactBody, status: 200, failure: `{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"context window exceeded"}}}`, compact: true, sse: true, stream: true, ordinaryAttempts: 2},
		{name: "passthrough_rejected_field", body: rejectedBody, status: 400, failure: `{"error":{"code":"unsupported_parameter","param":"max_output_tokens","message":"Unsupported parameter: max_output_tokens"}}`, passthrough: true, ordinaryAttempts: 2},
		{name: "passthrough_compact_http", body: compactBody, status: 400, failure: `{"error":{"code":"context_length_exceeded","message":"context window exceeded"}}`, passthrough: true, compact: true, ordinaryAttempts: 2},
		{name: "passthrough_compact_sse_nonstream", body: compactBody, status: 200, failure: `{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"context window exceeded"}}}`, passthrough: true, compact: true, sse: true, ordinaryAttempts: 2},
		{name: "passthrough_compact_sse_stream", body: compactBody, status: 200, failure: `{"type":"response.failed","response":{"status":"failed","error":{"code":"context_length_exceeded","message":"context window exceeded"}}}`, passthrough: true, compact: true, sse: true, stream: true, ordinaryAttempts: 2},
		{name: "unauthorized", body: encryptedBody, status: 401, failure: `{"error":{"code":"invalid_api_key","message":"authorization expired"}}`, ordinaryAttempts: 1},
	}
	for _, scenario := range scenarios {
		for _, managed := range []bool{false, true} {
			mode := "ordinary"
			if managed {
				mode = "managed"
			}
			t.Run(scenario.name+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				var mu sync.Mutex
				var bodies [][]byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, readErr := io.ReadAll(r.Body)
					if readErr != nil {
						t.Error(readErr)
					}
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
					attempt := calls.Add(1)
					if attempt == 1 {
						if scenario.sse {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(scenario.status)
							_, _ = io.WriteString(w, "event: response.failed\ndata: "+scenario.failure+"\n\n")
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(scenario.status)
							_, _ = io.WriteString(w, scenario.failure)
						}
						return
					}
					const success = `{"id":"resp_success","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
					if scenario.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":`+success+"}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, success)
					}
				}))
				defer server.Close()
				origin, parseErr := url.Parse(server.URL)
				require.NoError(t, parseErr)
				body := []byte(scenario.body)
				if scenario.stream {
					body = []byte(strings.ReplaceAll(scenario.body, `"stream":false`, `"stream":true`))
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				if scenario.compact {
					MarkOpenAINativeCompactionV2(c)
				}
				ctx := managedNoReplayContext(c, managed)
				cfg := managedNoReplayConfig()
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &managedNoReplayHTTPUpstream{client: server.Client(), target: origin}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg)}
				account := &Account{ID: 91, Name: "loopback-account", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Status: StatusActive, Schedulable: true,
					Credentials: map[string]any{"api_key": "test-key", "base_url": server.URL}, Extra: map[string]any{"openai_passthrough": scenario.passthrough}}
				result, forwardErr := svc.Forward(ctx, c, account, body)
				wantCalls := scenario.ordinaryAttempts
				if managed {
					wantCalls = 1
				}
				require.Equal(t, int32(wantCalls), calls.Load(), "an accepted inference must not be retried for a managed deployment")
				mu.Lock()
				requests := append([][]byte(nil), bodies...)
				mu.Unlock()
				require.Len(t, requests, wantCalls)
				if scenario.name == "invalid_encrypted_content" || scenario.name == "unauthorized" {
					require.Equal(t, "resp_original", gjson.GetBytes(requests[0], "previous_response_id").String())
					require.Equal(t, "gAAA", gjson.GetBytes(requests[0], "input.0.encrypted_content").String())
					require.Equal(t, "item_original", gjson.GetBytes(requests[0], "input.1.id").String())
				}
				if managed {
					require.Error(t, forwardErr)
					require.Nil(t, result)
					if scenario.compact {
						require.Equal(t, "gpt-5.5", gjson.GetBytes(requests[0], "model").String())
					}
					if events, ok := c.Get(OpsUpstreamErrorsKey); ok {
						for _, event := range events.([]*OpsUpstreamErrorEvent) {
							require.NotEqual(t, "compact_model_fallback", event.Reason)
						}
					}
				} else if wantCalls > 1 {
					require.NoError(t, forwardErr)
					require.NotNil(t, result)
					if scenario.compact {
						require.Equal(t, "gpt-5.4", gjson.GetBytes(requests[1], "model").String())
					}
					if strings.Contains(scenario.name, "rejected_field") {
						require.False(t, gjson.GetBytes(requests[1], "max_output_tokens").Exists())
					}
					if scenario.name == "invalid_encrypted_content" {
						require.False(t, gjson.GetBytes(requests[1], "input.0.encrypted_content").Exists())
					}
				}
			})
		}
	}
}

func TestManagedDeploymentWebSocketNeverReplaysDispatchedInference(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []string{"previous_response_not_found", "invalid_encrypted_content", "disconnect"} {
		for _, managed := range []bool{false, true} {
			mode := "ordinary"
			if managed {
				mode = "managed"
			}
			t.Run(failure+"/"+mode, func(t *testing.T) {
				var calls, httpCalls atomic.Int32
				var mu sync.Mutex
				var payloads [][]byte
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						http.Error(w, "unexpected HTTP fallback", 500)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, payload, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					payloads = append(payloads, payload)
					mu.Unlock()
					if calls.Add(1) == 1 {
						if failure != "disconnect" {
							_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"code": failure, "type": "invalid_request_error", "message": strings.ReplaceAll(failure, "_", " ")}})
						}
						return
					}
					_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_ws_success", "model": "gpt-5.3-codex", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}})
				}))
				defer server.Close()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
				c.Request.Header.Set("User-Agent", "custom-client/1.0")
				ctx, cancel := context.WithTimeout(managedNoReplayContext(c, managed), 5*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				cfg := managedNoReplayConfig()
				cfg.Gateway.OpenAIWS.Enabled = true
				cfg.Gateway.OpenAIWS.OAuthEnabled = true
				cfg.Gateway.OpenAIWS.APIKeyEnabled = true
				cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
				cfg.Gateway.OpenAIWS.FallbackCooldownSeconds = 1
				origin, err := url.Parse(server.URL)
				require.NoError(t, err)
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &managedNoReplayHTTPUpstream{client: server.Client(), target: origin}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 91, Name: "loopback-ws", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "test-key", "base_url": server.URL}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
				body := []byte(`{"model":"gpt-5.3-codex","stream":false,"previous_response_id":"resp_original","input":[{"type":"reasoning","encrypted_content":"gAAA","summary":[{"type":"summary_text","text":"keep me"}]},{"type":"item_reference","id":"item_original"},{"type":"input_text","text":"hello"}]}`)
				result, forwardErr := svc.Forward(ctx, c, account, body)
				require.Zero(t, httpCalls.Load(), "a managed failure must never switch transport/provider")
				mu.Lock()
				requests := append([][]byte(nil), payloads...)
				mu.Unlock()
				wantCalls := 2
				if managed {
					wantCalls = 1
				}
				require.Equal(t, int32(wantCalls), calls.Load())
				require.Len(t, requests, wantCalls)
				require.Equal(t, "resp_original", gjson.GetBytes(requests[0], "previous_response_id").String())
				require.Equal(t, "gAAA", gjson.GetBytes(requests[0], "input.0.encrypted_content").String())
				require.Equal(t, "item_original", gjson.GetBytes(requests[0], "input.1.id").String())
				if managed {
					require.Error(t, forwardErr)
					require.Nil(t, result)
				} else {
					require.NoError(t, forwardErr)
					require.NotNil(t, result)
					if failure != "disconnect" {
						require.False(t, gjson.GetBytes(requests[1], "previous_response_id").Exists())
					}
					if failure == "invalid_encrypted_content" {
						require.False(t, gjson.GetBytes(requests[1], "input.0.encrypted_content").Exists())
					}
				}
			})
		}
	}
}
