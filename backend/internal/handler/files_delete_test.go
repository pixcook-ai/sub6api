package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/handler"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/server/routes"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type deleteFilesRepository struct {
	service.FileRepository
	owner service.FileOwner
	id    string
	err   error
	calls int
}

func (r *deleteFilesRepository) DeleteLocalFile(_ context.Context, owner service.FileOwner, id string, drain time.Duration) error {
	r.owner, r.id = owner, id
	r.calls++
	return r.err
}

type deleteFilesStore struct{ service.FileBlobStore }

func newDeleteFilesRouter(t *testing.T, repo service.FileRepository, owner bool) *gin.Engine {
	t.Helper()
	cfg := config.DefaultFilesConfig()
	cfg.Enabled, cfg.Root = true, t.TempDir()
	files, err := service.NewFileService(repo, &deleteFilesStore{}, cfg)
	require.NoError(t, err)
	router := gin.New()
	routes.RegisterFileRoutes(router, handler.NewFileHandler(files), middleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		if owner {
			group := int64(42)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 9, UserID: 7, GroupID: &group})
		}
		c.Next()
	}))
	return router
}

func TestFilesDeleteRegisteredRouteReturnsStandardDeletedObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &deleteFilesRepository{}
	router := newDeleteFilesRouter(t, repo, true)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/files/file-local", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"id":"file-local","object":"file","deleted":true}`, rec.Body.String())
	require.Equal(t, service.FileOwner{UserID: 7, GroupID: 42, APIKeyID: 9}, repo.owner)
	require.Equal(t, "file-local", repo.id)
	require.Equal(t, 1, repo.calls)
}

func TestFilesDeleteErrorsDoNotClaimRemoteDeletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"missing", service.ErrFileNotFound, 404, "file_not_found"},
		{"managed remote", service.ErrFileRemoteDeleteUnsupported, 400, "file_remote_delete_unsupported"},
		{"storage unavailable", errors.New("PRIVATE database detail"), 503, "file_storage_unavailable"},
		{"canceled", context.Canceled, 503, "file_storage_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &deleteFilesRepository{err: tc.err}
			router := newDeleteFilesRouter(t, repo, true)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/files/file-owned", nil))
			require.Equal(t, tc.status, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
			require.NotContains(t, rec.Body.String(), `"deleted":true`)
			require.NotContains(t, rec.Body.String(), "PRIVATE")
		})
	}
}

func TestFilesDeleteRequiresAuthorizedOwnerAndOptionalRepositoryPort(t *testing.T) {
	repo := &deleteFilesRepository{}
	router := newDeleteFilesRouter(t, repo, false)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/files/file-owned", nil))
	require.Equal(t, 403, rec.Code)
	require.Zero(t, repo.calls)
	// The embedded interface supplies the old read/upload repository API only.
	oldRepo := &struct{ service.FileRepository }{}
	router = newDeleteFilesRouter(t, oldRepo, true)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/files/file-owned", nil))
	require.Equal(t, 503, rec.Code)
	require.NotContains(t, rec.Body.String(), `"deleted":true`)
}
