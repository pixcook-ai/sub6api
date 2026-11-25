package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/handler"
	adminhandler "github.com/MACOS-DO/sub4api/internal/handler/admin"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthTransferRoutesRequireAdministrator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "independent-transfer-route-test-secret"}}
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	r := gin.New()
	admin := r.Group("/api/v1/admin", gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, nil, nil, nil)))
	h := &handler.Handlers{Admin: &handler.AdminHandlers{Account: &adminhandler.AccountHandler{}, CoreOAuth: adminhandler.NewCoreOAuthHandler(nil)}}
	registerCoreOAuthRoutes(admin, h)
	registered := map[string]bool{}
	for _, route := range r.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, tc := range []struct{ method, path, registeredPath string }{
		{http.MethodPost, "/profiles/profile/handoff-invitations", "/profiles/:ref/handoff-invitations"},
		{http.MethodPost, "/profiles/profile/handoffs/accept", "/profiles/:ref/handoffs/accept"},
		{http.MethodGet, "/profiles/profile/handoffs/operation", "/profiles/:ref/handoffs/:operation_id"},
		{http.MethodPost, "/profiles/profile/handoffs/operation/abandon", "/profiles/:ref/handoffs/:operation_id/abandon"},
	} {
		require.True(t, registered[tc.method+" /api/v1/admin/core-oauth"+tc.registeredPath])
		for _, key := range []string{"", "source-controller-key-is-not-an-administrator"} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/v1/admin/core-oauth"+tc.path, nil)
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusUnauthorized, w.Code, tc.method+" "+tc.path)
		}
	}
}
