package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func requireOpenAITiboProxyBinding(account *Account, proxyURL string) error {
	if account != nil && account.ProxyID != nil && strings.TrimSpace(proxyURL) == "" {
		return fmt.Errorf("Tibo account proxy is unavailable")
	}
	return nil
}

// Synthetic probes share the target fork's OAuth body, account namespace,
// fingerprint projection and final request builder with ordinary forwarding.
func (s *OpenAIGatewayService) buildOpenAITiboOAuthProbeRequest(ctx context.Context, account *Account) (*http.Request, error) {
	session := uuid.Must(uuid.NewV7()).String()
	turn := uuid.Must(uuid.NewV7()).String()
	installation := deriveStableUUIDv4("turn-state-hunter:" + codexAccountIdentityNamespace(account))
	metadata := map[string]any{
		"installation_id": installation, "session_id": session, "thread_id": session,
		"turn_id": turn, "root_turn_id": turn, "window_id": session + ":1",
		"context_window_id": uuid.Must(uuid.NewV7()).String(), "window_number": 1,
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	identity := resolveCodexOutboundIdentity("")
	headers := c.Request.Header
	headers.Set("User-Agent", identity.userAgent)
	headers.Set("originator", identity.originator)
	headers.Set("Content-Type", "application/json")
	headers.Set("session-id", session)
	headers.Set("thread-id", session)
	headers.Set("x-client-request-id", session)
	headers.Set("x-codex-installation-id", installation)
	headers.Set("x-codex-window-id", session+":1")
	headers.Set(openAIWSTurnMetadataHeader, string(encodedMetadata))
	if _, err := s.prepareCodexAccountIdentitySource(ctx, c, account); err != nil {
		return nil, err
	}
	decoded := openAITiboProbePayload()
	decoded["prompt_cache_key"] = session
	decoded["parallel_tool_calls"] = false
	decoded["tools"] = []any{}
	decoded["tool_choice"] = "auto"
	decoded["client_metadata"] = map[string]any{
		"session_id": session, "thread_id": session, "turn_id": turn,
		"root_turn_id": turn, "x-codex-installation-id": installation,
		"x-codex-window-id": session + ":1", "x-codex-turn-metadata": string(encodedMetadata),
	}
	result := applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{IsCodexCLI: true})
	if result.Error != nil {
		return nil, result.Error
	}
	applyCodexClientMetadata(decoded, account)
	applyCodexAccountIdentityClientMetadataMap(decoded, codexAccountIdentitySource(c, account), 0)
	fingerprint := resolveCodexFingerprintIDsForCaller(account, headers, 0)
	if fingerprint != nil {
		applyCodexFingerprintClientMetadata(decoded, fingerprint)
	}
	stageCodexFingerprintIDs(c, fingerprint)
	upstreamModel := openAITiboProbeModel
	if result.NormalizedModel != "" {
		upstreamModel = result.NormalizedModel
	}
	SetOpsUpstreamModel(c, upstreamModel)
	body, err := json.Marshal(decoded)
	if err != nil {
		return nil, err
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.buildUpstreamRequest(ctx, c, account, body, token, true, session, true)
}
