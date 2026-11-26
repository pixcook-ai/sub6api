package admin

import (
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) PelicanTest(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var req struct {
		ModelID         string `json:"model_id"`
		Prompt          string `json:"prompt"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request")
		return
	}
	req.ModelID = strings.TrimSpace(req.ModelID)
	if req.ModelID == "" || len(req.ModelID) > 200 || strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > 64<<10 || strings.ContainsRune(req.Prompt, 0) {
		response.BadRequest(c, "Invalid model or test prompt")
		return
	}
	if !service.ValidIQReasoningEffort(req.ReasoningEffort) {
		response.BadRequest(c, "Invalid reasoning effort")
		return
	}
	if err := h.accountTestService.TestPelicanAccountConnection(c, accountID, req.ModelID, req.Prompt, req.ReasoningEffort); err != nil && !c.Writer.Written() {
		response.ErrorFrom(c, err)
	}
}

func (h *AccountHandler) GetTestModelReasoning(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	modelID := strings.TrimSpace(c.Query("model_id"))
	if modelID == "" || len(modelID) > 200 {
		response.BadRequest(c, "Invalid model ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil || account == nil {
		response.NotFound(c, "Account not found")
		return
	}
	response.Success(c, h.accountTestService.GetAccountTestReasoning(c.Request.Context(), account, modelID))
}
