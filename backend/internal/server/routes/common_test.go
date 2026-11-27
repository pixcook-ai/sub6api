package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCommonRoutesAcknowledgesTelemetryBatch(t *testing.T) {
	router := gin.New()
	RegisterCommonRoutes(router)

	for _, path := range []string{"/api/event_logging/batch", "/api/event_logging/v2/batch"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"events":[{"type":"synthetic_event"}]}`))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, req)

			require.Equal(t, http.StatusOK, recorder.Code)
			require.Empty(t, recorder.Body.String())
		})
	}
}
