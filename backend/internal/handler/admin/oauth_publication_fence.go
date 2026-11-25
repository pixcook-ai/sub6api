package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) publicationFenceAdmin(c *gin.Context) (service.ManagedOAuthPublicationFenceService, int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return nil, 0, false
	}
	s, ok := h.adminService.(service.ManagedOAuthPublicationFenceService)
	if !ok {
		response.ErrorFrom(c, service.ErrManagedOAuthUnavailable)
		return nil, 0, false
	}
	return s, id, true
}

func exactPublicationFenceFields(raw []byte, names ...string) bool {
	if !managedOAuthUniqueJSONFields(raw) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		if value, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func (h *AccountHandler) FenceOAuthPublication(c *gin.Context) {
	s, id, ok := h.publicationFenceAdmin(c)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10))
	if err != nil || !exactPublicationFenceFields(raw, "operation_id", "identity", "token_revision") {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !exactPublicationFenceFields(fields["identity"], "contract", "backend_id", "binding_ref", "profile_ref", "provider", "subject", "workspace_id", "identity_epoch") {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	var input service.ManagedOAuthPublicationFenceInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !oauthControllerOperationValid(input.OperationID) || input.Identity.Validate() != nil || input.TokenRevision < 0 || input.TokenRevision > 9007199254740991 {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	result, err := s.FenceManagedOAuthPublication(c.Request.Context(), id, &input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) GetOAuthPublicationFence(c *gin.Context) {
	s, id, ok := h.publicationFenceAdmin(c)
	if !ok {
		return
	}
	result, err := s.GetManagedOAuthPublicationFence(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}
