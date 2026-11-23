package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/ip"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

// Deferred checks only retain immutable authenticated facts, never a reused Gin context.
type requestAuthorizationFacts struct {
	keyID, userID, groupID int64
	clientIP               string
}

type fileUploadReauthorizeKey struct{}

func captureRequestAuthorizationFacts(key *service.APIKey, clientIP string) requestAuthorizationFacts {
	facts := requestAuthorizationFacts{clientIP: clientIP}
	if key != nil {
		facts.keyID, facts.userID = key.ID, key.UserID
		if key.GroupID != nil {
			facts.groupID = *key.GroupID
		}
	}
	return facts
}

// NewResourceRequestAuthorizationMiddleware must run after APIKeyAuth.
func NewResourceRequestAuthorizationMiddleware(keys *service.APIKeyService, subscriptions *service.SubscriptionService, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := GetAPIKeyFromContext(c)
		if !ok || key == nil || key.Key == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"type": "authentication_error", "message": "request authorization unavailable"}})
			return
		}
		trustForwarded := cfg != nil && cfg.TrustForwardedIPForAPIKeyACL()
		facts := captureRequestAuthorizationFacts(key, ip.GetSecurityClientIP(c, trustForwarded))
		credential := key.Key
		ctx := c.Request.Context()
		if isBPSRequestAuthorizationPath(c.Request.Method, c.Request.URL.Path) {
			ctx = service.WithBPSRequestReauthorize(ctx, newBPSRequestReauthorize(keys, subscriptions, cfg, credential, facts))
		}
		ctx = context.WithValue(ctx, fileUploadReauthorizeKey{}, newFileUploadReauthorize(keys, subscriptions, credential, facts))
		c.Request = c.Request.WithContext(ctx)
		if isResourceRequestAuthorizationPath(c.Request.Method, c.Request.URL.Path) {
			fresh, _, err := freshRequestAuthorization(ctx, keys, subscriptions, credential, facts, true)
			if err != nil {
				status := http.StatusServiceUnavailable
				message := "resource authorization unavailable"
				if errors.Is(err, service.ErrFileAuthorizationRevoked) {
					status, message = http.StatusForbidden, "resource authorization revoked"
				}
				c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "permission_error", "message": message}})
				return
			}
			c.Set(string(ContextKeyAPIKey), fresh)
			ctx = context.WithValue(ctx, ctxkey.Group, fresh.Group)
			c.Request = c.Request.WithContext(ctx)
		}
		c.Next()
	}
}

// FileUploadReauthorize is nil when the authenticated middleware was not installed.
func FileUploadReauthorize(c *gin.Context) func(context.Context) error {
	if c == nil || c.Request == nil {
		return nil
	}
	check, _ := c.Request.Context().Value(fileUploadReauthorizeKey{}).(func(context.Context) error)
	return check
}

func isResourceRequestAuthorizationPath(method, path string) bool {
	path = strings.TrimRight(path, "/")
	for _, root := range []string{"", "/v1", "/openai/v1", "/backend-api/codex"} {
		if path == root+"/files" || strings.HasPrefix(path, root+"/files/") {
			return true
		}
		if (method == http.MethodGet || method == http.MethodDelete) && strings.HasPrefix(path, root+"/responses/") {
			return true
		}
	}
	return false
}

func isBPSRequestAuthorizationPath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	path = strings.TrimRight(path, "/")
	for _, root := range []string{"/v1/responses", "/openai/v1/responses", "/responses", "/backend-api/codex/responses"} {
		if path == root || path == root+"/compact" {
			return true
		}
	}
	switch path {
	case "/v1/chat/completions", "/chat/completions", "/v1/messages":
		return true
	}
	return false
}

