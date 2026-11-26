package service

import (
	"context"
	"strings"
)

// AccountTestReasoning describes the supported request values for one model.
// An empty list leaves reasoning at the provider default.
type AccountTestReasoning struct {
	SupportedReasoningLevels []string `json:"supported_reasoning_levels"`
	DefaultReasoningLevel    string   `json:"default_reasoning_level"`
}

func (s *AccountTestService) GetAccountTestReasoning(ctx context.Context, account *Account, modelID string) AccountTestReasoning {
	result := AccountTestReasoning{SupportedReasoningLevels: []string{}}
	target := strings.TrimSpace(modelID)
	if account == nil || target == "" {
		return result
	}
	if !account.IsOpenAIPassthroughEnabled() {
		target = account.GetMappedModel(target)
	}
	if s != nil && s.openaiGatewayService != nil && !account.IsSyntheticUITest() {
		gateway := s.openaiGatewayService
		var catalog *OpenAIModelsResponse
		var err error
		switch {
		case account.IsOpenAIOAuth():
			version := CodexCanonicalClientVersion()
			if gateway.settingService != nil {
				version = gateway.settingService.GetOpenAICodexClientVersion(ctx)
			}
			catalog, err = gateway.FetchCodexModelsManifest(ctx, account, version, "")
		case account.IsOpenAIApiKey():
			catalog, err = gateway.FetchOpenAIModelsList(ctx, account)
		}
		if err == nil && catalog != nil {
			_, metadata, parseErr := extractUpstreamModelCatalog(catalog.Body, false)
			if parseErr == nil {
				if live, ok := accountTestReasoningFromMetadata(metadata[target]); ok {
					return live
				}
			}
		}
	}
	if metadata, found := account.GetUpstreamModelMetadata(target); found {
		if saved, ok := accountTestReasoningFromMetadata(metadata); ok {
			return saved
		}
	}
	descriptor := newConfiguredCodexModelDescriptor(target)
	for _, level := range descriptor.SupportedReasoningLevels {
		result.SupportedReasoningLevels = append(result.SupportedReasoningLevels, level.Effort)
	}
	if len(result.SupportedReasoningLevels) == 1 && result.SupportedReasoningLevels[0] == "none" {
		return AccountTestReasoning{SupportedReasoningLevels: []string{}}
	}
	if len(result.SupportedReasoningLevels) > 0 && descriptor.DefaultReasoningLevel != nil {
		result.DefaultReasoningLevel = *descriptor.DefaultReasoningLevel
	}
	return result
}

func accountTestReasoningFromMetadata(metadata UpstreamModelMetadata) (AccountTestReasoning, bool) {
	result := AccountTestReasoning{SupportedReasoningLevels: []string{}}
	if metadata.Reasoning != nil && !*metadata.Reasoning {
		return result, true
	}
	levels := normalizeReasoningLevels(metadata.SupportedReasoningLevels)
	if len(levels) == 0 {
		return result, false
	}
	result.SupportedReasoningLevels = levels
	result.DefaultReasoningLevel = levels[0]
	preferred := normalizeReasoningLevel(metadata.DefaultReasoningLevel)
	for _, level := range levels {
		if level == preferred {
			result.DefaultReasoningLevel = level
			break
		}
	}
	return result, true
}
