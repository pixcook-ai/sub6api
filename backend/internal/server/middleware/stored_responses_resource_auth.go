package middleware

import (
	"net/http"
	"strings"
)

// Management routes share resource authorization without making the bare
// Responses endpoint, inference requests, or unregistered subresources billing-free.
func isStoredResponseManagementRequest(method, path string) bool {
	if method != http.MethodGet && method != http.MethodDelete {
		return false
	}
	const prefix = "/v1/responses/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id := strings.TrimPrefix(path, prefix)
	if method == http.MethodGet && strings.HasSuffix(id, "/input_items") {
		id = strings.TrimSuffix(id, "/input_items")
	}
	return id != "" && !strings.Contains(id, "/")
}
