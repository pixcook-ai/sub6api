package service

import (
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

func storedResponsesTestAccount(id, groupID int64) Account {
	return Account{
		ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
		Credentials: map[string]any{
			"api_key": "sk-local-test", "base_url": "https://gateway.example",
			"openai_capabilities":    []string{"chat_completions", "responses", "bps_local_responses"},
			"bps_responses_contract": "local-responses/1", "bps_responses_control_url": "https://gateway.example",
			"bps_responses_service_key": "service-test", "bps_responses_backend_id": "backend-test",
			"bps_responses_subject_fingerprint":   "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"bps_responses_credential_generation": "generation-test",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)},
	}
}

func TestStoredResponsesAccountRequiresExplicitIndependentContract(t *testing.T) {
	a := storedResponsesTestAccount(14001, 14002)
	cfg, err := BPSStoredResponsesConfigForAccount(&a)
	require.NoError(t, err)
	require.Equal(t, a.ID, cfg.AccountID)
	require.Equal(t, "https://gateway.example", cfg.ControlURL)
	require.Equal(t, "backend-test", cfg.Expected.BackendID)
	require.True(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityBPSLocalResponses))
	require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityBPSFiles))
}
