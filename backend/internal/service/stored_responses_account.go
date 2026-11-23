package service

import (
	"errors"
	"net/url"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
)

const OpenAIEndpointCapabilityBPSLocalResponses OpenAIEndpointCapability = "bps_local_responses"

var ErrBPSStoredResponseRoute = errors.New("stored responses: backend route unavailable")

// BPSStoredResponsesBackendConfig is a trusted, comparable private binding. It
// does not imply Files support, and must never be serialized to public clients.
type BPSStoredResponsesBackendConfig struct {
	ControlURL  string
	ServiceKey  string
	ProxyURL    string
	Expected    managedfiles.Identity
	AccountID   int64
	Concurrency int
}

// BPSStoredResponsesConfigForAccount accepts only the explicitly configured
// native HTTP route whose complete output can be captured by the main gateway.
func BPSStoredResponsesConfigForAccount(a *Account) (BPSStoredResponsesBackendConfig, error) {
	var c BPSStoredResponsesBackendConfig
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey ||
		a.getExtraBool("openai_quality_guard_enabled") || a.getExtraBool("openai_raw_relay") || a.IsOpenAIPassthroughEnabled() ||
		a.IsOpenAIResponsesWebSocketV2Enabled() || openai_compat.ResolveResponsesSupport(a.Extra) != openai_compat.ResponsesSupportYes {
		return c, ErrBPSStoredResponseRoute
	}
	capabilities, _ := a.openAIEndpointCapabilitySet()
	if !capabilities[string(OpenAIEndpointCapabilityBPSLocalResponses)] ||
		!capabilities[string(OpenAIEndpointCapabilityResponses)] ||
		!capabilities[string(OpenAIEndpointCapabilityChatCompletions)] ||
		!a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) ||
		a.GetCredential("bps_responses_contract") != "local-responses/1" ||
		strings.TrimSpace(a.GetCredential("base_url")) == "" || strings.TrimSpace(a.GetCredential("api_key")) == "" {
		return c, ErrBPSStoredResponseRoute
	}
	c = BPSStoredResponsesBackendConfig{
		ControlURL: a.GetCredential("bps_responses_control_url"), ServiceKey: a.GetCredential("bps_responses_service_key"),
		Expected:  managedfiles.Identity{BackendID: a.GetCredential("bps_responses_backend_id"), SubjectFingerprint: a.GetCredential("bps_responses_subject_fingerprint"), CredentialGeneration: a.GetCredential("bps_responses_credential_generation")},
		AccountID: a.ID, Concurrency: a.Concurrency,
	}
	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.TrimSpace(c.ControlURL) != c.ControlURL ||
		!bpsFilesOpaque(c.ServiceKey, 4096) || managedfiles.ValidateIdentity(c.Expected) != nil || c.AccountID <= 0 || c.Concurrency <= 0 {
		return BPSStoredResponsesBackendConfig{}, ErrBPSStoredResponseRoute
	}
	if a.Proxy != nil {
		c.ProxyURL = a.Proxy.URL()
	}
	return c, nil
}

// A malformed local contract remains a declaration so it cannot silently fall
// back to the ordinary provider storage semantics.
func declaresBPSStoredResponses(a *Account) bool {
	if a == nil {
		return false
	}
	if _, present := a.Credentials["bps_responses_contract"]; present {
		return true
	}
	capabilities, _ := a.openAIEndpointCapabilitySet()
	return capabilities[string(OpenAIEndpointCapabilityBPSLocalResponses)]
}
