package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSAccountIsModelSupportedUsesConfiguredModelKeysAsWhitelist(t *testing.T) {
	tests := []struct {
		name        string
		mapping     map[string]any
		model       string
		wantSupport bool
	}{
		{name: "empty mapping is unrestricted", mapping: map[string]any{}, model: "gpt-6-astra", wantSupport: true},
		{name: "identity mapping admits its model", mapping: map[string]any{"gpt-6-astra": "gpt-6-astra"}, model: "gpt-6-astra", wantSupport: true},
		{name: "identity mapping rejects unlisted model", mapping: map[string]any{"gpt-6-astra": "gpt-6-astra"}, model: "gpt-5.6-sol", wantSupport: false},
		{name: "alias admits its client-facing key", mapping: map[string]any{"public-alias": "gpt-6-astra"}, model: "public-alias", wantSupport: true},
		{name: "alias does not admit its target key", mapping: map[string]any{"public-alias": "gpt-6-astra"}, model: "gpt-6-astra", wantSupport: false},
		{name: "wildcard admits matching client-facing models", mapping: map[string]any{"gpt-*": "gpt-6-astra"}, model: "gpt-5.6-sol", wantSupport: true},
		{name: "wildcard rejects nonmatching client-facing models", mapping: map[string]any{"gpt-*": "gpt-6-astra"}, model: "claude-opus-4.6", wantSupport: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := Account{
				Platform: PlatformOpenAIBPS,
				Credentials: map[string]any{
					"model_mapping": test.mapping,
				},
			}

			require.Equal(t, test.wantSupport, account.IsModelSupported(test.model))
		})
	}
}
