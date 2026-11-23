package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const managedFilesUnsupportedMessage = "Managed files require HTTP Responses with a configured BPS backend"

// ManagedFilesIngress runs after authentication and composite resolution, before
// platform dispatch. Only typed file positions participate; text and tool
// arguments stay opaque. Preparation holds local read leases until the handler
// has finished forwarding and draining usage.
func (h *OpenAIGatewayHandler) ManagedFilesIngress(c *gin.Context) {
	for _, header := range []string{managedfiles.ServiceKeyHeader, managedfiles.AdmissionHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.BindingHeader, managedfiles.RecoveryFeatureHeader} {
		c.Request.Header.Del(header)
	}
	if !h.managedFilesEnabled() || c.Request.Method != http.MethodPost {
		c.Next()
		return
	}
	path := c.Request.URL.Path
	protocol := ""
	switch {
	case strings.HasSuffix(path, "/responses") || strings.Contains(path, "/responses/"):
		protocol = "responses"
	case strings.HasSuffix(path, "/chat/completions"):
		protocol = "chat"
	case strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/messages/count_tokens"):
		protocol = "messages"
	default:
		c.Next()
		return
	}
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		status := http.StatusBadRequest
		if _, large := extractMaxBytesError(err); large {
			status = http.StatusRequestEntityTooLarge
		}
		rejectManagedFilesIngress(c, status, "invalid_request_error", "Invalid Responses request body")
		return
	}
	if protocol != "responses" {
		if managedMessageFileReference(body) {
			rejectManagedFilesUnsupported(c)
			return
		}
		c.Next()
		return
	}
	if !service.HasManagedResponseFiles(body) {
		c.Next()
		return
	}
	// Native local history rejects typed files before any file Prepare/leases.
	// Scan every top-level occurrence so mixed duplicate IDs cannot bypass it.
	if hasLocalPreviousCandidate15(body) {
		if h.nativePreviousResponsesEnabled15() {
			// Native admission prepares all ancestor/current files together.
			c.Next()
			return
		}
		handleStoredResponseError(c, service.ErrStoredResponseUnsupported)
		return
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil || key.GroupID == nil || key.UserID <= 0 || key.ID <= 0 {
		rejectManagedFilesIngress(c, http.StatusForbidden, "permission_denied", "A currently authorized group is required")
		return
	}
	platform := key.Group.Platform
	if resolved, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context()); ok {
		platform = resolved
	}
	if platform != service.PlatformOpenAI || !strings.HasSuffix(path, "/responses") {
		rejectManagedFilesUnsupported(c)
		return
	}
	if h.nativePreviousResponsesEnabled15() {
		store := gjson.GetBytes(body, "store")
		if !store.Exists() || store.Type != gjson.False {
			publicModel := gjson.GetBytes(body, "model").String()
			mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), key.GroupID, publicModel)
			forwardModel := openAIChannelForwardModel(mapping, publicModel)
			route, routeErr := h.gatewayService.ResolveBPSStoredResponseRoute(c.Request.Context(), key.GroupID, forwardModel, platform)
			if routeErr != nil {
				handleStoredResponseError(c, service.ErrBPSStoredResponseRoute)
				return
			}
			if route.Local {
				c.Next()
				return
			}
		}
	}
	if h.managedFiles == nil || !h.managedFiles.Enabled() {
		rejectManagedFilesUnsupported(c)
		return
	}
	owner := service.FileOwner{UserID: key.UserID, APIKeyID: key.ID, GroupID: *key.GroupID}
	plan, err := h.managedFiles.Prepare(c.Request.Context(), owner, body)
	if err != nil {
		status, code, message := http.StatusBadRequest, "managed_files_unavailable", "Managed file is unavailable"
		switch {
		case errors.Is(err, service.ErrFileNotFound):
			status, code, message = http.StatusNotFound, "file_not_found", "File not found"
		case errors.Is(err, service.ErrManagedFileBusy):
			status, code, message = http.StatusConflict, "managed_files_busy", "Managed file is busy"
		case errors.Is(err, service.ErrManagedFileUncertain):
			status, code, message = http.StatusConflict, "managed_files_uncertain", "Managed file upload outcome requires recovery"
		case errors.Is(err, service.ErrManagedFileBindingConflict):
			status, code, message = http.StatusConflict, "managed_files_binding_conflict", "Managed files belong to incompatible backends"
		}
		rejectManagedFilesIngress(c, status, code, message)
		return
	}
	if plan == nil {
		rejectManagedFilesUnsupported(c)
		return
	}
	defer plan.Close()
	c.Request = c.Request.WithContext(service.WithManagedFilePlan(c.Request.Context(), plan))
	c.Next()
}
func (h *OpenAIGatewayHandler) managedFilesEnabled() bool {
	return h != nil && h.cfg != nil && h.cfg.Gateway.Files.Enabled
}
func rejectManagedFilesUnsupported(c *gin.Context) {
	rejectManagedFilesIngress(c, http.StatusBadRequest, "managed_files_unsupported", managedFilesUnsupportedMessage)
}
func rejectManagedFilesIngress(c *gin.Context, status int, code, message string) {
	service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalPolicyDenied)
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
}

