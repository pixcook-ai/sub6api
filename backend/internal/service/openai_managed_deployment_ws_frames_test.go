//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

var errManagedWSRecordTest = errors.New("test ownership store unavailable: broken pipe")

type managedWSRecordRepo struct {
	*deploymentTestRepo
	fail  bool
	calls atomic.Int32
}

func (r *managedWSRecordRepo) BindManagedDeploymentResource(ctx context.Context, session *ManagedDeploymentSession, kind, id string) error {
	r.calls.Add(1)
	if r.fail {
		return errManagedWSRecordTest
	}
	return r.deploymentTestRepo.BindManagedDeploymentResource(ctx, session, kind, id)
}

func TestManagedDeploymentWebSocketFramesEnforceResourceOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		for _, managed := range []bool{false, true} {
			for _, recordFailure := range []bool{false, true} {
				title := mode + "/ordinary"
				if managed {
					title = mode + "/managed"
				}
				if recordFailure {
					title += "/record_failure"
				} else {
					title += "/foreign_followup"
				}
				t.Run(title, func(t *testing.T) {
					service, baseRepo := deploymentFixture()
					session, err := baseRepo.BindManagedDeploymentSession(t.Context(), 1, 2, 3, "ws-session", false)
					require.NoError(t, err)
					foreign := *session
					foreign.Binding = baseRepo.deployment.Codex
					require.NoError(t, baseRepo.BindManagedDeploymentResource(t.Context(), &foreign, "response", "resp_foreign"))
					repo := &managedWSRecordRepo{deploymentTestRepo: baseRepo, fail: recordFailure}
					service.accountRepo = repo
					var upstreamCalls atomic.Int32
					var mu sync.Mutex
					var requests [][]byte
					capture := func(raw []byte) string {
						mu.Lock()
						requests = append(requests, append([]byte(nil), raw...))
						mu.Unlock()
						number := upstreamCalls.Add(1)
						return fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_owned_%d","model":"gpt-5.1","status":"completed","output":[{"type":"reasoning","id":"rs_owned","encrypted_content":"opaque-owned","summary":[]}],"usage":{"input_tokens":1,"output_tokens":1}}}`, number)
					}
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if mode == OpenAIWSIngressModeHTTPBridge {
							raw, readErr := io.ReadAll(r.Body)
							if readErr != nil {
								t.Error(readErr)
								return
							}
							payload := capture(raw)
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "event: response.completed\ndata: "+payload+"\n\n")
							return
						}
						conn, acceptErr := upgrader.Upgrade(w, r, nil)
						if acceptErr != nil {
							t.Error(acceptErr)
							return
						}
						defer conn.Close()
						for {
							_, raw, readErr := conn.ReadMessage()
							if readErr != nil {
								return
							}
							if writeErr := conn.WriteMessage(websocket.TextMessage, []byte(capture(raw))); writeErr != nil {
								return
							}
						}
					}))
					defer upstream.Close()
					cfg := managedNoReplayConfig()
					cfg.Gateway.OpenAIWS.Enabled = true
					cfg.Gateway.OpenAIWS.OAuthEnabled = true
					cfg.Gateway.OpenAIWS.APIKeyEnabled = true
					cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
					cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
					cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
					cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
					cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
					cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
					cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
					cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
					cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
					service.cfg = cfg
					service.cache = &stubGatewayCache{}
					service.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
					service.toolCorrector = NewCodexToolCorrector()
					origin, err := url.Parse(upstream.URL)
					require.NoError(t, err)
					service.httpUpstream = &managedNoReplayHTTPUpstream{client: upstream.Client(), target: origin}
					service.openaiWSPool = newOpenAIWSConnPool(cfg)
					defer service.openaiWSPool.Close()
					account := baseRepo.accounts[11]
					account.Concurrency = 1
					account.Credentials = map[string]any{"api_key": "loopback-key", "base_url": upstream.URL}
					account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
					serverErr := make(chan error, 1)
					gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, acceptErr := coderws.Accept(w, r, nil)
						if acceptErr != nil {
							serverErr <- acceptErr
							return
						}
						defer conn.CloseNow()
						_, first, readErr := conn.Read(r.Context())
						if readErr != nil {
							serverErr <- readErr
							return
						}
						ctx := r.Context()
						if managed {
							ctx = WithManagedDeploymentSession(ctx, session)
						}
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = r.WithContext(ctx)
						proxyErr := service.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "loopback-key", first, nil)
						if proxyErr != nil {
							var closeErr *OpenAIWSClientCloseError
							if errors.As(proxyErr, &closeErr) {
								_ = conn.Close(closeErr.StatusCode(), "managed ownership failure")
							} else {
								_ = conn.Close(coderws.StatusInternalError, "gateway stopped")
							}
						}
						serverErr <- proxyErr
					}))
					defer gateway.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
					defer cancel()
					conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
					require.NoError(t, err)
					defer conn.CloseNow()
					require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","client_metadata":{"session_id":""},"input":[{"type":"input_text","text":"hello"}]}`)))
					_, first, readErr := conn.Read(ctx)
					if managed && recordFailure {
						require.Error(t, readErr, "ownership persistence failure must not expose the first native frame")
						require.Empty(t, first)
						require.EqualValues(t, 1, upstreamCalls.Load())
						select {
						case proxyErr := <-serverErr:
							require.ErrorIs(t, proxyErr, errManagedWSRecordTest)
						case <-ctx.Done():
							t.Fatal("gateway did not stop after ownership failure")
						}
						return
					}
					require.NoError(t, readErr)
					require.Equal(t, "resp_owned_1", gjson.GetBytes(first, "response.id").String())
					if managed {
						require.NoError(t, service.ValidateManagedDeploymentPayload(WithManagedDeploymentSession(t.Context(), session), []byte(`{"previous_response_id":"resp_owned_1","input":[{"type":"item_reference","id":"rs_owned"},{"type":"reasoning","encrypted_content":"opaque-owned"}]}`)), "every delivered response/item/encrypted reference must already have an owner")
					} else {
						require.Zero(t, repo.calls.Load(), "ordinary traffic must not require or mint managed ownership")
					}
					require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","client_metadata":{"session_id":""},"previous_response_id":"resp_owned_1","input":[{"type":"item_reference","id":"rs_owned"},{"type":"reasoning","encrypted_content":"opaque-owned"},{"type":"input_text","text":"continue"}]}`)))
					_, second, readErr := conn.Read(ctx)
					require.NoError(t, readErr)
					require.Equal(t, "resp_owned_2", gjson.GetBytes(second, "response.id").String())
					mu.Lock()
					secondRequest := append([]byte(nil), requests[1]...)
					mu.Unlock()
					if managed {
						expectedAffinity, _ := ScopeManagedDeploymentAffinity(WithManagedDeploymentSession(t.Context(), session), "")
						require.Equal(t, expectedAffinity, gjson.GetBytes(secondRequest, "prompt_cache_key").String(), "all actual WS sends must scope even an absent affinity key")
						require.Equal(t, expectedAffinity, gjson.GetBytes(secondRequest, "client_metadata.session_id").String())
						require.Equal(t, "resp_owned_1", gjson.GetBytes(secondRequest, "previous_response_id").String(), "managed bridges must preserve the native continuation anchor")
						require.Equal(t, "rs_owned", gjson.GetBytes(secondRequest, "input.0.id").String())
						require.Equal(t, "opaque-owned", gjson.GetBytes(secondRequest, "input.1.encrypted_content").String())
					}
					require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_foreign","input":[{"type":"input_text","text":"wrong owner"}]}`)))
					_, third, readErr := conn.Read(ctx)
					if managed {
						require.Error(t, readErr)
						require.Empty(t, third)
						require.EqualValues(t, 2, upstreamCalls.Load(), "a later frame owned by Codex must never reach this BPS binding")
						select {
						case proxyErr := <-serverErr:
							require.ErrorIs(t, proxyErr, ErrManagedDeploymentContext)
						case <-ctx.Done():
							t.Fatal("gateway did not reject foreign followup")
						}
					} else {
						require.NoError(t, readErr)
						require.Equal(t, "resp_owned_3", gjson.GetBytes(third, "response.id").String())
						require.EqualValues(t, 3, upstreamCalls.Load())
						require.Zero(t, repo.calls.Load())
						_ = conn.Close(coderws.StatusNormalClosure, "done")
						select {
						case <-serverErr:
						case <-ctx.Done():
							t.Fatal("ordinary connection did not stop")
						}
					}
				})
			}
		}
	}
}
