package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func openAITiboBPSAccountReady(account *Account, now time.Time) bool {
	if account.Type != AccountTypeOAuth || !account.IsModelSupported(openAITiboProbeModel) ||
		!strings.EqualFold(account.GetMappedModel(openAITiboProbeModel), openAITiboProbeModel) {
		return false
	}
	state := account.OpenAIBPSCredentialState(now)
	if state == nil || state.Status == "expired" || state.Status == "revoked" || state.Status == "auth_failed" {
		return false
	}
	token, workspace := strings.TrimSpace(account.GetCredential("access_token")), strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	return token != "" && workspace != "" && !strings.ContainsAny(token+workspace, "\r\n")
}

func (s *OpenAIGatewayService) buildOpenAITiboBPSProbeRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if !openAITiboBPSAccountReady(account, time.Now()) {
		return nil, errOpenAITiboAccountUnavailable
	}
	// Reuse BPS translation with a fresh scope. This plain-text request does
	// not read or write business conversation/cache state.
	payload := openAITiboProbePayload()
	payload["prompt_cache_key"] = "tibo-bps-" + uuid.NewString()
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	probe, _ := gin.CreateTestContext(httptest.NewRecorder())
	probe.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	// A request-triggered caller can carry a business BPS binding. Replace that
	// routing scope locally so the probe cannot inherit its task/turn identity.
	ctx = context.WithValue(ctx, bpsRoutingContextKey{}, bpsRoutingContext{Scope: s.bpsScope(probe, body)})
	prepared, err := s.prepareOpenAIBPS(ctx, probe, account, body)
	if err != nil {
		return nil, err
	}
	return buildOpenAIBPSRequest(WithHTTPUpstreamRedirectsDisabled(ctx), account, prepared.Body)
}

func observeOpenAITiboBPSProbe(data []byte, contentType string) *openAITiboHTTPObservation {
	observation := &openAITiboHTTPObservation{answerClass: "none"}
	if len(data) == 0 || len(data) > openAITiboProbeBodyLimit {
		observation.failed = true
		return observation
	}
	consume := func(eventType string, payload []byte) {
		if strings.TrimSpace(string(payload)) == "[DONE]" {
			return
		}
		if !gjson.ValidBytes(payload) {
			observation.failed = true
			return
		}
		if eventType == "" {
			eventType = gjson.GetBytes(payload, "type").String()
		}
		if eventType == "response.done" {
			eventType = "response.completed"
		}
		if eventType == "response.completed" && gjson.GetBytes(payload, "response.status").String() != "completed" {
			observation.failed = true
			return
		}
		observation.event(payload, eventType)
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		forEachOpenAISSEFrame(string(data), consume)
	} else {
		var response map[string]any
		if bpsDecode(data, &response) != nil || response == nil {
			observation.failed = true
			return observation
		}
		payload, _ := json.Marshal(map[string]any{"response": response})
		consume("response.completed", payload)
	}
	return observation
}
