package service

import (
	"maps"
	"strings"
)

// Runtime observations cannot be authored by account forms, imports or duplicates.
func IsOpenAITiboManagedExtraKey(key string) bool {
	return strings.HasPrefix(key, "codex_tibo_verdict:") ||
		key == OpenAITiboQualityIsolatedExtraKey || key == OpenAITiboQualityIsolationAtExtraKey
}

// Observation history is exposed only through its bounded admin DTO.
func IsOpenAITiboPrivateExtraKey(key string) bool {
	return strings.HasPrefix(key, "codex_tibo_verdict:")
}

func MergeOpenAITiboExtra(extra, current map[string]any) map[string]any {
	merged := maps.Clone(extra)
	for key := range merged {
		if IsOpenAITiboManagedExtraKey(key) {
			delete(merged, key)
		}
	}
	for key, value := range current {
		if !IsOpenAITiboManagedExtraKey(key) {
			continue
		}
		if merged == nil {
			merged = make(map[string]any)
		}
		merged[key] = value
	}
	return merged
}
