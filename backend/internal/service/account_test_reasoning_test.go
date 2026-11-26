package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetAccountTestReasoningUsesMappedModelMetadata(t *testing.T) {
	disabled := false
	account := ticketTestAccount(41)
	account.Credentials["model_mapping"] = map[string]any{
		"configured-alias": "upstream-model",
		"disabled-alias":   "gpt-6-astra",
	}
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Models: map[string]UpstreamModelMetadata{
			"upstream-model": {
				SupportedReasoningLevels: []string{"off", "medium", "extra_high", "xhigh", "invalid"},
				DefaultReasoningLevel:    "extra-high",
			},
			"gpt-6-astra": {Reasoning: &disabled},
		},
	})
	svc := &AccountTestService{}

	supported := svc.GetAccountTestReasoning(context.Background(), account, "configured-alias")
	require.Equal(t, []string{"none", "medium", "xhigh"}, supported.SupportedReasoningLevels)
	require.Equal(t, "xhigh", supported.DefaultReasoningLevel)

	for _, model := range []string{"disabled-alias", "unconfigured-model"} {
		result := svc.GetAccountTestReasoning(context.Background(), account, model)
		require.Equal(t, []string{}, result.SupportedReasoningLevels)
		require.Empty(t, result.DefaultReasoningLevel)
	}
}
