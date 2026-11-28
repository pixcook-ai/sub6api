package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type bpsRepairUpstream struct {
	HTTPUpstream
	bodies    []string
	responses []string
}

func (u *bpsRepairUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	response := u.responses[len(u.bodies)]
	u.bodies = append(u.bodies, string(body))
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response))}, nil
}

func TestOpenAIBPSForwardRepairsMalformedToolTransport(t *testing.T) {
	s, account := bpsFixture()
	raw := "const result = 2 + 3;\nresult;"
	bad := bpsNative("rejected", "", nil)
	bad["arguments"] = bpsJSON(map[string]any{"summary": "Prepare calculation", "code": raw})
	good := bpsNative("corrected", "", nil)
	good["arguments"] = bpsJSON(map[string]any{"summary": "codex2api.custom/evaluate", "code": raw})
	first := bpsResponse(bad)
	first["id"] = "resp_original"
	second := bpsResponse(good)
	second["id"] = "resp_repair"
	upstream := &bpsRepairUpstream{responses: []string{
		bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_original", "status": "in_progress"}}) +
			bpsEvent("response.completed", map[string]any{"response": first}),
		bpsEvent("response.completed", map[string]any{"response": second}),
		bpsEvent("response.completed", map[string]any{"response": bpsResponse(bpsText("five"))}),
	}}
	s.httpUpstream = upstream
	c, recorder := bpsContext(1, "/v1/responses")
	tools := []any{map[string]any{"name": "evaluate", "type": "custom"}}
	input := []any{bpsMessage("user", "Calculate the result")}
	body := []byte(bpsJSON(map[string]any{"model": "gpt-6-astra", "input": input, "tools": tools, "stream": true}))
	result, err := s.Forward(c.Request.Context(), c, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	stream := recorder.Body.String()
	require.Equal(t, 1, strings.Count(stream, "event: response.created"))
	require.Equal(t, 1, strings.Count(stream, "event: response.completed"))
	require.NotContains(t, stream, "response.failed")
	require.NotContains(t, stream, "resp_repair")
	require.NotContains(t, stream, "rejected")
	require.Contains(t, stream, "corrected")
	require.Equal(t, 200, result.Usage.InputTokens)
	require.Equal(t, 24, result.Usage.OutputTokens)
	require.Equal(t, "resp_original", result.ResponseID)
	repair := upstream.bodies[1]
	require.Equal(t, gjson.Get(upstream.bodies[0], "metadata.turn_id").String(), gjson.Get(repair, "metadata.turn_id").String())
	require.Equal(t, "2", gjson.Get(repair, "metadata.agent_iteration").String())
	require.Contains(t, repair, "invalid_client_tool_transport")

	for _, line := range strings.Split(stream, "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(payload, "type").String() == "response.completed" {
			call := gjson.Get(payload, "response.output.0")
			require.Equal(t, raw, call.Get("input").String())
			input = append(input, call.Value(), map[string]any{"type": "custom_tool_call_output", "call_id": "corrected", "output": "five"})
		}
	}
	require.Len(t, input, 3)
	c, _ = bpsContext(1, "/v1/responses")
	body = []byte(bpsJSON(map[string]any{"model": "gpt-6-astra", "input": input, "tools": tools}))
	_, err = s.Forward(c.Request.Context(), c, account, body)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	replay := upstream.bodies[2]
	require.Equal(t, "3", gjson.Get(replay, "metadata.agent_iteration").String())
	require.Contains(t, replay, "invalid_client_tool_transport")
	require.Equal(t, "corrected", gjson.Get(replay, "input.@reverse.0.call_id").String())
}

func TestOpenAIBPSForwardRejectsUnsafeToolCorrection(t *testing.T) {
	const raw = "calculate(2 + 3)"
	for _, tc := range []struct {
		name, rejected, corrected, wantError string
		wantAttempts                         int
	}{
		{name: "changed raw input", rejected: raw, corrected: "calculate(2 + 4)", wantError: "bps_tool_repair_failed", wantAttempts: 2},
		{name: "still malformed", rejected: raw, corrected: raw, wantError: "bps_tool_repair_failed", wantAttempts: 2},
		{name: "batch encoded as JSON", rejected: `[{"name":"evaluate","args":"calculate(2 + 3)"}]`, wantError: "bps_invalid_tool_call", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, account := bpsFixture()
			bad := bpsNative("rejected", "", nil)
			bad["arguments"] = bpsJSON(map[string]any{"code": tc.rejected})
			corrected := bpsNative("corrected", "", nil)
			args := map[string]any{"code": tc.corrected}
			if tc.name != "still malformed" {
				args["summary"] = "codex2api.custom/evaluate"
			}
			corrected["arguments"] = bpsJSON(args)
			upstream := &bpsRepairUpstream{responses: []string{
				bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_original", "status": "in_progress"}}) +
					bpsEvent("response.completed", map[string]any{"response": bpsResponse(bad)}),
				bpsEvent("response.completed", map[string]any{"response": bpsResponse(corrected)}),
			}}
			s.httpUpstream = upstream
			c, recorder := bpsContext(1, "/v1/responses")
			body := []byte(bpsJSON(map[string]any{
				"model": "gpt-6-astra", "input": "calculate", "stream": true,
				"tools": []any{map[string]any{"name": "evaluate", "type": "custom"}},
			}))
			result, err := s.Forward(c.Request.Context(), c, account, body)
			require.Error(t, err)
			require.Len(t, upstream.bodies, tc.wantAttempts)
			require.Equal(t, tc.wantAttempts*100, result.Usage.InputTokens)
			stream := recorder.Body.String()
			require.Contains(t, stream, tc.wantError)
			require.Equal(t, 1, strings.Count(stream, "event: response.failed"))
			require.NotContains(t, stream, "event: response.completed")
			require.NotContains(t, stream, "custom_tool_call")
			require.NotContains(t, stream, raw)
		})
	}
}
