package handler

import (
	"errors"
	"net/http"
	"strings"

	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// StoredResponsesIngress follows authentication and managed deployment binding.
// The route is selected by server state before a client storage flag is read.
func (h *OpenAIGatewayHandler) StoredResponsesIngress(c *gin.Context) {
	for name := range c.Request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-bps-responses-") || strings.HasPrefix(strings.ToLower(name), "x-bps-native-") {
			c.Request.Header.Del(name)
		}
	}
	if h == nil || h.cfg == nil || !h.cfg.Gateway.StoredResponses.Enabled || c.Request.Method != http.MethodPost || !strings.HasSuffix(c.Request.URL.Path, "/responses") {
		c.Next()
		return
	}
	owner, ok := storedResponseOwner(c)
	if !ok {
		return
	}
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		status := http.StatusBadRequest
		if _, tooLarge := extractMaxBytesError(err); tooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		storedResponseError(c, status, "invalid_request_error", "Invalid Responses request body", "invalid_request_error")
		return
	}
	resetBody := func(value []byte) {
		c.Request.Body = pkghttputil.NewPrereadBody(value)
		c.Request.ContentLength = int64(len(value))
		c.Request.TransferEncoding = nil
		c.Request.Header.Del("Content-Encoding")
		c.Request.Header.Del("Content-Length")
	}
	resetBody(body)
	model := gjson.GetBytes(body, "model").String()
	mapping, restricted := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), &owner.GroupID, model)
	if restricted {
		storedResponseError(c, http.StatusNotFound, "invalid_request_error", "Model is not available for this group", "model_not_found")
		return
	}
	forwardModel := openAIChannelForwardModel(mapping, model)
	previousID, current, localPrevious, err := parseLocalPreviousRequest15(body)
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if localPrevious && !h.nativePreviousResponsesEnabled15() {
		handleStoredResponseError(c, service.ErrStoredResponseUnsupported)
		return
	}
	if localPrevious {
		body = current
	}
	route, err := h.gatewayService.ResolveBPSStoredResponseRoute(c.Request.Context(), &owner.GroupID, forwardModel, service.PlatformOpenAI)
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if !route.Local {
		if localPrevious {
			handleStoredResponseError(c, service.ErrBPSStoredResponseRoute)
			return
		}
		c.Next()
		return
	}
	shouldStore, err := localStoredResponseStoreIntent(body, h.cfg.Gateway.StoredResponses.LocalDefaultStore())
	if err != nil {
		handleStoredResponseError(c, err)
		return
	}
	if localPrevious && !shouldStore {
		handleStoredResponseError(c, unsupportedLocalNativeContinuation())
		return
	}
	if shouldStore {
		planCtx, release := pkghttputil.WithRequestBodyMemoryScope(c.Request.Context())
		defer release()
		if err := pkghttputil.ReserveRequestBodyMemory(planCtx, int64(len(body))*32+(64<<10)); err != nil {
			handleStoredResponseError(c, err)
			return
		}
		c.Request = c.Request.WithContext(planCtx)
		var snapshot []byte
		if localPrevious {
			parent, err := h.gatewayService.ResolveStoredResponseNativeParent15(planCtx, owner, previousID, model, forwardModel)
			if err != nil {
				handleStoredResponseError(c, err)
				return
			}
			if parent == nil || parent.Account == nil {
				handleStoredResponseError(c, service.ErrStoredResponseNotFound)
				return
			}
			if session, managed := service.ManagedDeploymentSessionFromContext(planCtx); managed && session.Binding.AccountID != parent.Account.ID {
				handleStoredResponseError(c, service.ErrBPSRequestAuthorizationRevoked)
				return
			}
			snapshot = parent.Snapshot
			c.Request = c.Request.WithContext(service.WithStoredResponseAccountPin(c.Request.Context(), parent.Account.ID))
		}
		var plan *service.StoredResponsePlan
		if h.nativePreviousResponsesEnabled15() {
			plan, err = service.NewStoredResponseNativePlan15(owner, body, previousID, snapshot)
		} else {
			plan, err = service.NewStoredResponsePlan(owner, body)
		}
		if err != nil {
			if errors.Is(err, service.ErrStoredResponseUnsupported) {
				err = unsupportedLocalStoredResponseCombination()
			}
			handleStoredResponseError(c, err)
			return
		}
		if service.ManagedFilePlanFromContext(c.Request.Context()) != nil {
			handleStoredResponseError(c, unsupportedLocalStoredResponseCombination())
			return
		}
		c.Request = c.Request.WithContext(service.WithStoredResponsePlan(c.Request.Context(), plan))
	}
	body, err = sjson.SetBytes(body, "store", false)
	if err != nil {
		handleStoredResponseError(c, service.ErrStoredResponseInvalid)
		return
	}
	resetBody(body)
	c.Next()
}
