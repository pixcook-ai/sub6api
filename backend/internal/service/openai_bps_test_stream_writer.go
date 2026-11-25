package service

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
)

// bpsTestStreamWriter records the response like httptest.ResponseRecorder and
// also parses the Responses SSE as it is written, so the admin connectivity
// test can forward output text piece by piece instead of after completion.
type bpsTestStreamWriter struct {
	*httptest.ResponseRecorder
	pending     []byte
	deltas      int
	onDelta     func(string)
	onCompleted func(map[string]any)
}

func (w *bpsTestStreamWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	w.feed(p)
	return n, err
}

func (w *bpsTestStreamWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseRecorder.WriteString(s)
	w.feed([]byte(s))
	return n, err
}

func (w *bpsTestStreamWriter) feed(p []byte) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			return
		}
		line := strings.TrimSpace(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
		w.consume(line)
	}
}

func (w *bpsTestStreamWriter) consume(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	var event map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
		return
	}
	switch event["type"] {
	case "response.output_text.delta":
		if delta, ok := event["delta"].(string); ok && delta != "" {
			w.deltas++
			if w.onDelta != nil {
				w.onDelta(delta)
			}
		}
	case "response.completed", "response.done":
		if response, ok := event["response"].(map[string]any); ok && w.onCompleted != nil {
			w.onCompleted(response)
		}
	}
}
