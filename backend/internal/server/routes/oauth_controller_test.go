package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/handler"
	adminhandler "github.com/MACOS-DO/sub4api/internal/handler/admin"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const routeOAuthControllerKey = "controller-route-secret-0123456789-abcdefghijklmnop"

func TestOAuthControllerRouteAllowlistIsATOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB6_OAUTH_CONTROLLERS", `[{"key_env":"TEST_ROUTE_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"]}]`)
	t.Setenv("TEST_ROUTE_CONTROLLER_KEY", routeOAuthControllerKey)
	r := gin.New()
	registerOAuthControllerRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}})
	var routes []string
	for _, route := range r.Routes() {
		routes = append(routes, route.Method+" "+route.Path)
	}
	sort.Strings(routes)
	require.Equal(t, []string{
		"GET /api/v1/oauth-controller/accounts/:id/managed-credentials",
		"GET /api/v1/oauth-controller/accounts/:id/managed-credentials/auth-events",
		"PUT /api/v1/oauth-controller/accounts/:id/managed-credentials",
	}, routes)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/oauth-controller/accounts"},
		{http.MethodPut, "/api/v1/oauth-controller/accounts/17"},
		{http.MethodPost, "/api/v1/oauth-controller/accounts/17/refresh"},
		{http.MethodPost, "/api/v1/oauth-controller/accounts/17/schedulable"},
		{http.MethodGet, "/api/v1/oauth-controller/users"},
		{http.MethodDelete, "/api/v1/oauth-controller/accounts/17/managed-credentials"},
		{http.MethodPost, "/api/v1/oauth-controller/accounts/17/managed-credentials"},
		{http.MethodPut, "/api/v1/oauth-controller/accounts/17/managed-credentials/auth-events"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+routeOAuthControllerKey)
		r.ServeHTTP(w, req)
		require.Equal(t, 404, w.Code, tc.method+" "+tc.path)
	}
}

type oauthControllerInferenceRepo struct{ service.APIKeyRepository }

func (*oauthControllerInferenceRepo) GetByKey(context.Context, string) (*service.APIKey, error) {
	return nil, service.ErrAPIKeyNotFound
}
func (*oauthControllerInferenceRepo) GetByKeyForAuth(context.Context, string) (*service.APIKey, error) {
	return nil, service.ErrAPIKeyNotFound
}

func TestOAuthControllerKeyIsNotAdministratorOrInferenceCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB6_OAUTH_CONTROLLERS", `[{"key_env":"TEST_ROUTE_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"]}]`)
	t.Setenv("TEST_ROUTE_CONTROLLER_KEY", routeOAuthControllerKey)
	cfg := &config.Config{RunMode: config.RunModeSimple, JWT: config.JWTConfig{Secret: "unrelated-administrator-jwt-secret"}}
	apiKeys := service.NewAPIKeyService(&oauthControllerInferenceRepo{}, nil, nil, nil, nil, nil, cfg)
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	registerOAuthControllerRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}})
	r.GET("/api/v1/admin/protected", gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, nil, nil, nil)), func(c *gin.Context) { t.Error("controller key reached administrator handler"); c.Status(200) })
	r.POST("/v1/responses", gin.HandlerFunc(middleware.NewAPIKeyAuthMiddleware(apiKeys, nil, cfg)), func(c *gin.Context) { t.Error("controller key reached inference handler"); c.Status(200) })
	for _, tc := range []struct{ method, path string }{{http.MethodGet, "/api/v1/admin/protected"}, {http.MethodPost, "/v1/responses"}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+routeOAuthControllerKey)
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	}
}

func TestOAuthControllerCredentialManagementRoutesRequireAdministrator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB6_OAUTH_CONTROLLERS", `[{"key_env":"TEST_ROUTE_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"]}]`)
	t.Setenv("TEST_ROUTE_CONTROLLER_KEY", routeOAuthControllerKey)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "independent-real-administrator-jwt-secret"}}
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	h := &handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}}}
	adminAuth := middleware.NewAdminAuthMiddleware(auth, nil, nil, nil)
	audit := middleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := middleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(r.Group("/api/v1"), h, adminAuth, audit, stepUp, nil, nil)
	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	require.True(t, registered["POST /api/v1/admin/accounts/:id/oauth-controller-credentials"])
	require.True(t, registered["POST /api/v1/admin/accounts/:id/oauth-publication-fence"])
	require.True(t, registered["GET /api/v1/admin/accounts/:id/oauth-publication-fence"])
	require.True(t, registered["DELETE /api/v1/admin/accounts/:id/oauth-controller-credentials/:operation_id"])
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/admin/accounts/17/oauth-controller-credentials"},
		{http.MethodPost, "/api/v1/admin/accounts/17/oauth-publication-fence"},
		{http.MethodGet, "/api/v1/admin/accounts/17/oauth-publication-fence"},
		{http.MethodDelete, "/api/v1/admin/accounts/17/oauth-controller-credentials/c45ce361-c1d8-44b2-89b3-d0955e17cc88"},
	} {
		for _, key := range []string{"", routeOAuthControllerKey} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusUnauthorized, w.Code, tc.method+" "+tc.path+" "+w.Body.String())
		}
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/oauth-controller/accounts/17/oauth-controller-credentials"},
		{http.MethodPost, "/api/v1/oauth-controller/accounts/17/oauth-publication-fence"},
		{http.MethodGet, "/api/v1/oauth-controller/accounts/17/oauth-publication-fence"},
		{http.MethodDelete, "/api/v1/oauth-controller/accounts/17/oauth-controller-credentials/c45ce361-c1d8-44b2-89b3-d0955e17cc88"},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+routeOAuthControllerKey)
		r.ServeHTTP(w, req)
		require.Equal(t, 404, w.Code, tc.path)
	}
}
