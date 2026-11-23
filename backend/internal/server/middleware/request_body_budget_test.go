package middleware

import (
	"errors"
	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBodyLimitSharesBudgetAndReleasesAfterHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	budget := pkghttputil.NewBodyMemoryBudget(1024)
	r := gin.New()
	r.Use(RequestBodyLimit(8192, budget))
	r.POST("/", func(c *gin.Context) {
		_, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if errors.Is(err, pkghttputil.ErrRequestBodyMemoryBudget) {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		require.NoError(t, err)
		c.Status(http.StatusNoContent)
	})
	for _, tc := range []struct {
		body   string
		status int
	}{{strings.Repeat("x", 2048), 503}, {"small", 204}, {"small", 204}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
		require.Equal(t, tc.status, w.Code)
	}
}
