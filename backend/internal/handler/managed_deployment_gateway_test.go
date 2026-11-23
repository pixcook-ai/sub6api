//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	middleware2 "github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func deploymentWriterFixture(t *testing.T, contentType string, record func(context.Context, []byte) error) (*managedDeploymentResponseWriter, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.Header().Set("Content-Type", contentType)
	return &managedDeploymentResponseWriter{ResponseWriter: c.Writer, ctx: context.Background(), record: record}, rec
}
func TestManagedDeploymentWriterPersistsSSEBeforeIDDelivery(t *testing.T) {
	var payloads []string
	var rec *httptest.ResponseRecorder
	w, r := deploymentWriterFixture(t, "text/event-stream", func(_ context.Context, raw []byte) error {
		require.NotContains(t, rec.Body.String(), "resp_created")
		payloads = append(payloads, string(raw))
		return nil
	})
	rec = r
	_, e := w.Write([]byte("event: response.created\ndata: {\"response\":{\"id\":\"resp_created\"}}\n"))
	require.NoError(t, e)
	require.Empty(t, rec.Body.String())
	w.Flush()
	require.Empty(t, rec.Body.String())
	_, e = w.Write([]byte("\n"))
	require.NoError(t, e)
	require.Len(t, payloads, 1)
	require.Contains(t, rec.Body.String(), "resp_created")
	require.NoError(t, w.finish())
}
func TestManagedDeploymentWriterPersistenceFailureDoesNotExposeResource(t *testing.T) {
	failure := errors.New("durable owner store unavailable")
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			w, rec := deploymentWriterFixture(t, contentType, func(context.Context, []byte) error { return failure })
			raw := `{"id":"resp_secret"}`
			if strings.Contains(contentType, "stream") {
				raw = "data: " + raw + "\n\n"
			}
			_, e := w.Write([]byte(raw))
			if strings.Contains(contentType, "stream") {
				require.ErrorIs(t, e, failure)
			} else {
				require.NoError(t, e)
			}
			require.ErrorIs(t, w.finish(), failure)
			require.Empty(t, rec.Body.String())
		})
	}
}
func TestManagedDeploymentWriterJSONAndCRLFEvents(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			ctype := "application/json"
			if mode == "sse" {
				ctype = "text/event-stream"
			}
			var rec *httptest.ResponseRecorder
			w, recorder := deploymentWriterFixture(t, ctype, func(_ context.Context, raw []byte) error {
				calls++
				require.JSONEq(t, `{"id":"resp_created"}`, string(raw))
				require.Empty(t, rec.Body.String())
				return nil
			})
			rec = recorder
			raw := `{"id":"resp_created"}`
			if mode == "sse" {
				raw = "data: " + raw + "\r\n\r\n"
			}
			for _, b := range []byte(raw) {
				_, e := w.Write([]byte{b})
				require.NoError(t, e)
			}
			require.NoError(t, w.finish())
			require.Equal(t, 1, calls)
			require.Equal(t, raw, rec.Body.String())
		})
	}
}
func TestManagedDeploymentWriterPassesBinaryDownloads(t *testing.T) {
	calls := 0
	w, rec := deploymentWriterFixture(t, "application/octet-stream", func(context.Context, []byte) error { calls++; return nil })
	data := []byte{0, 1, 2, 255}
	_, e := w.Write(data)
	require.NoError(t, e)
	require.NoError(t, w.finish())
	require.Equal(t, data, rec.Body.Bytes())
	require.Zero(t, calls)
}
func TestManagedDeploymentDisablesHTTPFailoverReplay(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/responses", nil)
	require.True(t, openAIRequestAllowsFailoverReplay(c))
	c.Request = c.Request.WithContext(service.WithManagedDeploymentSession(c.Request.Context(), &service.ManagedDeploymentSession{DeploymentID: "test"}))
	require.False(t, openAIRequestAllowsFailoverReplay(c))
	require.False(t, openAIForwardMayFailover(c, 0, &service.UpstreamFailoverError{SafeToFailoverAfterWrite: true}))
}

type deploymentGatewayRepo struct {
	service.AccountRepository
	service.ManagedDeploymentRepository
	mu        sync.Mutex
	d         service.ManagedDeployment
	sessions  map[string]*service.ManagedDeploymentSession
	resources map[string]*service.ManagedDeploymentSession
}

