package handler

import "github.com/MACOS-DO/sub4api/internal/service"

func (h *OpenAIGatewayHandler) SetManagedFileResolver(resolver *service.ManagedFileResolver) {
	h.managedFiles = resolver
	h.gatewayService.SetManagedFileResolver(resolver)
}
