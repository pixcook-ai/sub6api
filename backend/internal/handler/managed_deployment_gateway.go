package handler

import (
	"bytes"
	"context"
	"errors"
	middleware2 "github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// ManagedDeploymentRouting runs after API-key authentication. Discovery does not
// create an inference session; WebSocket binding happens after its first frame.
func (h *OpenAIGatewayHandler) ManagedDeploymentRouting() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey, ok := middleware2.GetAPIKeyFromContext(c)
		if !ok || apiKey == nil || h == nil || h.gatewayService == nil {
			c.Next()
			return
		}
		d, e := h.gatewayService.ManagedDeploymentForGroup(c.Request.Context(), apiKey.GroupID)
		if e != nil {
			h.managedDeploymentError(c, e)
			return
		}
		if d == nil {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if managedDeploymentDiscoveryRequest(c.Request.Method, path) {
			c.Next()
			return
		}
		if !managedDeploymentEndpointSupported(c.Request.Method, path) {
			c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{"error": gin.H{"type": "managed_deployment_endpoint_unsupported", "code": "managed_deployment_endpoint_unsupported", "message": "This endpoint is not enabled for this managed deployment"}})
			return
		}
		if managedDeploymentWebSocketUpgrade(c.Request) {
			c.Next()
			return
		}
		var body []byte
		if c.Request.Body != nil && (managedDeploymentJSONBodyPath(path) || strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "json") || c.GetHeader("Content-Type") == "") {
			body, e = readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
			if e != nil {
				h.managedDeploymentError(c, service.ErrManagedDeploymentInvalid)
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		if !h.prepareManagedDeploymentRequest(c, apiKey, body) {
			return
		}
		original := c.Writer
		writer := &managedDeploymentResponseWriter{ResponseWriter: original, ctx: c.Request.Context(), gateway: h.gatewayService, validateOnly: c.Request.Method == http.MethodGet}
		c.Writer = writer
		c.Next()
		e = writer.finish()
		c.Writer = original
		if e != nil {
			slog.Warn("managed_deployment.response_owner_failed", "error", e)
		}
		if e != nil && !original.Written() {
			h.managedDeploymentError(c, e)
		}
	}
}
func (h *OpenAIGatewayHandler) prepareManagedDeploymentRequest(c *gin.Context, apiKey *service.APIKey, body []byte) bool {
	if e := h.bindManagedDeploymentRequest(c, apiKey, body); e != nil {
		h.managedDeploymentError(c, e)
		return false
	}
	return true
}
func (h *OpenAIGatewayHandler) prepareManagedDeploymentWebSocket(c *gin.Context, apiKey *service.APIKey, body []byte) bool {
	return h.bindManagedDeploymentRequest(c, apiKey, body) == nil
}
func (h *OpenAIGatewayHandler) bindManagedDeploymentRequest(c *gin.Context, apiKey *service.APIKey, body []byte) error {
	d, e := h.gatewayService.ManagedDeploymentForGroup(c.Request.Context(), apiKey.GroupID)
	if e != nil {
		return e
	}
	if d == nil {
		return nil
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok || apiKey.GroupID == nil || subject.UserID <= 0 {
		return service.ErrManagedDeploymentInvalid
	}
	explicit := ""
	for _, header := range []string{"session-id", "session_id", "X-Session-ID"} {
		for _, raw := range c.Request.Header.Values(header) {
			value := strings.TrimSpace(raw)
			if value == "" {
				continue
			}
			if explicit != "" && explicit != value {
				return service.ErrManagedDeploymentInvalid
			}
			explicit = value
		}
	}
	if explicit == "" {
		explicit = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	refs := []service.ManagedDeploymentResourceRef{}
	if len(body) > 0 {
		refs, e = service.ManagedDeploymentPayloadResources(body, false)
		if e != nil {
			return e
		}
	}
	parts := strings.Split(strings.Trim(c.Request.URL.Path, "/"), "/")
	for i, part := range parts {
		if i+1 >= len(parts) || parts[i+1] == "" {
			continue
		}
		switch part {
		case "files":
			refs = append(refs, service.ManagedDeploymentResourceRef{Kind: "file", ID: parts[i+1]})
		case "responses":
			if strings.HasPrefix(parts[i+1], "resp_") {
				refs = append(refs, service.ManagedDeploymentResourceRef{Kind: "response", ID: parts[i+1]})
			}
		}
	}
	session, e := h.gatewayService.BindManagedDeploymentRequest(c.Request.Context(), *apiKey.GroupID, subject.UserID, apiKey.ID, explicit, refs)
	if e != nil {
		return e
	}
	c.Request = c.Request.WithContext(service.WithManagedDeploymentSession(c.Request.Context(), session))
	return nil
}
func (h *OpenAIGatewayHandler) managedDeploymentError(c *gin.Context, e error) {
	status := http.StatusConflict
	code := "managed_deployment_context_required"
	message := "A trusted session or resource owner is required; rebuild context with a new explicit session"
	if errors.Is(e, service.ErrManagedDeploymentInvalid) {
		status = http.StatusBadRequest
		code = "managed_deployment_invalid"
		message = "Invalid managed deployment session or resource"
	} else if errors.Is(e, service.ErrManagedDeploymentUnavailable) {
		status = http.StatusServiceUnavailable
		code = "managed_deployment_unavailable"
		message = "Selected deployment account is unavailable; restore its authorization or scheduling explicitly"
	} else if errors.Is(e, service.ErrManagedDeploymentConflict) {
		code = "managed_deployment_conflict"
		message = "Managed deployment identity or revision changed"
	} else if !errors.Is(e, service.ErrManagedDeploymentContext) {
		status = http.StatusServiceUnavailable
		code = "managed_deployment_unavailable"
		message = "Durable deployment ownership is unavailable"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": code, "code": code, "message": message}})
}

// Buffer one complete JSON response or SSE event, persist ownership, then expose
// its IDs. No response ID reaches the client before the durable insert succeeds.
type managedDeploymentResponseWriter struct {
	gin.ResponseWriter
	ctx          context.Context
	gateway      *service.OpenAIGatewayService
	record       func(context.Context, []byte) error
	validateOnly bool
	pending      []byte
	failed       error
}

const managedDeploymentMaxResponseChunk = 32 << 20

func (w *managedDeploymentResponseWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}
func (w *managedDeploymentResponseWriter) Write(p []byte) (int, error) {
	if w.failed != nil {
		return 0, w.failed
	}
	kind := strings.ToLower(w.Header().Get("Content-Type"))
	if !strings.Contains(kind, "json") && !strings.Contains(kind, "text/event-stream") {
		return w.ResponseWriter.Write(p)
	}
	if len(w.pending)+len(p) > managedDeploymentMaxResponseChunk {
		w.failed = service.ErrManagedDeploymentUnavailable
		return 0, w.failed
	}
	w.pending = append(w.pending, p...)
	if strings.Contains(kind, "text/event-stream") {
		for {
			end := bytes.Index(w.pending, []byte("\n\n"))
			sep := 2
			if end < 0 {
				end = bytes.Index(w.pending, []byte("\r\n\r\n"))
				sep = 4
			}
			if end < 0 {
				break
			}
			event := w.pending[:end+sep]
			if e := w.recordEvent(event); e != nil {
				w.failed = e
				return 0, e
			}
			if _, e := w.ResponseWriter.Write(event); e != nil {
				w.failed = e
				return 0, e
			}
			w.pending = w.pending[end+sep:]
		}
	}
	return len(p), nil
}
func (w *managedDeploymentResponseWriter) recordEvent(event []byte) error {
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	return w.recordPayload(data)
}
func (w *managedDeploymentResponseWriter) Flush() {
	if w.failed == nil && len(w.pending) == 0 {
		w.ResponseWriter.Flush()
	}
}
func (w *managedDeploymentResponseWriter) finish() error {
	if w.failed != nil {
		return w.failed
	}
	if len(w.pending) == 0 {
		return nil
	}
	if strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream") {
		if e := w.recordEvent(w.pending); e != nil {
			return e
		}
	} else if w.Status() < 400 {
		if e := w.recordPayload(w.pending); e != nil {
			return e
		}
	}
	_, e := w.ResponseWriter.Write(w.pending)
	w.pending = nil
	return e
}

func (w *managedDeploymentResponseWriter) recordPayload(raw []byte) error {
	if w.record != nil {
		return w.record(w.ctx, raw)
	}
	if w.validateOnly {
		return w.gateway.ValidateManagedDeploymentOutputPayload(w.ctx, raw)
	}
	return w.gateway.RecordManagedDeploymentPayload(w.ctx, raw)
}

func managedDeploymentJSONBodyPath(path string) bool {
	return strings.Contains(path, "/responses") || strings.HasSuffix(path, "/chat/completions") || strings.Contains(path, "/messages") || strings.HasSuffix(path, "/embeddings") || strings.HasSuffix(path, "/alpha/search") || strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/videos") || strings.Contains(path, "/videos/") || strings.HasSuffix(path, "/live")
}
func (h *OpenAIGatewayHandler) validateManagedDeploymentHTTPBody(c *gin.Context, body []byte) bool {
	if e := h.gatewayService.ValidateManagedDeploymentPayload(c.Request.Context(), body); e != nil {
		h.managedDeploymentError(c, e)
		return false
	}
	return true
}

func managedDeploymentWebSocketUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	switch r.URL.Path {
	case "/responses", "/v1/responses", "/openai/v1/responses", "/backend-api/codex/responses":
	default:
		return false
	}
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

// Background jobs and live sessions do not carry this deployment's durable pin.
// Fail before dispatch instead of letting them enter an unscoped account pool.
func managedDeploymentEndpointSupported(method, path string) bool {
	path = managedDeploymentNormalizedPath(path)
	if path == "/responses" || strings.HasPrefix(path, "/responses/") {
		return method == http.MethodPost || method == http.MethodGet || method == http.MethodDelete
	}
	if path == "/files" || strings.HasPrefix(path, "/files/") {
		return method == http.MethodPost || method == http.MethodGet || method == http.MethodDelete
	}
	if method == http.MethodPost {
		switch path {
		case "/chat/completions", "/messages", "/messages/count_tokens", "/embeddings", "/images/generations", "/images/edits":
			return true
		}
	}
	return false
}

// Preserve ResponseController read deadlines through the ownership recorder,
// notably the fail-closed multipart upload commit path.
func (w *managedDeploymentResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func managedDeploymentNormalizedPath(path string) string {
	for _, prefix := range []string{"/backend-api/codex", "/openai"} {
		if strings.HasPrefix(path, prefix+"/") {
			path = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return path
}
func managedDeploymentDiscoveryRequest(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	path = managedDeploymentNormalizedPath(path)
	if path == "/models" || path == "/usage" || path == "/sub2api/billing" {
		return true
	}
	model := strings.TrimPrefix(path, "/models/")
	return model != path && model != "" && !strings.Contains(model, "/")
}
