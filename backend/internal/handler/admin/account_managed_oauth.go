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

func (h *AccountHandler) managedOAuthAdmin(c *gin.Context) (service.ManagedOAuthAdminService, int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return nil, 0, false
	}
	admin, ok := h.adminService.(service.ManagedOAuthAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrManagedOAuthUnavailable)
		return nil, 0, false
	}
	return admin, id, true
}

// These AT-only handlers serve administrator routes and the separately scoped controller routes.
func (h *AccountHandler) GetManagedOAuthCredentials(c *gin.Context) {
	admin, id, ok := h.managedOAuthAdmin(c)
	if !ok {
		return
	}
	state, err := admin.GetManagedOAuthState(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) InstallManagedOAuthCredentials(c *gin.Context) {
	admin, id, ok := h.managedOAuthAdmin(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128<<10)
	raw, readErr := io.ReadAll(c.Request.Body)
	if readErr != nil || !managedOAuthUniqueJSONFields(raw) {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input service.ManagedOAuthInstallRequest
	if decoder.Decode(&input) != nil {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	state, err := admin.InstallManagedOAuthCredentials(c.Request.Context(), id, &input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func managedOAuthUniqueJSONFields(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	_, err = decoder.Token()
	return err == nil
}

func (h *AccountHandler) ListManagedOAuthAuthEvents(c *gin.Context) {
	admin, id, ok := h.managedOAuthAdmin(c)
	if !ok {
		return
	}
	after := int64(0)
	if raw, exists := c.GetQuery("after"); exists {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
			return
		}
		after = value
	}
	page, err := admin.ListManagedOAuthAuthEvents(c.Request.Context(), id, after)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, page)
}
