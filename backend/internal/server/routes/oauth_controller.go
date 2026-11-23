package routes

import (
	"github.com/MACOS-DO/sub4api/internal/handler"
	adminhandler "github.com/MACOS-DO/sub4api/internal/handler/admin"
	"github.com/gin-gonic/gin"
)

func registerOAuthControllerRoutes(v1 *gin.RouterGroup, h *handler.Handlers) {
	var account *adminhandler.AccountHandler
	if h != nil && h.Admin != nil {
		account = h.Admin.Account
	}
	controller := v1.Group("/oauth-controller", adminhandler.NewOAuthControllerAuth(account))
	controller.GET("/accounts/:id/managed-credentials", account.GetManagedOAuthCredentials)
	controller.PUT("/accounts/:id/managed-credentials", account.InstallManagedOAuthCredentials)
	controller.GET("/accounts/:id/managed-credentials/auth-events", account.ListManagedOAuthAuthEvents)
}

func registerOAuthControllerAdminRoutes(admin *gin.RouterGroup, h *handler.Handlers) {
	admin.POST("/accounts/:id/oauth-controller-credentials", h.Admin.Account.RegisterOAuthControllerCredentials)
	admin.DELETE("/accounts/:id/oauth-controller-credentials/:operation_id", h.Admin.Account.RevokeOAuthControllerCredentials)
}
