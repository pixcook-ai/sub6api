package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tiboSettingsMemoryRepo struct {
	service.SettingRepository
	value  string
	writes int
}

func (r *tiboSettingsMemoryRepo) GetValue(context.Context, string) (string, error) {
	if r.value == "" {
		return "", service.ErrSettingNotFound
	}
	return r.value, nil
}
func (r *tiboSettingsMemoryRepo) Set(_ context.Context, _ string, value string) error {
	r.value = value
	r.writes++
	return nil
}

func TestTiboSettingsHTTPRoundTripWithoutGroups(t *testing.T) {
	repo := &tiboSettingsMemoryRepo{}
	h := &SettingHandler{settingService: service.NewSettingService(repo, nil)}
	router := gin.New()
	router.GET("/settings", h.GetOpenAITiboSettings)
	router.PUT("/settings", h.UpdateOpenAITiboSettings)
	invoke := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/settings", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		return w
	}
	first := invoke(http.MethodGet, "")
	require.Equal(t, 200, first.Code)
	var initial struct {
		Data service.TiboAutomationSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &initial))
	require.False(t, initial.Data.Enabled)
	saved := invoke(http.MethodPut, `{"enabled":true,"auto_group_enabled":false,"healthy_group_id":0,"degraded_group_id":0,"pause_on_degraded":true,"auto_resume_on_healthy":false}`)
	require.Equal(t, 200, saved.Code, saved.Body.String())
	require.Equal(t, 1, repo.writes)
	var payload struct {
		Data service.TiboAutomationSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(invoke(http.MethodGet, "").Body.Bytes(), &payload))
	require.True(t, payload.Data.Enabled)
	require.False(t, payload.Data.AutoGroupEnabled)
	require.True(t, payload.Data.PauseOnDegraded)
	invalid := invoke(http.MethodPut, `{"enabled":true,"auto_group_enabled":true,"healthy_group_id":4,"degraded_group_id":4}`)
	require.Equal(t, 400, invalid.Code, invalid.Body.String())
	require.Equal(t, 1, repo.writes)
	malformed := invoke(http.MethodPut, `{"enabled":"yes"}`)
	require.Equal(t, 400, malformed.Code)
	require.Equal(t, 1, repo.writes)
}

func TestTiboClearIsolationRejectsInvalidAccount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       string
		platform string
	}{
		{name: "invalid_id", id: "wrong", platform: service.PlatformOpenAI},
		{name: "nonpositive_id", id: "0", platform: service.PlatformOpenAI},
		{name: "other_platform", id: "42", platform: service.PlatformAnthropic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adminService := &stubAdminService{getAccountResult: &service.Account{ID: 42, Platform: tc.platform}}
			h := &AccountHandler{adminService: adminService}
			router := gin.New()
			router.POST("/accounts/:id/tibo/clear-isolation", h.ClearTiboQualityIsolation)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/"+tc.id+"/tibo/clear-isolation", nil))
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, adminService.updateAccountCalls)
		})
	}
}
