package admin

import (
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"

	"github.com/gin-gonic/gin"
)

// OpenAIBatchLoginHandler exposes the batch "email----password----2FA" OAuth login.
type OpenAIBatchLoginHandler struct {
	svc    *service.OpenAIBatchLoginService
	reauth *service.OpenAIReauthService
}

func NewOpenAIBatchLoginHandler(svc *service.OpenAIBatchLoginService, reauth *service.OpenAIReauthService) *OpenAIBatchLoginHandler {
	return &OpenAIBatchLoginHandler{svc: svc, reauth: reauth}
}

// OpenAIBatchLoginRequest is the request body. Text is never echoed back.
type OpenAIBatchLoginRequest struct {
	Text            string   `json:"text" binding:"required"`
	Targets         []string `json:"targets" binding:"required"`
	ProxyID         *int64   `json:"proxy_id"`
	Concurrency     int      `json:"concurrency"`
	Priority        int      `json:"priority"`
	BPSGroupIDs     []int64  `json:"bps_group_ids"`
	CodexGroupIDs   []int64  `json:"codex_group_ids"`
	SaveCredentials bool     `json:"save_credentials"`
}

// parseBatchLoginText splits lines into entries. Each non-empty line must have
// exactly three "----" separated fields; anything else rejects the whole batch.
func parseBatchLoginText(text string) ([]service.BatchLoginEntry, bool) {
	var entries []service.BatchLoginEntry
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "----")
		if len(parts) != 3 {
			return nil, false
		}
		entries = append(entries, service.BatchLoginEntry{
			Email:      strings.TrimSpace(parts[0]),
			Password:   strings.TrimSpace(parts[1]),
			TOTPSecret: parts[2],
		})
	}
	return entries, true
}

// Start creates a batch login job.
// POST /api/v1/admin/openai/batch-login
func (h *OpenAIBatchLoginHandler) Start(c *gin.Context) {
	var req OpenAIBatchLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request")
		return
	}
	entries, ok := parseBatchLoginText(req.Text)
	if !ok {
		response.BadRequest(c, "Each line must be email----password----2FA secret")
		return
	}
	job, err := h.svc.Start(c.Request.Context(), &service.BatchLoginInput{
		Entries: entries, Targets: req.Targets, ProxyID: req.ProxyID,
		AccountConcurrency: req.Concurrency, Priority: req.Priority,
		BPSGroupIDs: req.BPSGroupIDs, CodexGroupIDs: req.CodexGroupIDs, SaveCredentials: req.SaveCredentials,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Accepted(c, job)
}

// Get returns a job snapshot.
// GET /api/v1/admin/openai/batch-login/:id
func (h *OpenAIBatchLoginHandler) Get(c *gin.Context) {
	job, ok := h.svc.Get(c.Param("id"))
	if !ok {
		response.NotFound(c, "Batch login job not found")
		return
	}
	response.Success(c, job)
}

// Cancel cancels a running job.
// DELETE /api/v1/admin/openai/batch-login/:id
func (h *OpenAIBatchLoginHandler) Cancel(c *gin.Context) {
	if !h.svc.Cancel(c.Param("id")) {
		response.NotFound(c, "Batch login job not found")
		return
	}
	response.Success(c, gin.H{"canceled": true})
}

// GetCredentialStatus reports whether a login credential is stored for the account.
// GET /api/v1/admin/openai/accounts/:id/login-credential
func (h *OpenAIBatchLoginHandler) GetCredentialStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	saved, err := h.reauth.HasCredential(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"saved": saved})
}

// DeleteCredential removes the stored login credential of the account.
// DELETE /api/v1/admin/openai/accounts/:id/login-credential
func (h *OpenAIBatchLoginHandler) DeleteCredential(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if err := h.reauth.DeleteCredential(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}
