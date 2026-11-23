package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// This reader removes private frames before ANY generic response handling,
// logging, tool correction, public usage merge or client model/ID rewrite.
type nativeTerminalBody15 struct {
	source       io.ReadCloser
	reader       *bufio.Reader
	ctx          context.Context
	now          func() time.Time
	maxFrame     int
	allowPrivate bool
	pending      []byte
	heldTerminal []byte
	ended        bool
	endErr       error
}

func newNativeTerminalBody15(ctx context.Context, source io.ReadCloser, now func() time.Time) (io.ReadCloser, error) {
	state := nativeTerminalAttemptFrom15(ctx)
	if (state != nil && state.gate == nil) || source == nil || now == nil {
		return nil, ErrStoredResponseInvalid
	}
	limit := 64 << 20
	if state != nil {
		limit = 2*base64.StdEncoding.EncodedLen(state.gate.maxBytes) + (32 << 10)
	}
	return &nativeTerminalBody15{source: source, reader: bufio.NewReaderSize(source, 32<<10), ctx: ctx, now: now, maxFrame: limit, allowPrivate: state != nil}, nil
}
func (b *nativeTerminalBody15) Close() error { return b.source.Close() }

// A transport failure or invalid trailer must invalidate reusable proof while
// retaining independently authenticated usage for failure settlement.
func (b *nativeTerminalBody15) fail() (int, error) {
	b.pending = nil
	b.heldTerminal = nil
	b.ended = true
	b.endErr = &StoredResponseLocalError{ErrStoredResponseInvalid}
	if state := nativeTerminalAttemptFrom15(b.ctx); state != nil {
		state.gate.reject()
		state.mu.Lock()
		state.delivery = nil
		state.mu.Unlock()
	}
	return 0, b.endErr
}

func (b *nativeTerminalBody15) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.ended {
			if b.endErr != nil {
				return 0, b.endErr
			}
			return 0, io.EOF
		}
		frame, eof, err := b.readFrame()
		if err != nil {
			return b.fail()
		}
		if len(frame) > 0 {
			drop, consumeErr := b.consumeFrame(frame)
			if consumeErr != nil {
				return b.fail()
			}
			if !drop {
				// A matching signed terminal is still provisional until the whole
				// HTTP body ends cleanly. Keep its bounded frame and any trailing
				// syntactic comments away from generic capture/Finalize meanwhile.
				if b.heldTerminal != nil || (b.allowPrivate && nativeTerminalDeliveryFromContext15(b.ctx) != nil) {
					if len(frame) > b.maxFrame-len(b.heldTerminal) {
						return b.fail()
					}
					b.heldTerminal = append(b.heldTerminal, frame...)
				} else {
					b.pending = frame
				}
			}
		}
		if eof {
			b.ended = true
			if err := finishNativeTerminalAttempt15(b.ctx); err != nil {
				return b.fail()
			}
			if len(b.heldTerminal) > 0 {
				b.pending = b.heldTerminal
				b.heldTerminal = nil
			}
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *nativeTerminalBody15) readFrame() ([]byte, bool, error) {
	var frame []byte
	lineStart := 0
	for {
		part, err := b.reader.ReadSlice('\n')
		if len(part) > b.maxFrame-len(frame) {
			return nil, false, ErrStoredResponseTooLarge
		}
		frame = append(frame, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF {
				return frame, true, nil
			}
			return nil, false, err
		}
		line := frame[lineStart:]
		lineStart = len(frame)
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return frame, false, nil
		}
	}
}
func (b *nativeTerminalBody15) consumeFrame(frame []byte) (bool, error) {
	var event string
	var data []byte
	hasData, hasEvent := false, false
	native := nativeTerminalAttemptFrom15(b.ctx) != nil
	for _, rawLine := range bytes.Split(frame, []byte{'\n'}) {
		line := bytes.TrimSuffix(rawLine, []byte{'\r'})
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		field, value, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			value = nil
		}
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "event":
			if hasEvent && (native || strings.HasPrefix(event, "bps.private.") || strings.HasPrefix(string(value), "bps.private.")) {
				return true, ErrStoredResponseInvalid
			}
			hasEvent = true
			event = string(value)
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			hasData = true
			data = append(data, value...)
		}
	}
	if !hasData {
		if strings.HasPrefix(event, "bps.private.") || (native && hasEvent) {
			return true, ErrStoredResponseInvalid
		}
		return false, nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if strings.HasPrefix(event, "bps.private.") || (native && hasEvent) {
			return true, ErrStoredResponseInvalid
		}
		return consumeNativeTerminalEvent15(b.ctx, event, data, b.now())
	}
	// Detect the private envelope by shape as well as event name. A malformed
	// BPS server frame must never turn private payloads into generic diagnostics.
	value, err := managedfiles.Parse(data)
	object, ok := value.(map[string]any)
	if err != nil || !ok {
		return true, ErrStoredResponseInvalid
	}
	privateShape := nativeTerminalPrivateObject15(object)
	if privateShape && event != managedfiles.NativeTerminalEvent15 {
		return true, ErrStoredResponseInvalid
	}
	if strings.HasPrefix(event, "bps.private.") {
		if !b.allowPrivate {
			return true, ErrStoredResponseInvalid
		}
		return consumeNativeTerminalEvent15(b.ctx, event, data, b.now())
	}
	payloadType, _ := object["type"].(string)
	if native && payloadType == "" {
		return true, ErrStoredResponseInvalid
	}
	if payloadType != "" {
		if event != "" && event != payloadType && (native || strings.HasPrefix(payloadType, "bps.private.")) {
			return true, ErrStoredResponseInvalid
		}
		event = payloadType
	}
	return consumeNativeTerminalEvent15(b.ctx, event, data, b.now())
}

