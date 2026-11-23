package admin

import (
	"bytes"
	"encoding/json"
	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"strings"
)

func (h *AccountHandler) GetManagedDeploymentSwitchOptions(c *gin.Context) {
	if h.codexTicketGateway == nil {
		response.ErrorFrom(c, service.ErrManagedDeploymentUnavailable)
		return
	}
	query := c.Request.URL.Query()
	for name, values := range query {
		if (name != "requested_model" && name != "required_capabilities") || len(values) != 1 {
			response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
			return
		}
	}
	capabilities := []service.OpenAIEndpointCapability{}
	if raw := query.Get("required_capabilities"); raw != "" {
		for _, capability := range strings.Split(raw, ",") {
			capabilities = append(capabilities, service.OpenAIEndpointCapability(capability))
		}
	}
	state, err := h.codexTicketGateway.GetManagedDeploymentSwitchOptions(c.Request.Context(), c.Param("id"), query.Get("requested_model"), capabilities)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) SwitchManagedDeployment(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10))
	if err != nil || !managedDeploymentUniqueJSON(raw) {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in service.ManagedDeploymentSwitchRequest
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	if h.codexTicketGateway == nil {
		response.ErrorFrom(c, service.ErrManagedDeploymentUnavailable)
		return
	}
	state, err := h.codexTicketGateway.SwitchManagedDeployment(c.Request.Context(), c.Param("id"), in)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) InstallCoreOAuthRuntime(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	if err != nil || !managedDeploymentUniqueJSON(raw) {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in service.CoreOAuthRuntimeInstallRequest
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	if h.codexTicketGateway == nil {
		response.ErrorFrom(c, service.ErrManagedDeploymentUnavailable)
		return
	}
	state, err := h.codexTicketGateway.InstallCoreOAuthRuntime(c.Request.Context(), c.Param("id"), in)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, state)
}

func (h *AccountHandler) GetManagedDeployment(c *gin.Context) {
	admin, ok := h.adminService.(service.ManagedDeploymentAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrManagedDeploymentUnavailable)
		return
	}
	d, e := admin.GetManagedDeployment(c.Request.Context(), c.Param("id"))
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	if d == nil {
		response.NotFound(c, "Managed deployment not found")
		return
	}
	response.Success(c, d)
}
func (h *AccountHandler) PutManagedDeployment(c *gin.Context) {
	admin, ok := h.adminService.(service.ManagedDeploymentAdminService)
	if !ok {
		response.ErrorFrom(c, service.ErrManagedDeploymentUnavailable)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10))
	if e != nil || !managedDeploymentUniqueJSON(raw) {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in service.ManagedDeploymentUpdate
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || in.DeploymentID != c.Param("id") {
		response.ErrorFrom(c, service.ErrManagedDeploymentInvalid)
		return
	}
	d, e := admin.PutManagedDeployment(c.Request.Context(), &in)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, d)
}

// Reject duplicate names recursively, including immutable binding objects.
func managedDeploymentUniqueJSON(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func() bool
	walk = func() bool {
		t, e := dec.Token()
		if e != nil {
			return false
		}
		d, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				v, ok := k.(string)
				if e != nil || !ok || seen[v] {
					return false
				}
				seen[v] = true
				if !walk() {
					return false
				}
			}
			t, e = dec.Token()
			return e == nil && t == json.Delim('}')
		case '[':
			for dec.More() {
				if !walk() {
					return false
				}
			}
			t, e = dec.Token()
			return e == nil && t == json.Delim(']')
		default:
			return false
		}
	}
	return len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' && walk() && dec.Decode(new(any)) == io.EOF
}
