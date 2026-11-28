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
	request := struct {
		service.TiboAutomationSettings
		BPSHealthyGroupID  *int64 `json:"bps_healthy_group_id"`
		BPSDegradedGroupID *int64 `json:"bps_degraded_group_id"`
	}{TiboAutomationSettings: service.DefaultOpenAITiboSettings()}
	if err := c.ShouldBindJSON(&request); err != nil {
		response.BadRequest(c, "Invalid Tibo settings")
		return
	}
	settings := request.TiboAutomationSettings
	// Older clients do not send BPS fields. Preserve them while still allowing
	// a new client to explicitly clear either ID with zero.
	if request.BPSHealthyGroupID == nil || request.BPSDegradedGroupID == nil {
		current, err := h.settingService.GetOpenAITiboSettings(c.Request.Context())
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		settings.BPSHealthyGroupID, settings.BPSDegradedGroupID = current.BPSHealthyGroupID, current.BPSDegradedGroupID
	}
	if request.BPSHealthyGroupID != nil {
		settings.BPSHealthyGroupID = *request.BPSHealthyGroupID
	}
	if request.BPSDegradedGroupID != nil {
		settings.BPSDegradedGroupID = *request.BPSDegradedGroupID
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
	if account == nil || (!account.IsOpenAI() && (!account.IsOpenAIBPS() || account.Type != service.AccountTypeOAuth)) {
		response.BadRequest(c, "Tibo quality isolation applies to OpenAI or BPS OAuth accounts")
		return
	}
	if err := h.accountTestService.ClearOpenAITiboQualityIsolation(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"cleared": true})
}
