package service

import (
	"context"

	"github.com/gin-gonic/gin"
)

// Connectivity tests consume their stream before returning, so the probe owns
// the same request lease as inference for its complete lifetime.
func (s *AccountTestService) beginAccountTestRequestProxy(c *gin.Context, account *Account) (context.Context, func(), error) {
	ctx := c.Request.Context()
	if account == nil || !account.OpenAIRequestProxyEnabled() {
		return ctx, func() {}, nil
	}
	if s.openaiGatewayService == nil {
		return ctx, func() {}, errOpenAIRequestProxyUnavailable
	}
	original := c.Request
	scoped, release, err := s.openaiGatewayService.beginOpenAIRequestProxy(ctx, nil, account)
	if err == nil {
		c.Request = c.Request.WithContext(scoped)
	}
	return scoped, func() {
		release()
		c.Request = original
	}, err
}
