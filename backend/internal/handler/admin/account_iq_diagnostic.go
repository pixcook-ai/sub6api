package admin

import (
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) StateProbe(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input struct {
		ModelID string `json:"model_id"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&input); err != nil {
			response.BadRequest(c, "Invalid request")
			return
		}
	}
	result, err := h.accountTestService.ProbeOpenAICodexState(c.Request.Context(), id, strings.TrimSpace(input.ModelID))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) ModelTraceTest(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	var input struct {
		ModelID         string `json:"model_id"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || strings.TrimSpace(input.ModelID) == "" || len(strings.TrimSpace(input.ModelID)) > 100 || !service.ValidIQReasoningEffort(input.ReasoningEffort) {
		response.BadRequest(c, "Invalid model or reasoning effort")
		return
	}
	if err := h.accountTestService.TestModelTraceAccountConnection(c, id, strings.TrimSpace(input.ModelID), input.ReasoningEffort); err != nil && !c.Writer.Written() {
		response.ErrorFrom(c, err)
	}
}
