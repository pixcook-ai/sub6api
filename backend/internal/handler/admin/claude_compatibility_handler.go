package admin

import (
	"strconv"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// GetClaudeOAuthCompatibility returns a recent, safe local observation without probing upstream.
func (h *AccountHandler) GetClaudeOAuthCompatibility(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account == nil || account.Platform != service.PlatformAnthropic || account.Type != service.AccountTypeOAuth {
		response.BadRequest(c, "Claude compatibility observations require an Anthropic OAuth account")
		return
	}
	response.Success(c, gin.H{
		"account_id":       accountID,
		"attribution_mode": "transparent",
		"observation":      service.GetClaudeOAuthCompatibilityDiagnostic(accountID),
	})
}
