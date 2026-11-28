package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTiboRuntimeExtraPreservesCommittedObservations(t *testing.T) {
	current := map[string]any{
		"codex_tibo_verdict:http":            map[string]any{"verdict": "degraded"},
		"codex_tibo_verdict:auto_group":      map[string]any{"started_at": "2026-11-20T00:00:00Z"},
		OpenAITiboQualityIsolatedExtraKey:    true,
		OpenAITiboQualityIsolationAtExtraKey: "2026-11-20T00:00:00Z",
		"label":                              "old",
	}
	incoming := map[string]any{"label": "new", OpenAITiboQualityIsolatedExtraKey: false, "codex_tibo_verdict:http": "forged"}
	merged := MergeOpenAITiboExtra(incoming, current)
	require.Equal(t, true, merged[OpenAITiboQualityIsolatedExtraKey])
	require.Equal(t, map[string]any{"verdict": "degraded"}, merged["codex_tibo_verdict:http"])
	require.Equal(t, "new", merged["label"])
	require.Equal(t, false, incoming[OpenAITiboQualityIsolatedExtraKey], "input map must stay untouched")
	require.NotContains(t, MergeOpenAITiboExtra(incoming, nil), OpenAITiboQualityIsolatedExtraKey)
	require.NotContains(t, MergeOpenAITiboExtra(incoming, nil), "codex_tibo_verdict:http")
}

func TestTiboRuntimeExtraStaleFormCannotRestoreClearedIsolation(t *testing.T) {
	merged := MergeOpenAITiboExtra(map[string]any{OpenAITiboQualityIsolatedExtraKey: true}, map[string]any{OpenAITiboQualityIsolatedExtraKey: false})
	require.Equal(t, false, merged[OpenAITiboQualityIsolatedExtraKey])
	require.True(t, IsOpenAITiboPrivateExtraKey("codex_tibo_verdict:http"))
	require.False(t, IsOpenAITiboPrivateExtraKey(OpenAITiboQualityIsolatedExtraKey), "admin must see quality isolation independently of scheduling")
}