func freshRequestAuthorization(ctx context.Context, keys *service.APIKeyService, subscriptions *service.SubscriptionService, credential string, facts requestAuthorizationFacts, openAIOnly bool) (*service.APIKey, *service.UserSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	fresh, err := keys.GetForFilePublication(ctx, credential)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
		return nil, nil, service.ErrFileAuthorizationUnavailable
	}
	if fresh == nil || facts.keyID <= 0 || facts.userID <= 0 || facts.groupID <= 0 || credential == "" || fresh.Key != credential || fresh.ID != facts.keyID || fresh.UserID != facts.userID || fresh.GroupID == nil || *fresh.GroupID != facts.groupID || fresh.User == nil || fresh.User.ID != facts.userID || fresh.User.DeletedAt != nil || !fresh.User.IsActive() || fresh.Group == nil || fresh.Group.ID != facts.groupID || (fresh.Status != service.StatusActive && fresh.Status != service.StatusAPIKeyQuotaExhausted) || fresh.IsExpired() {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if _, _, ok := validateAPIKeyGroupAvailable(fresh); !ok {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if !validateAPIKeyGroupAllowed(fresh) || openAIOnly && fresh.Group.Platform != service.PlatformOpenAI && fresh.Group.Platform != service.PlatformOpenAIBPS {
		return nil, nil, service.ErrFileAuthorizationRevoked
	}
	if len(fresh.IPWhitelist) > 0 || len(fresh.IPBlacklist) > 0 {
		allowed, _ := ip.CheckIPRestrictionWithCompiledRules(facts.clientIP, fresh.CompiledIPWhitelist, fresh.CompiledIPBlacklist)
		if !allowed {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
	}
	var sub *service.UserSubscription
	if fresh.Group.IsSubscriptionType() {
		sub, err = subscriptions.GetForFilePublication(ctx, facts.userID, facts.groupID)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil && !errors.Is(err, service.ErrSubscriptionNotFound) {
			return nil, nil, service.ErrFileAuthorizationUnavailable
		}
		if err != nil || sub == nil || sub.UserID != facts.userID || sub.GroupID != facts.groupID || sub.DeletedAt != nil || !sub.IsActive() {
			return nil, nil, service.ErrFileAuthorizationRevoked
		}
	}
	return fresh, sub, ctx.Err()
}

func checkFreshRequestSpending(ctx context.Context, subscriptions *service.SubscriptionService, cfg *config.Config, fresh *service.APIKey, sub *service.UserSubscription, facts requestAuthorizationFacts) error {
	if fresh.Status != service.StatusActive || fresh.IsQuotaExhausted() || fresh.RateLimit5h > 0 && fresh.EffectiveUsage5h() >= fresh.RateLimit5h || fresh.RateLimit1d > 0 && fresh.EffectiveUsage1d() >= fresh.RateLimit1d || fresh.RateLimit7d > 0 && fresh.EffectiveUsage7d() >= fresh.RateLimit7d {
		return service.ErrFileAuthorizationRevoked
	}
	if sub != nil {
		maintenance, validationErr := subscriptions.ValidateAndCheckLimits(sub, fresh.Group)
		if maintenance {
			originalID := sub.ID
			var err error
			sub, err = subscriptions.EnsureWindowMaintenance(ctx, sub)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return service.ErrFileAuthorizationUnavailable
			}
			if sub == nil || sub.ID != originalID || !sub.IsActive() || sub.UserID != facts.userID || sub.GroupID != facts.groupID || sub.DeletedAt != nil {
				return service.ErrFileAuthorizationRevoked
			}
			_, validationErr = subscriptions.ValidateAndCheckLimits(sub, fresh.Group)
		}
		if validationErr != nil {
			return service.ErrFileAuthorizationRevoked
		}
	} else if apiKeyBalanceBelowAuthThreshold(fresh.User.Balance, cfg) {
		return service.ErrFileAuthorizationRevoked
	}
	return ctx.Err()
}

func newFileUploadReauthorize(keys *service.APIKeyService, subscriptions *service.SubscriptionService, credential string, facts requestAuthorizationFacts) func(context.Context) error {
	return func(ctx context.Context) error {
		// Upload publication is unbilled, but never retains revoked identity or ACLs.
		_, _, err := freshRequestAuthorization(ctx, keys, subscriptions, credential, facts, true)
		return err
	}
}

func newBPSRequestReauthorize(keys *service.APIKeyService, subscriptions *service.SubscriptionService, cfg *config.Config, credential string, facts requestAuthorizationFacts) service.BPSRequestReauthorize {
	return func(ctx context.Context, requestedModel string) error {
		fresh, sub, err := freshRequestAuthorization(ctx, keys, subscriptions, credential, facts, false)
		if err == nil {
			if requestedModel == "" || strings.TrimSpace(requestedModel) != requestedModel || !fresh.Group.ModelAllowlist.Allows(requestedModel) || (fresh.Group.Platform != service.PlatformOpenAI && fresh.Group.Platform != service.PlatformOpenAIBPS && fresh.Group.Platform != service.PlatformComposite) {
				err = service.ErrFileAuthorizationRevoked
			} else {
				err = checkFreshRequestSpending(ctx, subscriptions, cfg, fresh, sub, facts)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, service.ErrFileAuthorizationRevoked) {
			return service.ErrBPSRequestAuthorizationRevoked
		}
		if err != nil {
			return service.ErrBPSRequestAuthorizationUnavailable
		}
		return nil
	}
}

// IsUnbilledResourceRequest does not exempt authentication or fresh ACL checks.
func IsUnbilledResourceRequest(method, path string) bool {
	path = strings.TrimRight(path, "/")
	for _, root := range []string{"", "/v1", "/openai/v1", "/backend-api/codex"} {
		if path == root+"/files" && (method == http.MethodGet || method == http.MethodPost) {
			return true
		}
		if strings.HasPrefix(path, root+"/files/") && (method == http.MethodGet || method == http.MethodDelete) {
			return true
		}
		if strings.HasPrefix(path, root+"/responses/") && (method == http.MethodGet || method == http.MethodDelete) {
			return true
		}
	}
	return false
}
