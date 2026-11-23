package admin

import (
	"context"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

type deploymentAdminStub struct {
	service.AdminService
	calls int
}

func (s *deploymentAdminStub) GetManagedDeployment(context.Context, string) (*service.ManagedDeployment, error) {
	return nil, nil
}
func (s *deploymentAdminStub) PutManagedDeployment(_ context.Context, in *service.ManagedDeploymentUpdate) (*service.ManagedDeployment, error) {
	s.calls++
	return &in.ManagedDeployment, nil
}
func TestManagedDeploymentAdminRejectsAmbiguousBindings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{`{"deployment_id":"test","deployment_id":"test"}`, `{"deployment_id":"test","bps":{"provider":"bps_excel","provider":"codex"}}`, `{"deployment_id":"test","bps":{"unknown":1}}`, `{"deployment_id":"other"}`, `{"deployment_id":"test"} {}`, strings.Repeat("a", 33<<10)} {
		stub := &deploymentAdminStub{}
		h := &AccountHandler{adminService: stub}
		r := gin.New()
		r.PUT("/deployments/:id", h.PutManagedDeployment)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("PUT", "/deployments/test", strings.NewReader(body)))
		require.Equal(t, 400, rec.Code, rec.Body.String())
		require.Zero(t, stub.calls)
	}
}
func TestManagedDeploymentAdminDelegatesExactCASPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := &deploymentAdminStub{}
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	r.PUT("/deployments/:id", h.PutManagedDeployment)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/deployments/test", strings.NewReader(`{"deployment_id":"test","expected_route_revision":3,"route_revision":4,"selected_provider":"codex"}`)))
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, 1, stub.calls)
}