// Enumerating structural keys also sees duplicate and case-variant candidates
// accepted by other protocol decoders. Native document.source is intentionally
// outside this walk, including Anthropic file_ identifiers.
func managedMessageFileReference(body []byte) bool {
	found := false
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.String(), "messages") {
			return true
		}
		value.ForEach(func(_, message gjson.Result) bool {
			message.ForEach(func(key, content gjson.Result) bool {
				if strings.EqualFold(key.String(), "content") {
					found = managedContentFileReference(content, 0)
				}
				return !found
			})
			return !found
		})
		return !found
	})
	return found
}
func managedContentFileReference(content gjson.Result, depth int) bool {
	if depth > 128 {
		return true
	}
	found := false
	visit := func(part gjson.Result) bool {
		fileKind, directKind, imageKind, toolResult := false, false, false, false
		part.ForEach(func(k, v gjson.Result) bool {
			if strings.EqualFold(k.String(), "type") {
				fileKind = fileKind || strings.EqualFold(v.String(), "file")
				directKind = directKind || strings.EqualFold(v.String(), "input_file")
				imageKind = imageKind || strings.EqualFold(v.String(), "input_image")
				toolResult = toolResult || v.String() == "tool_result"
			}
			return true
		})
		part.ForEach(func(k, v gjson.Result) bool {
			if directKind || imageKind && strings.EqualFold(k.String(), "file_id") {
				found = true
			}
			if fileKind && strings.EqualFold(k.String(), "file") {
				v.ForEach(func(k, _ gjson.Result) bool {
					if strings.EqualFold(k.String(), "file_id") || strings.EqualFold(k.String(), "file_data") || strings.EqualFold(k.String(), "file_url") {
						found = true
					}
					return !found
				})
			}
			if toolResult && strings.EqualFold(k.String(), "content") {
				found = found || managedContentFileReference(v, depth+1)
			}
			return !found
		})
		return !found
	}
	if content.IsObject() {
		visit(content)
	} else if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool { return visit(part) })
	}
	return found
}
func (h *OpenAIGatewayHandler) managedFilesWSUnsupported(body []byte) bool {
	if !h.managedFilesEnabled() {
		return false
	}
	if service.HasManagedResponseFiles(body) {
		return true
	}
	found := false
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		switch {
		case strings.EqualFold(key.String(), "item"):
			found = managedContentFileReference(value, 0)
			if found {
				break
			}
			value.ForEach(func(key, content gjson.Result) bool {
				if strings.EqualFold(key.String(), "content") {
					found = managedContentFileReference(content, 0)
				}
				return !found
			})
		case strings.EqualFold(key.String(), "session"), strings.EqualFold(key.String(), "response"):
			found = service.HasManagedResponseFiles([]byte(value.Raw))
			if !found {
				value.ForEach(func(key, content gjson.Result) bool {
					if strings.EqualFold(key.String(), "content") {
						found = managedContentFileReference(content, 0)
					}
					return !found
				})
			}
		}
		return !found
	})
	return found
}
