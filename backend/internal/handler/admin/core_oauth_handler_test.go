package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func coreOAuthHandlerRequest(t *testing.T, actor int64, role, body string, handle func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/core-oauth/profiles/example/complete", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "ref", Value: "example"}}
	if actor != 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: actor})
	}
	if role != "" {
		c.Set(string(middleware.ContextKeyUserRole), role)
	}
	handle(c)
	return w
}

type coreOAuthHandlerSpy struct {
	coreOAuthAdminService
	completeInput   service.CoreOAuthCompleteInput
	completeCalls   int
	startActor      int64
	cancelActor     int64
	refreshRevision int64
}

type coreOAuthRuntimeShellAdminStub struct{ *stubAdminService }

func (s *coreOAuthRuntimeShellAdminStub) ListCoreOAuthRuntimeShells(_ context.Context, groupID int64) ([]service.Account, error) {
	if groupID != 2 {
		return nil, service.ErrCoreOAuthInvalidInput
	}
	return []service.Account{{ID: 17, Name: "Sub6 Excel Runtime", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, GroupIDs: []int64{2}, Schedulable: false, Status: service.StatusActive, Credentials: map[string]any{"api_key": "must-not-escape"}, Extra: map[string]any{service.CoreOAuthRuntimeShellExtraKey: map[string]any{"provider": service.CoreOAuthProviderExcel, "group_id": int64(2)}, "must_not_escape": "private"}}}, nil
}

func TestCoreRuntimeShellRecoveryReturnsOnlySafePausedSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := &coreOAuthRuntimeShellAdminStub{stubAdminService: newStubAdminService()}
	h := NewAccountHandler(adminService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/core-oauth/runtime-shells?group_id=2", nil)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
	c.Set(string(middleware.ContextKeyUserRole), "admin")
	h.ListCoreRuntimeShells(c)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"accounts":[{"id":17,"provider":"bps_excel","name":"Sub6 Excel Runtime","schedulable":false,"status":"active"}]}}`, w.Body.String())
	require.NotContains(t, w.Body.String(), "must-not-escape")
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestCoreRuntimeShellRecoveryRequiresAdminAndGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAccountHandler(newStubAdminService(), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, tc := range []struct {
		actor int64
		role  string
		query string
		want  int
	}{{0, "admin", "?group_id=2", http.StatusUnauthorized}, {7, "user", "?group_id=2", http.StatusForbidden}, {7, "admin", "", http.StatusBadRequest}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/core-oauth/runtime-shells"+tc.query, nil)
		if tc.actor > 0 {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: tc.actor})
		}
		c.Set(string(middleware.ContextKeyUserRole), tc.role)
		h.ListCoreRuntimeShells(c)
		require.Equal(t, tc.want, w.Code)
	}
}

func (s *coreOAuthHandlerSpy) CompleteAuthorization(_ context.Context, _ string, in service.CoreOAuthCompleteInput) (*service.CoreOAuthProfile, error) {
	s.completeInput = in
	s.completeCalls++
	return &service.CoreOAuthProfile{ProfileRef: "example", Provider: "bps_excel", Status: "authorized", Authorized: true}, nil
}
func (s *coreOAuthHandlerSpy) StartAuthorization(_ context.Context, _ string, actor int64) (*service.CoreOAuthAuthorization, error) {
	s.startActor = actor
	return &service.CoreOAuthAuthorization{AttemptID: "attempt"}, nil
}
func (s *coreOAuthHandlerSpy) CancelAuthorization(_ context.Context, _, _ string, actor int64) (*service.CoreOAuthProfile, error) {
	s.cancelActor = actor
	return &service.CoreOAuthProfile{ProfileRef: "example"}, nil
}
func (s *coreOAuthHandlerSpy) Refresh(_ context.Context, _ string, rev int64) (*service.CoreOAuthProfile, error) {
	s.refreshRevision = rev
	return &service.CoreOAuthProfile{ProfileRef: "example", TokenRevision: rev + 1}, nil
}

func TestCoreOAuthHandlerBindsAttemptsToMiddlewareActorAndKeepsCallbackPrivate(t *testing.T) {
	spy := &coreOAuthHandlerSpy{}
	h := &CoreOAuthHandler{core: spy}
	callback := "https://example.invalid/callback?code=fixture-private-code&state=fixture-private-state"
	w := coreOAuthHandlerRequest(t, 17, "admin", `{"attempt_id":"attempt","callback_url":"`+callback+`"}`, h.Complete)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(17), spy.completeInput.ActorID)
	require.Equal(t, callback, spy.completeInput.CallbackURL)
	require.NotContains(t, w.Body.String(), "fixture-private")
	require.NotContains(t, w.Body.String(), "callback_url")
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	w = coreOAuthHandlerRequest(t, 17, "admin", `{}`, h.Authorize)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(17), spy.startActor)
	w = coreOAuthHandlerRequest(t, 17, "admin", `{"attempt_id":"attempt"}`, h.Cancel)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(17), spy.cancelActor)
	w = coreOAuthHandlerRequest(t, 17, "admin", `{"expected_revision":4}`, h.Refresh)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(4), spy.refreshRevision)
}

func TestCoreOAuthHandlerInvalidCompletionNeverReachesProvider(t *testing.T) {
	spy := &coreOAuthHandlerSpy{}
	h := &CoreOAuthHandler{core: spy}
	for _, body := range []string{
		`{"attempt_id":"attempt","callback_url":"https://example.invalid/cb","actor_id":99}`,
		`{"attempt_id":"","callback_url":"https://example.invalid/cb"}`,
		`{"attempt_id":"attempt","callback_url":"` + strings.Repeat("x", 16385) + `"}`,
		`{"attempt_id":"attempt","callback_url":"one","callback_url":"two"}`,
	} {
		w := coreOAuthHandlerRequest(t, 17, "admin", body, h.Complete)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Contains(t, w.Body.String(), "CORE_OAUTH_INVALID_INPUT")
	}
	require.Zero(t, spy.completeCalls)
	for _, body := range []string{`{}`, `{"expected_revision":0}`, `{"expected_revision":-1}`, `{"expected_revision":1.5}`} {
		w := coreOAuthHandlerRequest(t, 17, "admin", body, h.Refresh)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
	require.Zero(t, spy.refreshRevision)
}

func TestCoreOAuthHandlerUnavailableCapabilityDoesNotBreakServer(t *testing.T) {
	h := NewCoreOAuthHandler(nil)
	w := coreOAuthHandlerRequest(t, 7, "admin", "", h.Capabilities)
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"code":0,"message":"success","data":{"available":false,"reason":"stable_encryption_key_required","providers":[{"provider":"codex","available":false,"requires_callback_url":true},{"provider":"bps_excel","available":false,"requires_callback_url":true}]}}`, w.Body.String())
	w = coreOAuthHandlerRequest(t, 7, "admin", `{}`, h.ListProfiles)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "CORE_OAUTH_UNAVAILABLE")
}

