//go:build unit

package service

import (
	"context"
	"time"

	"bytes"
	coderws "github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestManagedDeploymentUnauthorizedRetainsControllerEventWithoutReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"http", "sse_nonstream", "sse_stream"} {
		for _, passthrough := range []bool{false, true} {
			name := mode + "/normalized"
			if passthrough {
				name = mode + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				account, repo, admin, identity := managedOAuthFixture()
				install := managedOAuthRequest(identity, 0, 1)
				_, err := admin.InstallManagedOAuthCredentials(t.Context(), account.ID, install)
				require.NoError(t, err)
				account.Extra["openai_passthrough"] = passthrough
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					require.Equal(t, "Bearer "+install.AccessToken, r.Header.Get("Authorization"))
					if mode == "http" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(401)
						_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"invalid_api_key","message":"Unauthorized"}}`)
					} else {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.failed\ndata: "+`{"type":"response.failed","response":{"error":{"type":"authentication_error","code":"invalid_api_key","message":"Unauthorized"}}}`+"\n\n")
					}
				}))
				defer server.Close()
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				body := []byte(`{"model":"gpt-5.5","stream":false,"instructions":"test","input":[{"type":"message","role":"user","content":"hello"}]}`)
				if mode == "sse_stream" {
					body = []byte(strings.ReplaceAll(string(body), `"stream":false`, `"stream":true`))
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				ctx := managedNoReplayContext(c, true)
				cfg := managedNoReplayConfig()
				gateway := &OpenAIGatewayService{accountRepo: repo, cfg: cfg, httpUpstream: &managedNoReplayHTTPUpstream{client: server.Client(), target: target}, openAITokenProvider: NewOpenAITokenProvider(repo, nil, nil)}
				_, err = gateway.Forward(ctx, c, account, body)
				require.Error(t, err)
				require.EqualValues(t, 1, calls.Load())
				require.Equal(t, 1, repo.events, "both HTTP and SSE 401 must notify toSub2 once")
				page, err := admin.ListManagedOAuthAuthEvents(ctx, account.ID, 0)
				require.NoError(t, err)
				require.Len(t, page.Events, 1)
				require.EqualValues(t, 1, page.Events[0].FailedTokenRevision)
				state, err := admin.GetManagedOAuthState(ctx, account.ID)
				require.NoError(t, err)
				require.True(t, state.AuthBlocked)
				require.False(t, state.Ready)
				require.False(t, account.Schedulable)
				require.Equal(t, "admin note", account.ErrorMessage)
			})
		}
	}
}

func TestManagedDeploymentUnauthorizedDoesNotRecoverCurrentHTTPOrWS(t *testing.T) {
	for _, transport := range []string{"http", "ws_pool", "ws_passthrough"} {
		for _, managed := range []bool{false, true} {
			mode := "ordinary"
			if managed {
				mode = "managed"
			}
			t.Run(transport+"/"+mode, func(t *testing.T) {
				account, repo, executor, provider := newOpenAIRecoveryFixture()
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer repaired-access" {
						http.Error(w, "expired", http.StatusUnauthorized)
						return
					}
					if transport == "http" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_repaired"}`)
						return
					}
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					_, _, err = conn.Read(r.Context())
					if err != nil {
						return
					}
					_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_repaired","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
					_, _, _ = conn.Read(r.Context())
				}))
				defer upstream.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if managed {
					ctx = WithManagedDeploymentSession(ctx, &ManagedDeploymentSession{DeploymentID: "no-replay", Binding: ManagedDeploymentBinding{AccountID: account.ID}})
				}
				cfg := managedNoReplayConfig()
				service := &OpenAIGatewayService{accountRepo: repo, cfg: cfg, openAITokenProvider: provider}
				switch transport {
				case "http":
					target, err := url.Parse(upstream.URL)
					require.NoError(t, err)
					service.httpUpstream = &managedNoReplayHTTPUpstream{client: upstream.Client(), target: target}
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+"/responses", strings.NewReader(`{"input":"hello"}`))
					require.NoError(t, err)
					request.Header.Set("Authorization", "Bearer rejected-access")
					response, err := service.doOpenAIUpstream(request, "", account)
					require.NoError(t, err)
					defer response.Body.Close()
					if managed {
						require.Equal(t, 401, response.StatusCode)
					} else {
						require.Equal(t, 200, response.StatusCode)
					}
				case "ws_pool":
					cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
					cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
					pool := newOpenAIWSConnPool(cfg)
					defer pool.Close()
					headers := make(http.Header)
					headers.Set("Authorization", "Bearer rejected-access")
					request := openAIWSAcquireRequest{Account: account, WSURL: "ws" + strings.TrimPrefix(upstream.URL, "http"), Headers: headers}
					lease, err := service.acquireOpenAIWSWithAuthRecovery(ctx, pool, &request)
					if managed {
						var dialErr *openAIWSDialError
						require.ErrorAs(t, err, &dialErr)
						require.Equal(t, 401, dialErr.StatusCode)
						require.Nil(t, lease)
					} else {
						require.NoError(t, err)
						lease.MarkBroken()
						lease.Release()
					}
				case "ws_passthrough":
					cfg.Gateway.OpenAIWS.Enabled = true
					cfg.Gateway.OpenAIWS.OAuthEnabled = true
					cfg.Gateway.OpenAIWS.APIKeyEnabled = true
					cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
					cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
					cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
					cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
					cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 2
					account.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}
					service.openaiWSResolver = NewOpenAIWSProtocolResolver(cfg)
					service.openaiWSPassthroughDialer = &openAIRecoveryLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http")}
					done := make(chan error, 1)
					gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := coderws.Accept(w, r, nil)
						if err != nil {
							done <- err
							return
						}
						defer conn.CloseNow()
						_, first, err := conn.Read(ctx)
						if err != nil {
							done <- err
							return
						}
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = r.WithContext(ctx)
						done <- service.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "rejected-access", first, nil)
					}))
					defer gateway.Close()
					conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
					require.NoError(t, err)
					defer conn.CloseNow()
					require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","instructions":"test","input":[{"type":"message","role":"user","content":"hello"}]}`)))
					_, _, readErr := conn.Read(ctx)
					if managed {
						require.Error(t, readErr)
					} else {
						require.NoError(t, readErr)
						_ = conn.Close(coderws.StatusNormalClosure, "done")
					}
					select {
					case err := <-done:
						if managed {
							require.Error(t, err)
						} else {
							require.NoError(t, err)
						}
					case <-ctx.Done():
						t.Fatal("passthrough did not stop")
					}
				}
				if managed {
					require.EqualValues(t, 1, calls.Load())
					require.Zero(t, executor.calls.Load(), "401 recovery belongs to the controller and must not resend the current request")
				} else {
					require.EqualValues(t, 2, calls.Load())
					require.EqualValues(t, 1, executor.calls.Load())
				}
			})
		}
	}
}
