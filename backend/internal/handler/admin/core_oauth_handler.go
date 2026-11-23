package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type coreOAuthAdminService interface {
	CreateProfile(context.Context, service.CoreOAuthCreateInput) (*service.CoreOAuthProfile, error)
	ListProfiles(context.Context) ([]service.CoreOAuthProfile, error)
	GetProfile(context.Context, string) (*service.CoreOAuthProfile, error)
	StartAuthorization(context.Context, string, int64) (*service.CoreOAuthAuthorization, error)
	CompleteAuthorization(context.Context, string, service.CoreOAuthCompleteInput) (*service.CoreOAuthProfile, error)
	CancelAuthorization(context.Context, string, string, int64) (*service.CoreOAuthProfile, error)
	Refresh(context.Context, string, int64) (*service.CoreOAuthProfile, error)
}

// CoreOAuthHandler exposes safe profile state. Tokens never cross this boundary.
type CoreOAuthHandler struct{ core coreOAuthAdminService }

func NewCoreOAuthHandler(core *service.CoreOAuthService) *CoreOAuthHandler {
	h := &CoreOAuthHandler{}
	if core != nil {
		h.core = core
	}
	return h
}

func coreOAuthAdminActor(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "Administrator authentication required")
		return 0, false
	}
	role, ok := middleware.GetUserRoleFromContext(c)
	if !ok || role != "admin" {
		response.Forbidden(c, "Administrator access required")
		return 0, false
	}
	return subject.UserID, true
}

func (h *CoreOAuthHandler) available(c *gin.Context) bool {
	if h == nil || h.core == nil {
		response.ErrorFrom(c, service.ErrCoreOAuthUnavailable)
		return false
	}
	return true
}

func (h *CoreOAuthHandler) Capabilities(c *gin.Context) {
	if _, ok := coreOAuthAdminActor(c); !ok {
		return
	}
	available := h != nil && h.core != nil
	result := gin.H{"available": available, "providers": []gin.H{
		{"provider": service.CoreOAuthProviderCodex, "available": available, "requires_callback_url": true},
		{"provider": service.CoreOAuthProviderExcel, "available": available, "requires_callback_url": true},
	}}
	if !available {
		result["reason"] = "stable_encryption_key_required"
	}
	response.Success(c, result)
}

func (h *CoreOAuthHandler) ListProfiles(c *gin.Context) {
	if _, ok := coreOAuthAdminActor(c); !ok || !h.available(c) {
		return
	}
	profiles, err := h.core.ListProfiles(c.Request.Context())
	if response.ErrorFrom(c, err) {
		return
	}
	if profiles == nil {
		profiles = []service.CoreOAuthProfile{}
	}
	response.Success(c, gin.H{"profiles": profiles})
}

func (h *CoreOAuthHandler) CreateProfile(c *gin.Context) {
	if _, ok := coreOAuthAdminActor(c); !ok || !h.available(c) {
		return
	}
	var in service.CoreOAuthCreateInput
	if !decodeCoreOAuthBody(c, &in) {
		return
	}
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 || (in.ProxyID != nil && *in.ProxyID <= 0) {
		response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
		return
	}
	profile, err := h.core.CreateProfile(c.Request.Context(), in)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, profile)
}

func (h *CoreOAuthHandler) GetProfile(c *gin.Context) {
	if _, ok := coreOAuthAdminActor(c); !ok || !h.available(c) {
		return
	}
	profile, err := h.core.GetProfile(c.Request.Context(), c.Param("ref"))
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, profile)
}

func (h *CoreOAuthHandler) Authorize(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok || !h.available(c) {
		return
	}
	var in struct{}
	if !decodeCoreOAuthBody(c, &in) {
		return
	}
	authorization, err := h.core.StartAuthorization(c.Request.Context(), c.Param("ref"), actor)
	if response.ErrorFrom(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	response.Success(c, authorization)
}

func (h *CoreOAuthHandler) Complete(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok || !h.available(c) {
		return
	}
	var in service.CoreOAuthCompleteInput
	if !decodeCoreOAuthBody(c, &in) {
		return
	}
	if strings.TrimSpace(in.AttemptID) == "" || len(in.AttemptID) > 200 || strings.TrimSpace(in.CallbackURL) == "" || len(in.CallbackURL) > 16384 {
		response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
		return
	}
	in.ActorID = actor
	profile, err := h.core.CompleteAuthorization(c.Request.Context(), c.Param("ref"), in)
	if response.ErrorFrom(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, profile)
}

func (h *CoreOAuthHandler) Cancel(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok || !h.available(c) {
		return
	}
	var in struct {
		AttemptID string `json:"attempt_id"`
	}
	if !decodeCoreOAuthBody(c, &in) {
		return
	}
	if strings.TrimSpace(in.AttemptID) == "" || len(in.AttemptID) > 200 {
		response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
		return
	}
	profile, err := h.core.CancelAuthorization(c.Request.Context(), c.Param("ref"), in.AttemptID, actor)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, profile)
}

func (h *CoreOAuthHandler) Refresh(c *gin.Context) {
	if _, ok := coreOAuthAdminActor(c); !ok || !h.available(c) {
		return
	}
	var in struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeCoreOAuthBody(c, &in) {
		return
	}
	if in.ExpectedRevision <= 0 {
		response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
		return
	}
	profile, err := h.core.Refresh(c.Request.Context(), c.Param("ref"), in.ExpectedRevision)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, profile)
}

func decodeCoreOAuthBody(c *gin.Context, target any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10))
	if err == nil && managedDeploymentUniqueJSON(raw) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if dec.Decode(target) == nil && dec.Decode(new(any)) == io.EOF {
			return true
		}
	}
	// Do not echo a parser error: callbacks can contain authorization secrets.
	response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
	return false
}
