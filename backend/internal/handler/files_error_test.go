package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFilesErrorsDistinguishUploadDeadlineFromStorageTimeout(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{context.DeadlineExceeded, 503, "file_storage_unavailable"},
		{service.ErrFileUploadDeadline, 408, "file_upload_timeout"},
		{service.ErrFileAuthorizationUnavailable, 503, "file_storage_unavailable"},
		{service.ErrFileAuthorizationRevoked, 403, "file_authorization_revoked"},
	} {
		t.Run(tc.code+tc.err.Error(), func(t *testing.T) {
			r := gin.New()
			r.GET("/v1/files", func(c *gin.Context) { handleFileError(c, tc.err) })
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/v1/files", nil))
			require.Equal(t, tc.status, w.Code)
			require.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
		})
	}
}
