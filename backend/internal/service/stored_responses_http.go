package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// PrepareStoredResponseRequest runs after the final upstream request is built.
func (s *OpenAIGatewayService) PrepareStoredResponseRequest(ctx context.Context, account *Account, request *http.Request) (*http.Request, context.Context, func() error, error) {
	plan := StoredResponsePlanFromContext(ctx)
	if plan == nil {
		return request, ctx, func() error { return nil }, nil
	}
	if plan.native15 != nil && openAIManagedDeploymentNoReplay(ctx) {
		client, err := scopeManagedDeploymentWirePayload(ctx, plan.Request.UpstreamBody)
		if err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
		normalized, err := NormalizeStoredResponseNativeRequest15(client)
		if err != nil {
			return nil, ctx, nil, &StoredResponseLocalError{err}
		}
		normalized.Store = true
		plan = &StoredResponsePlan{Owner: plan.Owner, Request: normalized, ClientRequest: normalized.UpstreamBody, native15: plan.native15}
	}
	return s.admitStoredResponse(ctx, account, request, plan)
}

// ClaimStoredResponseRequest precedes the sole upstream inference send.
func ClaimStoredResponseRequest(ctx context.Context, accountID int64) error {
	return claimStoredResponseGeneration(ctx, accountID)
}

// PrepareStoredResponseHTTPResponse filters native material before generic
// decoders, diagnostic capture or client delivery can observe response bytes.
func (s *OpenAIGatewayService) PrepareStoredResponseHTTPResponse(ctx context.Context, account *Account, response *http.Response) error {
	if !isStoredResponseGeneration(ctx) {
		return nil
	}
	if response == nil || response.Body == nil || s == nil || s.storedResponses == nil {
		return &StoredResponseLocalError{ErrStoredResponseInvalid}
	}
	if !verifyStoredResponseAck(ctx, response.Header) {
		return &StoredResponseLocalError{ErrStoredResponseInvalid}
	}
	if nativeTerminalAttemptFrom15(ctx) != nil {
		if err := prepareNativeTerminalResponse15(ctx, response, time.Now); err != nil {
			return err
		}
	}
	stripStoredResponsePrivateHeaders(response.Header)
	stripNativeResponsePrivateHeaders15(response.Header)
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	response.TransferEncoding = nil
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (media != "application/json" && media != "text/event-stream") {
		return &StoredResponseLocalError{ErrStoredResponseInvalid}
	}
	response.Body = &storedResponsePublicBody{
		source: response.Body, reader: bufio.NewReaderSize(response.Body, 32<<10),
		ctx: ctx, service: s, stream: media == "text/event-stream",
		limit: s.storedResponses.cfg.Limits.MaxResponseBytes,
	}
	return nil
}

type storedResponsePublicBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	ctx     context.Context
	service *OpenAIGatewayService
	stream  bool
	limit   int64
	pending []byte
	ended   bool
}

func (b *storedResponsePublicBody) Close() error { return b.source.Close() }
func (b *storedResponsePublicBody) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.ended {
			return 0, io.EOF
		}
		var raw []byte
		var err error
		if !b.stream {
			b.ended = true
			raw, err = io.ReadAll(io.LimitReader(b.reader, b.limit+1))
			if err == nil && int64(len(raw)) > b.limit {
				err = ErrStoredResponseTooLarge
			}
			if err == nil {
				raw, err = captureStoredResponseJSON(b.ctx, raw)
			}
			if err == nil {
				err = b.service.RecordManagedDeploymentPayload(b.ctx, raw)
			}
		} else {
			raw, err = b.readFrame()
			if errors.Is(err, io.EOF) {
				b.ended = true
				if len(raw) == 0 {
					if finishErr := finishNativeTerminalAttempt15(b.ctx); finishErr != nil {
						return 0, withStoredResponseFailureUsage(b.ctx, &StoredResponseLocalError{finishErr})
					}
					return 0, io.EOF
				}
				err = nil
			}
			if err == nil {
				raw, err = b.captureFrame(raw)
			}
		}
		if err != nil {
			b.ended = true
			return 0, withStoredResponseFailureUsage(b.ctx, &StoredResponseLocalError{err})
		}
		b.pending = raw
	}
	n := copy(destination, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *storedResponsePublicBody) readFrame() ([]byte, error) {
	var frame []byte
	lineLength := 0
	for {
		part, err := b.reader.ReadSlice('\n')
		if int64(len(frame))+int64(len(part)) > b.limit+(64<<10) {
			return nil, ErrStoredResponseTooLarge
		}
		frame = append(frame, part...)
		lineLength += len(part)
		if err == nil {
			if lineLength <= 2 && (bytes.Equal(part, []byte("\n")) || bytes.Equal(part, []byte("\r\n"))) {
				return frame, nil
			}
			lineLength = 0
		}
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return frame, err
		}
	}
}

func (b *storedResponsePublicBody) captureFrame(frame []byte) ([]byte, error) {
	var event string
	var data [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			event = strings.TrimSpace(string(line[6:]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
		}
	}
	if len(data) == 0 {
		return frame, nil
	}
	raw := bytes.Join(data, []byte("\n"))
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		if err := finishNativeTerminalAttempt15(b.ctx); err != nil {
			return nil, err
		}
		return frame, nil
	}
	if event == "" {
		value, err := decodeStoredResponseJSON(raw)
		if err != nil {
			return nil, err
		}
		if object, ok := value.(map[string]any); ok {
			event, _ = object["type"].(string)
		}
	}
	public, err := captureStoredResponseEvent(b.ctx, event, raw)
	if err != nil {
		return nil, err
	}
	if err := b.service.RecordManagedDeploymentPayload(b.ctx, public); err != nil {
		return nil, err
	}
	return append(append([]byte("event: "+event+"\ndata: "), public...), []byte("\n\n")...), nil
}