func gatewayDeploymentKey(group, user, key int64, id string) string {
	return fmt.Sprintf("%d/%d/%d/%s", group, user, key, id)
}
func (r *deploymentGatewayRepo) GetManagedDeploymentForGroup(_ context.Context, group int64) (*service.ManagedDeployment, error) {
	if group != 1 {
		return nil, nil
	}
	d := r.d
	return &d, nil
}
func (r *deploymentGatewayRepo) BindManagedDeploymentSession(_ context.Context, g, u, k int64, id string, _ bool) (*service.ManagedDeploymentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := gatewayDeploymentKey(g, u, k, id)
	if s := r.sessions[key]; s != nil {
		return s, nil
	}
	b := r.d.BPS
	if r.d.SelectedProvider == "codex" {
		b = r.d.Codex
	}
	s := &service.ManagedDeploymentSession{DeploymentID: r.d.DeploymentID, GroupID: g, UserID: u, APIKeyID: k, SessionKey: id, RouteRevision: r.d.RouteRevision, Binding: b}
	r.sessions[key] = s
	return s, nil
}
func (r *deploymentGatewayRepo) BindManagedDeploymentResource(_ context.Context, s *service.ManagedDeploymentSession, kind, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := gatewayDeploymentKey(s.GroupID, s.UserID, s.APIKeyID, kind+"/"+id)
	if old := r.resources[key]; old != nil && old.Binding != s.Binding {
		return service.ErrManagedDeploymentConflict
	}
	r.resources[key] = s
	return nil
}
func (r *deploymentGatewayRepo) ResolveManagedDeploymentResource(_ context.Context, g, u, k int64, kind, id string) (*service.ManagedDeploymentSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.resources[gatewayDeploymentKey(g, u, k, kind+"/"+id)]
	if s == nil {
		return nil, service.ErrManagedDeploymentContext
	}
	return s, nil
}
func deploymentGatewayFixture() (*OpenAIGatewayHandler, *deploymentGatewayRepo) {
	b := service.ManagedDeploymentBinding{Provider: "bps_excel", AccountID: 11, BackendID: "bps", BindingRef: "bind_11111111-1111-4111-8111-111111111111", ProfileRef: "oauth_22222222-2222-4222-8222-222222222222", IdentityEpoch: "33333333-3333-4333-8333-333333333333", Subject: "subject", WorkspaceID: "workspace"}
	c := b
	c.Provider = "codex"
	c.AccountID = 12
	c.BackendID = "codex"
	r := &deploymentGatewayRepo{d: service.ManagedDeployment{DeploymentID: "deployment_44444444-4444-4444-8444-444444444444", GroupID: 1, RouteRevision: 1, SelectedProvider: "bps_excel", BPS: b, Codex: c}, sessions: map[string]*service.ManagedDeploymentSession{}, resources: map[string]*service.ManagedDeploymentSession{}}
	s := service.NewOpenAIGatewayService(r, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	return &OpenAIGatewayHandler{gatewayService: s}, r
}
func TestManagedDeploymentGatewayRoutesNewAndOldSessionsWithDurableOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, repo := deploymentGatewayFixture()
	router := gin.New()
	key := int64(10)
	router.Use(func(c *gin.Context) {
		group := int64(1)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: key, GroupID: &group})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
	})
	router.Use(h.ManagedDeploymentRouting())
	calls := 0
	router.POST("/responses", func(c *gin.Context) {
		calls++
		s, ok := service.ManagedDeploymentSessionFromContext(c.Request.Context())
		require.True(t, ok)
		c.JSON(200, gin.H{"id": fmt.Sprintf("resp_%s_%d", s.Binding.Provider, calls), "provider": s.Binding.Provider})
	})
	request := func(session, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("session_id", session)
		}
		router.ServeHTTP(rec, req)
		return rec
	}
	first := request("old", `{"model":"model","input":"hello"}`)
	require.Equal(t, 200, first.Code, first.Body.String())
	require.Contains(t, first.Body.String(), "bps_excel")
	var result map[string]any
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &result))
	id := result["id"].(string)
	repo.d.SelectedProvider = "codex"
	repo.d.RouteRevision++
	newer := request("new", `{"model":"model","input":"new context"}`)
	require.Equal(t, 200, newer.Code, newer.Body.String())
	require.Contains(t, newer.Body.String(), "codex")
	old := request("old", `{"model":"model","input":"continue"}`)
	require.Equal(t, 200, old.Code, old.Body.String())
	require.Contains(t, old.Body.String(), "bps_excel")
	byOwner := request("", `{"model":"model","previous_response_id":"`+id+`"}`)
	require.Equal(t, 200, byOwner.Code, byOwner.Body.String())
	require.Contains(t, byOwner.Body.String(), "bps_excel")
	before := calls
	cross := request("new", `{"model":"model","previous_response_id":"`+id+`"}`)
	require.Equal(t, 409, cross.Code, cross.Body.String())
	key = 11
	stolen := request("", `{"previous_response_id":"`+id+`"}`)
	require.Equal(t, 409, stolen.Code, stolen.Body.String())
	require.Equal(t, before, calls, "untrusted requests must stop before dispatch")
}
func TestManagedDeploymentGatewayListingsCannotMintOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, repo := deploymentGatewayFixture()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		group := int64(1)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &group})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
	})
	router.Use(h.ManagedDeploymentRouting())
	router.GET("/files", func(c *gin.Context) { c.JSON(200, gin.H{"object": "list", "data": []gin.H{{"id": "file_unknown"}}}) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/files", nil)
	req.Header.Set("session_id", "listing")
	router.ServeHTTP(rec, req)
	require.Equal(t, 409, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "file_unknown")
	require.Empty(t, repo.resources)
}

func TestManagedDeploymentGatewayCannotHideStateWithContentType(t *testing.T) {
	for _, path := range []string{"/responses", "/chat/completions", "/messages", "/responses/compact"} {
		for _, ctype := range []string{"text/plain", "application/octet-stream", "multipart/form-data; boundary=fake"} {
			t.Run(path+"/"+ctype, func(t *testing.T) {
				h, _ := deploymentGatewayFixture()
				r := gin.New()
				r.Use(func(c *gin.Context) {
					group := int64(1)
					c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &group})
					c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
				})
				r.Use(h.ManagedDeploymentRouting())
				calls := 0
				r.POST(path, func(c *gin.Context) { calls++; c.JSON(200, gin.H{"ok": true}) })
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", path, strings.NewReader(`{"previous_response_id":"resp_unowned","file_id":"file_unowned","encrypted_content":"unknown","call_id":"call_unknown"}`))
				req.Header.Set("session_id", "fresh")
				req.Header.Set("Content-Type", ctype)
				r.ServeHTTP(rec, req)
				require.Equal(t, 409, rec.Code, rec.Body.String())
				require.Zero(t, calls, "declared Content-Type cannot bypass the durable ledger")
			})
		}
	}
}

