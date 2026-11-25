package routes

import (
	"github.com/MACOS-DO/sub4api/internal/handler"
	"github.com/gin-gonic/gin"
)

// Core OAuth stays inside the authenticated, audited administrator group.
func registerCoreOAuthRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	group := admin.Group("/core-oauth")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Next()
	})
	group.GET("/capabilities", h.Admin.CoreOAuth.Capabilities)
	group.GET("/profiles", h.Admin.CoreOAuth.ListProfiles)
	group.POST("/profiles", h.Admin.CoreOAuth.CreateProfile)
	group.POST("/runtime-shells", h.Admin.Account.CreateCoreRuntimeShell)
	group.GET("/profiles/:ref", h.Admin.CoreOAuth.GetProfile)
	group.POST("/profiles/:ref/authorize", h.Admin.CoreOAuth.Authorize)
	group.POST("/profiles/:ref/complete", h.Admin.CoreOAuth.Complete)
	group.POST("/profiles/:ref/cancel", h.Admin.CoreOAuth.Cancel)
	group.POST("/profiles/:ref/refresh", h.Admin.CoreOAuth.Refresh)
	group.POST("/profiles/:ref/handoff-invitations", h.Admin.CoreOAuth.CreateHandoffInvitation)
	group.POST("/profiles/:ref/handoffs/accept", h.Admin.CoreOAuth.AcceptProtectedHandoff)
	group.GET("/profiles/:ref/handoffs/:operation_id", h.Admin.CoreOAuth.GetHandoff)
	group.POST("/profiles/:ref/handoffs/:operation_id/abandon", h.Admin.CoreOAuth.AbandonHandoff)
}
