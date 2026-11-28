package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityConnectionTestPromptAndOutputLimit(t *testing.T) {
	svc := &AntigravityGatewayService{}
	const question = "How many candies guarantee a matching pair?\nOnly give the number."
	for _, protocol := range []struct {
		name  string
		model string
		build func(string, string, ...string) ([]byte, error)
	}{
		{"Gemini", "gemini-3.1-pro-high", svc.buildGeminiTestRequest},
		{"Claude", "claude-sonnet-4-6", svc.buildClaudeTestRequest},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			for _, test := range []struct {
				name      string
				prompts   []string
				wantText  string
				wantLimit int64
			}{
				{"connectivity", nil, ".", 1},
				{"blank prompt", []string{" \n "}, ".", 1},
				{"question", []string{question}, question, 8192},
			} {
				t.Run(test.name, func(t *testing.T) {
					body, err := protocol.build("test-project", protocol.model, test.prompts...)
					require.NoError(t, err)
					require.Equal(t, "test-project", gjson.GetBytes(body, "project").String())
					require.Equal(t, protocol.model, gjson.GetBytes(body, "model").String())
					require.Equal(t, "user", gjson.GetBytes(body, "request.contents.0.role").String())
					require.Equal(t, test.wantText, gjson.GetBytes(body, "request.contents.0.parts.0.text").String())
					require.Equal(t, test.wantLimit, gjson.GetBytes(body, "request.generationConfig.maxOutputTokens").Int())
				})
			}
		})
	}
}