// PrepareNativeTerminalResponse15 is called immediately after the activation
// ACK is verified. It never logs envelopes or sends them to generic decoders.
func prepareNativeTerminalResponse15(ctx context.Context, resp *http.Response, now func() time.Time) error {
	return prepareNativeTerminalResponseUnrecorded15(ctx, resp, now)
}

func prepareNativeTerminalResponseUnrecorded15(ctx context.Context, resp *http.Response, now func() time.Time) error {
	fail := func() error { return &StoredResponseLocalError{ErrStoredResponseInvalid} }
	if resp == nil || resp.Body == nil || now == nil {
		return fail()
	}
	media, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	state := nativeTerminalAttemptFrom15(ctx)
	if media == nativeTerminalMediaType15 && (state == nil || resp.StatusCode != http.StatusOK) {
		return fail()
	}
	if media == "text/event-stream" {
		body, err := newNativeTerminalBody15(ctx, resp.Body, now)
		if err != nil {
			return fail()
		}
		if resp.StatusCode != http.StatusOK {
			body.(*nativeTerminalBody15).allowPrivate = false
		}
		resp.Body = body
		return nil
	}
	if state != nil && resp.StatusCode == http.StatusOK && (mediaErr != nil || resp.Header.Get("Content-Encoding") != "") {
		return fail()
	}
	// Inspect all nonstreaming BPS statuses before generic diagnostics. This
	// also rejects a private envelope mislabeled as an ordinary error or JSON.
	limit := int64(managedfiles.MaxJSONBytes)
	if resp.StatusCode != http.StatusOK {
		limit = 1 << 20
	}
	if state != nil && resp.StatusCode == http.StatusOK {
		limit = int64(2*base64.StdEncoding.EncodedLen(state.gate.maxBytes) + (16 << 10))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	_ = resp.Body.Close()
	if err != nil || int64(len(raw)) > limit {
		return fail()
	}
	if media == nativeTerminalMediaType15 {
		delivery, err := state.gate.UnwrapJSON(resp.Header.Get("Content-Type"), raw, now())
		if err != nil {
			return fail()
		}
		state.mu.Lock()
		state.delivery = delivery
		state.mu.Unlock()
		observeNativeTerminalUsage15(ctx, delivery.gatewayTerminal)
		raw = delivery.gatewayTerminal
	} else {
		value, parseErr := managedfiles.Parse(raw)
		object, objectOK := value.(map[string]any)
		structured := bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("["))
		if objectOK && nativeTerminalPrivateObject15(object) {
			return fail()
		}
		if (media == "application/json" || structured) && (parseErr != nil || !objectOK) {
			return fail()
		}
		if state != nil && resp.StatusCode == http.StatusOK {
			if media != "application/json" || len(raw) > state.gate.maxBytes {
				return fail()
			}
			var response struct {
				Status string          `json:"status"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(raw, &response) != nil {
				return fail()
			}
			event := ""
			switch response.Status {
			case "incomplete":
				event = "response.incomplete"
			case "failed", "cancelled", "canceled":
				event = "response.failed"
			default:
				if len(response.Error) > 0 && string(response.Error) != "null" {
					event = "error"
				} else {
					return fail()
				}
			}
			if _, _, err := state.gate.Consume(event, raw, now()); err != nil {
				return fail()
			}
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.TransferEncoding = nil
	if state != nil && resp.StatusCode == http.StatusOK {
		resp.Header.Del("Content-Encoding")
		resp.Header.Set("Content-Length", strconv.Itoa(len(raw)))
		resp.Header.Set("Content-Type", "application/json")
	}
	return nil
}

func nativeTerminalPrivateObject15(object map[string]any) bool {
	if object["version"] == managedfiles.NativeTerminalContract15 {
		return true
	}
	for _, key := range []string{"snapshot_base64", "terminal_base64", "snapshot_sha256", "native_wire_sha256"} {
		if _, exists := object[key]; exists {
			return true
		}
	}
	return false
}
