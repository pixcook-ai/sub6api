package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type StoredResponseHandler struct {
	responses *service.StoredResponseService
}

func NewStoredResponseHandler(responses *service.StoredResponseService) *StoredResponseHandler {
	return &StoredResponseHandler{responses: responses}
}

func (h *StoredResponseHandler) Enabled() bool {
	return h != nil && h.responses.Enabled()
}

func storedResponseOwner(c *gin.Context) (service.StoredResponseOwner, bool) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 || key.Group == nil || key.Group.ID != *key.GroupID {
		storedResponseError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required", "permission_denied")
		return service.StoredResponseOwner{}, false
	}
	return service.StoredResponseOwner{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID}, true
}

func (h *StoredResponseHandler) Retrieve(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if invalid := parseStoredResponseRetrieveQuery(c.Request.URL.RawQuery); invalid != "" {
		storedInputItemsQueryError(c, invalid)
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	record, err := h.responses.Get(c.Request.Context(), owner, c.Param("response_id"))
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if record == nil || len(record.PublicResponse) == 0 {
		storedResponseUnavailable(c)
		return
	}
	if !storedResponseModelAllowed(c, record.Model) {
		return
	}
	// StoredResponse contains backend binding and lease metadata. Return only its
	// separately validated public object, never serialize the private record.
	c.Data(http.StatusOK, "application/json", record.PublicResponse)
}

func (h *StoredResponseHandler) Delete(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	if c.Request.URL.RawQuery != "" {
		storedResponseError(c, http.StatusBadRequest, "invalid_request_error", "Response deletion query parameters are not supported", "invalid_request_error")
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	id := c.Param("response_id")
	model, err := h.responses.GetManagementModel(c.Request.Context(), owner, id)
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if !storedResponseModelAllowed(c, model) {
		return
	}
	if err := h.responses.Delete(c.Request.Context(), owner, id); err != nil {
		handleStoredResponseError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": id, "object": "response.deleted", "deleted": true})
}

func storedResponseModelAllowed(c *gin.Context, model string) bool {
	if strings.TrimSpace(model) == "" {
		storedResponseUnavailable(c)
		return false
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil {
		storedResponseError(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required", "permission_denied")
		return false
	}
	if !key.Group.ModelAllowlist.Allows(model) {
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
		middleware.MarkIngressRejected(c, middleware.IngressRejectModelNotAllowed)
		middleware.OpenAIErrorWriter(c, http.StatusNotFound, fmt.Sprintf("Model %q is not available for this group", model))
		c.Abort()
		return false
	}
	return true
}

func storedResponseError(c *gin.Context, status int, kind, message, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": kind, "param": nil, "code": code}})
}

func storedResponseUnavailable(c *gin.Context) {
	storedResponseError(c, http.StatusServiceUnavailable, "server_error", "Response storage is temporarily unavailable", "response_storage_unavailable")
}

type storedResponsePublicError struct {
	status              int
	kind, code, message string
	param               any
}

func storedResponsePublicErrorFor(err error) storedResponsePublicError {
	// Native history resolves public files through the same resource ACL.
	if status, kind, code, message, ok := managedFilePreparationErrorDetails(&service.ManagedFilePreparationError{Err: err}); ok && code != "managed_files_unavailable" {
		return storedResponsePublicError{status, kind, code, message, nil}
	}
	var policy *managedfiles.ResponsesPreflightError
	if errors.As(err, &policy) {
		return storedResponsePublicError{policy.Status, policy.Type, policy.Code, policy.Message, policy.Param}
	}
	switch {
	case errors.Is(err, service.ErrBPSRequestAuthorizationRevoked):
		return storedResponsePublicError{403, "permission_denied", "permission_denied", "Request authorization is no longer valid", nil}
	case errors.Is(err, service.ErrBPSRequestAuthorizationUnavailable):
		return storedResponsePublicError{503, "server_error", "response_storage_unavailable", "Request authorization is temporarily unavailable", nil}
	case errors.Is(err, service.ErrBPSStoredResponseRoute):
		return storedResponsePublicError{503, "server_error", "response_storage_route_unavailable", "Response storage backend route is unavailable", nil}
	case errors.Is(err, service.ErrStoredResponseNotFound):
		return storedResponsePublicError{404, "invalid_request_error", "response_not_found", "Response not found", nil}
	case errors.Is(err, service.ErrStoredResponseInvalid), errors.Is(err, service.ErrStoredResponseUnsupported):
		return storedResponsePublicError{400, "invalid_request_error", "invalid_request_error", "Invalid stored response request", nil}
	case errors.Is(err, service.ErrStoredResponseQuota):
		return storedResponsePublicError{429, "rate_limit_error", "response_storage_quota_exceeded", "Response storage capacity is exhausted", nil}
	case errors.Is(err, service.ErrStoredResponseTooLarge):
		return storedResponsePublicError{413, "invalid_request_error", "response_too_large", "Response exceeds the configured storage limit", nil}
	case errors.Is(err, service.ErrStoredResponseConflict):
		return storedResponsePublicError{409, "conflict_error", "response_conflict", "Response operation conflicts with its current state", nil}
	default:
		return storedResponsePublicError{503, "server_error", "response_storage_unavailable", "Response storage is temporarily unavailable", nil}
	}
}

func handleStoredResponseError(c *gin.Context, err error) {
	public := storedResponsePublicErrorFor(err)
	c.AbortWithStatusJSON(public.status, gin.H{"error": gin.H{"type": public.kind, "code": public.code, "param": public.param, "message": public.message}})
}

func (h *OpenAIGatewayHandler) handleStoredResponseDispatchError(c *gin.Context, err error, streamStarted bool) {
	if !streamStarted && !c.Writer.Written() {
		handleStoredResponseError(c, err)
		return
	}
	public := storedResponsePublicErrorFor(err)
	h.handleStreamingAwareErrorWithCode(c, public.status, public.kind, public.code, public.message, true, false)
}
