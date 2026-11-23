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
	"github.com/google/uuid"
)

func (h *AccountHandler) oauthControllerKeyAdmin(c *gin.Context) (service.ManagedOAuthControllerService, int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return nil, 0, false
	}
	controller, ok := h.adminService.(service.ManagedOAuthControllerService)
	if !ok {
		response.ErrorFrom(c, service.ErrManagedOAuthUnavailable)
		return nil, 0, false
	}
	return controller, id, true
}

func oauthControllerOperationValid(operation string) bool {
	id, err := uuid.Parse(operation)
	return err == nil && id != uuid.Nil && id.String() == operation
}

// RegisterOAuthControllerCredentials only accepts a caller-persisted candidate key. It never returns it.
func (h *AccountHandler) RegisterOAuthControllerCredentials(c *gin.Context) {
	controller, id, ok := h.oauthControllerKeyAdmin(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil || !managedOAuthUniqueJSONFields(raw) {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 2 || fields["operation_id"] == nil || fields["key"] == nil {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	var input struct {
		OperationID string `json:"operation_id"`
		Key         string `json:"key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !oauthControllerOperationValid(input.OperationID) {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	if _, err := service.ManagedOAuthControllerKeyHash(input.Key); err != nil {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	state, err := controller.RegisterManagedOAuthControllerKey(c.Request.Context(), id, input.OperationID, input.Key)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) RevokeOAuthControllerCredentials(c *gin.Context) {
	controller, id, ok := h.oauthControllerKeyAdmin(c)
	if !ok {
		return
	}
	operation := c.Param("operation_id")
	if !oauthControllerOperationValid(operation) {
		response.ErrorFrom(c, service.ErrManagedOAuthInvalid)
		return
	}
	state, err := controller.RevokeManagedOAuthControllerKey(c.Request.Context(), id, operation)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}
