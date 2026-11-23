package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type managedOAuthAdminStub struct {
	*stubAdminService
	installs int
	last     *service.ManagedOAuthInstallRequest
}

func (s *managedOAuthAdminStub) GetManagedOAuthState(context.Context, int64) (*service.ManagedOAuthPublicState, error) {
	return &service.ManagedOAuthPublicState{AccountID: 17, TokenRevision: 9, LastOperationID: "operation"}, nil
}
func (s *managedOAuthAdminStub) InstallManagedOAuthCredentials(_ context.Context, _ int64, input *service.ManagedOAuthInstallRequest) (*service.ManagedOAuthPublicState, error) {
	s.installs++
	s.last = input
	return &service.ManagedOAuthPublicState{AccountID: 17, TokenRevision: input.TokenRevision}, nil
}
func (s *managedOAuthAdminStub) ListManagedOAuthAuthEvents(context.Context, int64, int64) (*service.ManagedOAuthEventPage, error) {
	return &service.ManagedOAuthEventPage{Events: []service.ManagedOAuthAuthEvent{}, NextCursor: 4}, nil
}
func TestManagedOAuthHandlerRejectsNonATPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{"refresh_token":"rt"}`, `{"id_token":"id"}`, `{"access_token":"a","access_token":"b"}`, `{"token_revision":1} {}`, `[]`, strings.Repeat("a", 129<<10)} {
		t.Run(body[:min(len(body), 40)], func(t *testing.T) {
			stub := &managedOAuthAdminStub{stubAdminService: newStubAdminService()}
			h := &AccountHandler{adminService: stub}
			router := gin.New()
			router.PUT("/accounts/:id/managed-credentials", h.InstallManagedOAuthCredentials)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/accounts/17/managed-credentials", bytes.NewBufferString(body))
			router.ServeHTTP(w, req)
			require.Equal(t, 400, w.Code, w.Body.String())
			require.Zero(t, stub.installs)
		})
	}
}
func TestManagedOAuthHandlerDelegatesAndReturnsPublicState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &managedOAuthAdminStub{stubAdminService: newStubAdminService()}
	h := &AccountHandler{adminService: stub}
	router := gin.New()
	router.PUT("/accounts/:id/managed-credentials", h.InstallManagedOAuthCredentials)
	router.GET("/accounts/:id/managed-credentials", h.GetManagedOAuthCredentials)
	router.GET("/accounts/:id/managed-credentials/auth-events", h.ListManagedOAuthAuthEvents)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/accounts/17/managed-credentials", strings.NewReader(`{"access_token":"synthetic-at","token_revision":9}`)))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, stub.installs)
	require.Equal(t, "synthetic-at", stub.last.AccessToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/accounts/17/managed-credentials", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "last_operation_id")
	require.NotContains(t, w.Body.String(), "access_token")
	require.NotContains(t, w.Body.String(), "refresh_token")
	require.NotContains(t, w.Body.String(), "operation_hash")
	for _, suffix := range []string{"?after=-1", "?after=bad"} {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/accounts/17/managed-credentials/auth-events"+suffix, nil))
		require.Equal(t, 400, w.Code)
	}
}
func TestManagedOAuthHandlerFailsClosedWithoutControllerService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &AccountHandler{adminService: newStubAdminService()}
	router := gin.New()
	router.GET("/accounts/:id/managed-credentials", h.GetManagedOAuthCredentials)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/accounts/17/managed-credentials", nil))
	require.Equal(t, 503, w.Code)
}
