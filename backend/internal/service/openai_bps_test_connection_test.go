package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bpsTestStreamBody(deltas ...string) string {
	full := strings.Join(deltas, "")
	body := bpsEvent("response.created", map[string]any{"response": map[string]any{"id": "resp_test", "status": "in_progress", "model": "gpt-6-astra", "output": []any{}}})
	body += bpsEvent("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "status": "in_progress", "content": []any{}}})
	for _, delta := range deltas {
		body += bpsEvent("response.output_text.delta", map[string]any{"item_id": "msg_test", "output_index": 0, "content_index": 0, "delta": delta})
	}
	body += bpsEvent("response.output_item.done", map[string]any{"output_index": 0, "item": bpsText(full)})
	body += bpsEvent("response.completed", map[string]any{"response": bpsResponse(bpsText(full))})
	return body
}

func bpsTestEvents(body string) []gjson.Result {
	var events []gjson.Result
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			events = append(events, gjson.Parse(strings.TrimPrefix(line, "data: ")))
		}
	}
	return events
}

// The BPS connectivity test must show the model output piece by piece, then
// report token usage and elapsed time, like the regular OpenAI account test.
func TestOpenAIBPSAccountTestStreamsContentAndUsage(t *testing.T) {
	s, a := bpsFixture()
	stub := &bpsHTTPStub{status: 200, contentType: "text/event-stream", body: bpsTestStreamBody("O", "K")}
	s.httpUpstream = stub
	service := &AccountTestService{openaiGatewayService: s}
	c, rec := bpsContext(1, "/admin/test")

	require.NoError(t, service.testOpenAIBPSAccountConnection(c, a, "gpt-6-astra", "hi", "default"))

	require.True(t, gjson.GetBytes(stub.requestBody, "stream").Bool(), "upstream request must be streamed")
	events := bpsTestEvents(rec.Body.String())
	var contents []string
	usageAt, completeAt := -1, -1
	var usageText string
	for i, event := range events {
		switch event.Get("type").String() {
		case "content":
			contents = append(contents, event.Get("text").String())
		case "status":
			usageAt, usageText = i, event.Get("text").String()
		case "test_complete":
			completeAt = i
		}
	}
	require.Equal(t, []string{"O", "K"}, contents)
	require.NotEqual(t, -1, usageAt, "a usage status event is expected")
	require.Contains(t, usageText, "input 100")
	require.Contains(t, usageText, "output 12")
	require.Contains(t, usageText, "ms")
	require.Greater(t, completeAt, usageAt, "usage must be reported before test_complete")
	require.True(t, events[completeAt].Get("success").Bool())
}
