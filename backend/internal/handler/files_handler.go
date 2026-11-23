package handler

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type FileHandler struct {
	files *service.FileService
}

func NewFileHandler(files *service.FileService) *FileHandler { return &FileHandler{files: files} }
func (h *FileHandler) Enabled() bool                         { return h != nil && h.files.Enabled() }
func (h *FileHandler) MaxUploadBodyBytes() int64             { return h.files.MaxUploadBodyBytes() }
func fileOwner(c *gin.Context) (service.FileOwner, bool) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 {
		fileError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required")
		return service.FileOwner{}, false
	}
	return service.FileOwner{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID}, true
}
func fileObject(f *service.StoredFile) gin.H {
	obj := gin.H{"id": f.PublicID, "object": "file", "bytes": f.Bytes, "created_at": f.CreatedAt.Unix(), "filename": f.Filename, "purpose": f.Purpose}
	if f.ExpiresAt != nil {
		obj["expires_at"] = f.ExpiresAt.Unix()
	}
	return obj
}
func (h *FileHandler) List(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	query := c.Request.URL.Query()
	for name, values := range query {
		if len(values) != 1 || (name != "after" && name != "purpose" && name != "order" && name != "limit") {
			fileError(c, 400, "invalid_request_error", "Invalid file list parameter", "invalid_request_error")
			return
		}
	}
	q := service.FileListQuery{After: query.Get("after"), Purpose: query.Get("purpose"), Order: "desc", Limit: 10000}
	if _, present := query["after"]; present && q.After == "" {
		fileError(c, 400, "invalid_request_error", "Invalid file cursor", "invalid_cursor")
		return
	}
	if value, present := query["order"]; present {
		q.Order = value[0]
		if q.Order == "" {
			fileError(c, 400, "invalid_request_error", "Invalid order", "invalid_request_error")
			return
		}
	}
	if value, present := query["limit"]; present {
		n, err := strconv.Atoi(value[0])
		if err != nil || n < 1 || n > 10000 {
			fileError(c, 400, "invalid_request_error", "Invalid limit", "invalid_request_error")
			return
		}
		q.Limit = n
	}
	page, err := h.files.List(c.Request.Context(), owner, q)
	if err != nil {
		handleFileError(c, err)
		return
	}
	data := make([]gin.H, 0, len(page.Files))
	for _, f := range page.Files {
		data = append(data, fileObject(f))
	}
	var first, last *string
	if len(page.Files) > 0 {
		first = &page.Files[0].PublicID
		last = &page.Files[len(page.Files)-1].PublicID
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data, "first_id": first, "last_id": last, "has_more": page.HasMore})
}
func (h *FileHandler) Upload(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	key, _ := middleware.GetAPIKeyFromContext(c)
	if key.Group == nil || key.Group.Platform != service.PlatformOpenAI {
		fileError(c, http.StatusForbidden, "permission_denied", "File uploads require an OpenAI group", "unsupported_file_platform")
		return
	}
	reauthorize := middleware.FileUploadReauthorize(c)
	if reauthorize == nil {
		fileError(c, 503, "server_error", "Fresh upload authorization is unavailable", "file_authorization_unavailable")
		return
	}
	reader, err := c.Request.MultipartReader()
	if err != nil {
		fileError(c, 400, "invalid_request_error", "Expected multipart/form-data")
		return
	}
	uploadCtx, cancel := context.WithTimeout(c.Request.Context(), h.files.UploadTimeout())
	defer cancel()
	stop, err := bindFileUploadDeadline(c.Writer, uploadCtx)
	if err != nil {
		fileError(c, 503, "server_error", "Bounded upload transport is unavailable", "file_storage_unavailable")
		return
	}
	defer stop()
	f, err := h.files.UploadMultipartWithHooks(uploadCtx, owner, reader, service.FileUploadHooks{InterruptRead: cancel, Reauthorize: reauthorize})
	if err != nil {
		if errors.Is(uploadCtx.Err(), context.DeadlineExceeded) {
			err = service.ErrFileUploadDeadline
		}
		handleFileError(c, err)
		return
	}
	c.JSON(http.StatusOK, fileObject(f))
}
func (h *FileHandler) Get(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	f, err := h.files.Get(c.Request.Context(), owner, c.Param("file_id"))
	if err != nil {
		handleFileError(c, err)
		return
	}
	c.JSON(http.StatusOK, fileObject(f))
}
func (h *FileHandler) Delete(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	id := c.Param("file_id")
	if err := h.files.Delete(c.Request.Context(), owner, id); err != nil {
		handleFileError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{"id": id, "object": "file", "deleted": true})
}
func (h *FileHandler) Content(c *gin.Context) {
	owner, ok := fileOwner(c)
	if !ok {
		return
	}
	f, reader, err := h.files.Open(c.Request.Context(), owner, c.Param("file_id"))
	if err != nil {
		handleFileError(c, err)
		return
	}
	defer reader.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Filename}))
	c.Header("Content-Length", strconv.FormatInt(f.Bytes, 10))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-SHA256", f.SHA256)
	c.Status(http.StatusOK)
	_, _ = io.CopyBuffer(c.Writer, reader, make([]byte, 32*1024))
}
func fileError(c *gin.Context, status int, kind, message string, code ...string) {
	var value any
	if len(code) > 0 {
		value = code[0]
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": kind, "param": nil, "code": value}})
}
func handleFileError(c *gin.Context, err error) {
	var bodyTooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, service.ErrFileRemoteDeleteUnsupported):
		fileError(c, 400, "invalid_request_error", "Deletion of managed remote file copies is not supported", "file_remote_delete_unsupported")
	case errors.Is(err, service.ErrFileAuthorizationRevoked):
		fileError(c, 403, "permission_denied", "File upload authorization is no longer valid", "file_authorization_revoked")
	case errors.Is(err, service.ErrFileUploadDeadline):
		fileError(c, 408, "timeout_error", "File upload deadline exceeded", "file_upload_timeout")
	case errors.Is(err, service.ErrFileUploadConcurrency):
		fileError(c, 429, "rate_limit_error", "Concurrent file upload limit exceeded", "file_upload_concurrency_exceeded")
	case errors.Is(err, service.ErrFileInvalidCursor):
		fileError(c, 400, "invalid_request_error", "Invalid file cursor", "invalid_cursor")
	case errors.Is(err, service.ErrFileNotFound):
		fileError(c, 404, "invalid_request_error", "File not found", "file_not_found")
	case errors.Is(err, service.ErrFileTooLarge) || errors.As(err, &bodyTooLarge):
		fileError(c, 413, "invalid_request_error", "File exceeds the configured upload limit", "file_too_large")
	case errors.Is(err, service.ErrInvalidFile):
		fileError(c, 400, "invalid_request_error", err.Error())
	case errors.Is(err, service.ErrFileQuotaExceeded) || errors.Is(err, service.ErrFileStorageFull):
		fileError(c, 429, "storage_quota_exceeded", "File storage capacity is exhausted", "file_storage_quota_exceeded")
	case errors.Is(err, service.ErrFileLeaseConflict):
		fileError(c, 409, "conflict_error", "File operation lease expired; retry the operation", "file_lease_conflict")
	default:
		fileError(c, 503, "server_error", "File storage is temporarily unavailable", "file_storage_unavailable")
	}
}

func bindFileUploadDeadline(writer http.ResponseWriter, ctx context.Context) (func(), error) {
	controller := http.NewResponseController(writer)
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, http.ErrNotSupported
	}
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = controller.SetReadDeadline(time.Now()) })
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
			if ctx.Err() != nil {
				_ = controller.SetReadDeadline(time.Now())
			} else {
				_ = controller.SetReadDeadline(time.Time{})
			}
		})
	}, nil
}
