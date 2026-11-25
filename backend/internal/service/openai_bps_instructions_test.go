package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// BPS echoes its internal Excel-agent system prompt in the response; it must never reach downstream clients.
func TestOpenAIBPSTransformResponseStripsUpstreamInstructions(t *testing.T) {
	s, a := bpsFixture()
	c, _ := bpsContext(1, "/v1/responses")
	r, err := s.prepareOpenAIBPS(c.Request.Context(), c, a, bpsRequestBody("hi", false))
	require.NoError(t, err)

	upstream := bpsResponse(bpsText("pong"))
	upstream["instructions"] = "You are the spreadsheet agent for the ChatGPT Excel Plugin."

	transformed, err := s.bpsTransformResponse(c.Request.Context(), a, r, upstream)
	require.NoError(t, err)
	require.NotContains(t, transformed, "instructions")

	output, ok := transformed["output"].([]any)
	require.True(t, ok)
	require.Len(t, output, 1)
	require.Equal(t, "message", output[0].(map[string]any)["type"])
	require.Equal(t, "completed", transformed["status"])
}
