package admin

import (
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetOpenAITiboSettings(c *gin.Context) {
	settings, err := h.settingService.GetOpenAITiboSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *SettingHandler) UpdateOpenAITiboSettings(c *gin.Context) {
	settings := service.DefaultOpenAITiboSettings()
	if err := c.ShouldBindJSON(&settings); err != nil {
		response.BadRequest(c, "Invalid Tibo settings")
		return
	}
	if err := h.settingService.UpdateOpenAITiboSettings(c.Request.Context(), settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

func (h *AccountHandler) ClearTiboQualityIsolation(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account == nil || !account.IsOpenAI() {
		response.BadRequest(c, "Tibo quality isolation applies to OpenAI accounts")
		return
	}
	if err := h.accountTestService.ClearOpenAITiboQualityIsolation(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"cleared": true})
}