func TestCoreOAuthHandlerRequiresTrustedAdminActor(t *testing.T) {
	h := NewCoreOAuthHandler(nil)
	for _, method := range []func(*gin.Context){h.Capabilities, h.CreateProfile, h.ListProfiles, h.GetProfile, h.Authorize, h.Complete, h.Cancel, h.Refresh} {
		w := coreOAuthHandlerRequest(t, 0, "admin", `{"actor_id":7}`, method)
		require.Equal(t, http.StatusUnauthorized, w.Code)
		w = coreOAuthHandlerRequest(t, 7, "user", `{}`, method)
		require.Equal(t, http.StatusForbidden, w.Code)
	}
}

func TestCoreOAuthStrictBodyRejectsAmbiguousAndOversizedJSON(t *testing.T) {
	for _, body := range []string{
		`null`, `[]`, `{} {}`, `{"attempt_id":"one","attempt_id":"two"}`,
		`{"attempt_id":"one","actor_id":8}`, `{"callback_url":`,
		`{"callback_url":"` + strings.Repeat("x", (32<<10)+1) + `"}`,
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		var in struct {
			AttemptID   string `json:"attempt_id"`
			CallbackURL string `json:"callback_url"`
		}
		require.False(t, decodeCoreOAuthBody(c, &in), "body must be rejected")
		require.NotContains(t, w.Body.String(), body)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"attempt_id":"one","callback_url":"https://example.invalid/callback"}`))
	var in struct {
		AttemptID   string `json:"attempt_id"`
		CallbackURL string `json:"callback_url"`
	}
	require.True(t, decodeCoreOAuthBody(c, &in))
	require.Equal(t, "one", in.AttemptID)
}

func TestCoreRuntimeShellCreatesOnlyPausedProviderSpecificAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name      string
		body      string
		wantType  string
		wantCreds map[string]any
	}{
		{name: "codex", body: `{"provider":"codex","group_id":2}`, wantType: service.AccountTypeOAuth, wantCreds: map[string]any{}},
		{name: "excel", body: `{"provider":"bps_excel","group_id":2,"base_url":"https://worker.example/v1","api_key":"inference-key"}`, wantType: service.AccountTypeAPIKey, wantCreds: map[string]any{"base_url": "https://worker.example/v1", "api_key": "inference-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newStubAdminService()
			h := NewAccountHandler(coreShellAdminService{stubAdminService: stub, platform: service.PlatformOpenAI}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			w := coreOAuthHandlerRequest(t, 7, "admin", tc.body, h.CreateCoreRuntimeShell)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Len(t, stub.createdAccounts, 1)
			created := stub.createdAccounts[0]
			require.Equal(t, service.PlatformOpenAI, created.Platform)
			require.Equal(t, tc.wantType, created.Type)
			require.Equal(t, tc.wantCreds, created.Credentials)
			if tc.name == "excel" {
				require.Equal(t, 1, created.Concurrency)
			}
			require.False(t, *created.Schedulable)
			require.NotContains(t, created.Extra, service.CoreOAuthAccountExtraKey)
			require.Contains(t, created.Extra, service.CoreOAuthRuntimeShellExtraKey)
		})
	}
}

func TestCoreRuntimeShellRejectsCredentialAndGroupInjection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"provider":"codex","group_id":2,"api_key":"secret"}`,
		`{"provider":"bps_excel","group_id":2,"base_url":"https://worker.example","api_key":"key","schedulable":true}`,
		`{"provider":"bps_excel","group_id":2,"base_url":"http://worker.example/path?q=1","api_key":"key"}`,
	} {
		stub := newStubAdminService()
		h := NewAccountHandler(coreShellAdminService{stubAdminService: stub, platform: service.PlatformOpenAI}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		w := coreOAuthHandlerRequest(t, 7, "admin", body, h.CreateCoreRuntimeShell)
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Empty(t, stub.createdAccounts)
	}
	stub := newStubAdminService()
	h := NewAccountHandler(coreShellAdminService{stubAdminService: stub, platform: service.PlatformAnthropic}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	w := coreOAuthHandlerRequest(t, 7, "admin", `{"provider":"codex","group_id":2}`, h.CreateCoreRuntimeShell)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Empty(t, stub.createdAccounts)
}

type coreShellAdminService struct {
	*stubAdminService
	platform string
}

func (s coreShellAdminService) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Platform: s.platform, Status: service.StatusActive}, nil
}