func TestManagedDeploymentFakeUpgradeCannotBypassHTTPBinding(t *testing.T) {
	for _, path := range []string{"/responses", "/chat/completions"} {
		h, _ := deploymentGatewayFixture()
		r := gin.New()
		r.Use(func(c *gin.Context) {
			group := int64(1)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &group})
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
		})
		r.Use(h.ManagedDeploymentRouting())
		calls := 0
		r.POST(path, func(c *gin.Context) {
			s, ok := service.ManagedDeploymentSessionFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, "bps_excel", s.Binding.Provider)
			calls++
			c.JSON(200, gin.H{"ok": true})
		})
		for _, body := range []string{`{"input":"hello"}`, `{"previous_response_id":"resp_unknown"}`} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader(body))
			req.Header.Set("session_id", "session")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			r.ServeHTTP(rec, req)
			if strings.Contains(body, "previous_response_id") {
				require.Equal(t, 409, rec.Code)
			} else {
				require.Equal(t, 200, rec.Code, rec.Body.String())
			}
		}
		require.Equal(t, 1, calls)
	}
}
func TestManagedDeploymentWebSocketUpgradeMatchesOnlyActualRoute(t *testing.T) {
	for _, test := range []struct {
		method, path, connection string
		want                     bool
	}{{"GET", "/responses", "Upgrade", true}, {"GET", "/v1/responses", "keep-alive, Upgrade", true}, {"GET", "/backend-api/codex/responses", "Upgrade", true}, {"POST", "/responses", "Upgrade", false}, {"GET", "/files/responses", "Upgrade", false}, {"GET", "/responses", "", false}} {
		req := httptest.NewRequest(test.method, test.path, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", test.connection)
		require.Equal(t, test.want, managedDeploymentWebSocketUpgrade(req))
	}
}

func TestManagedDeploymentOfficialSessionHeaderAndConflicts(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		h, _ := deploymentGatewayFixture()
		r := gin.New()
		r.Use(func(c *gin.Context) {
			group := int64(1)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &group})
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
		})
		r.Use(h.ManagedDeploymentRouting())
		calls := 0
		r.POST("/responses", func(c *gin.Context) {
			calls++
			session, ok := service.ManagedDeploymentSessionFromContext(c.Request.Context())
			require.True(t, ok)
			require.Equal(t, service.ManagedDeploymentOwnerHash("official-session"), session.SessionKey)
			c.JSON(200, gin.H{"ok": true})
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"prompt_cache_key":"different-cache-key","input":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("session-id", "official-session")
		if conflict {
			req.Header.Set("session_id", "another-session")
		}
		r.ServeHTTP(rec, req)
		if conflict {
			require.Equal(t, 400, rec.Code)
			require.Zero(t, calls)
		} else {
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Equal(t, 1, calls)
		}
	}
}
func TestManagedDeploymentUnsupportedJobsNeverDispatch(t *testing.T) {
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstream.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	for _, path := range []string{"/images/generations/async", "/images/edits/async", "/images/tasks/existing", "/videos", "/videos/generations", "/live", "/backend-api/codex/realtime/calls", "/contents/generations/tasks", "/alpha/search"} {
		h, _ := deploymentGatewayFixture()
		r := gin.New()
		group := int64(1)
		r.Use(func(c *gin.Context) {
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 10, GroupID: &group})
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 20})
		})
		r.Use(h.ManagedDeploymentRouting())
		jobs := 0
		r.POST(path, func(c *gin.Context) {
			jobs++
			resp, e := http.Post(server.URL, "application/json", strings.NewReader("{}"))
			require.NoError(t, e)
			if resp != nil {
				resp.Body.Close()
			}
			c.Status(http.StatusAccepted)
		})
		request := func() *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"input":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("session-id", "session")
			r.ServeHTTP(rec, req)
			return rec
		}
		before := upstream.Load()
		rec := request()
		require.Equal(t, 501, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "managed_deployment_endpoint_unsupported")
		require.Zero(t, jobs, path)
		require.Equal(t, before, upstream.Load(), path)
		// Ordinary groups retain the existing endpoint and prove the loopback/count
		// fixture can actually create a job and dispatch upstream.
		group = 2
		ordinary := request()
		require.Equal(t, http.StatusAccepted, ordinary.Code, ordinary.Body.String())
		require.Equal(t, 1, jobs, path)
		require.Equal(t, before+1, upstream.Load(), path)
	}
}

func TestManagedDeploymentDiscoveryBypassMatchesOnlyDiscovery(t *testing.T) {
	for _, path := range []string{"/models", "/v1/models/gpt-5", "/backend-api/codex/models", "/openai/v1/models", "/v1/usage", "/v1/sub2api/billing"} {
		require.True(t, managedDeploymentDiscoveryRequest("GET", path), path)
	}
	for _, path := range []string{"/responses/models/input_items", "/files/models/file", "/videos/usage", "/responses/resp_id/billing", "/models/model/unknown"} {
		require.False(t, managedDeploymentDiscoveryRequest("GET", path), path)
	}
	require.False(t, managedDeploymentDiscoveryRequest("POST", "/models"))
}

func TestManagedDeploymentAllowsInitialAccountAttemptOnly(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/responses", nil)
	c.Request = c.Request.WithContext(service.WithManagedDeploymentSession(c.Request.Context(), &service.ManagedDeploymentSession{DeploymentID: "test"}))
	require.True(t, openAIRequestAllowsAccountAttempt(c, false))
	require.False(t, openAIRequestAllowsAccountAttempt(c, true))
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(ctx)
	require.False(t, openAIRequestAllowsAccountAttempt(c, false))
}
