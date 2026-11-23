package service

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func storedHTTPFixture(t *testing.T, db *captureTestDB) (*OpenAIGatewayService, context.Context, func() error) {
	t.Helper()
	cfg := DefaultStoredResponseConfig()
	cfg.Enabled = true
	stored, err := NewStoredResponseService(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NormalizeStoredResponseRequest([]byte("{\"model\":\"fixture\",\"input\":\"test\",\"store\":true}"))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := stored.Begin(context.Background(), StoredResponseOwner{1, 2, 3}, request, StoredResponseBinding{AccountID: 4, Scope: "private-binding"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cleanup := withStoredResponseCapture(context.Background(), capture)
	ctx = context.WithValue(ctx, storedResponseGenerationKey{}, &storedResponseGeneration{caller: ctx, accountID: 4, authorize: func(context.Context) error { return nil }})
	return &OpenAIGatewayService{storedResponses: stored}, ctx, cleanup
}

func TestStoredHTTPClaimForbidsSecondInferenceSend(t *testing.T) {
	sends := 0
	ctx := context.WithValue(context.Background(), storedResponseGenerationKey{}, &storedResponseGeneration{caller: context.Background(), accountID: 4, authorize: func(context.Context) error { sends++; return nil }})
	if err := ClaimStoredResponseRequest(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := ClaimStoredResponseRequest(ctx, 4); !errors.Is(err, ErrStoredResponseConflict) {
		t.Fatal("second inference send was allowed", err)
	}
	if sends != 1 {
		t.Fatal("authorization/send claim repeated", sends)
	}
}

func TestStoredHTTPPublicJSONCommitsBeforeDelivery(t *testing.T) {
	db := &captureTestDB{}
	service, ctx, cleanup := storedHTTPFixture(t, db)
	defer cleanup()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{\"id\":\"resp_remote\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"fixture\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}")
	}))
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := service.PrepareStoredResponseHTTPResponse(ctx, &Account{ID: 4}, response); err != nil {
		t.Fatal(err)
	}
	public, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if db.terminal == nil || db.terminal.State != "completed" {
		t.Fatal("completion was delivered before commit")
	}
	if strings.Contains(string(public), "resp_remote") || !strings.Contains(string(public), "resp_local_") || !strings.Contains(string(public), "\"input_tokens\":3") {
		t.Fatal("public identity or usage lost", string(public))
	}
}

func TestStoredHTTPFailedCommitWithholdsJSONAndSSETerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			db := &captureTestDB{finishErr: errors.New("write unavailable")}
			service, ctx, cleanup := storedHTTPFixture(t, db)
			defer cleanup()
			terminal := "{\"id\":\"resp_remote\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"fixture\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}"
			body, media := terminal, "application/json"
			if stream {
				body, media = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+terminal+"}\n\n", "text/event-stream"
			}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(body))}
			if err := service.PrepareStoredResponseHTTPResponse(ctx, &Account{ID: 4}, response); err != nil {
				t.Fatal(err)
			}
			public, err := io.ReadAll(response.Body)
			if err == nil || strings.Contains(string(public), "completed") {
				t.Fatal("uncommitted terminal escaped", string(public), err)
			}
			attempt := storedResponseAttemptFrom(ctx)
			if attempt == nil || attempt.capture.observedUsage == nil {
				t.Fatal("observed failure usage was discarded")
			}
		})
	}
}

func TestStoredHTTPFrameBoundIsEnforcedBeforeCapture(t *testing.T) {
	body := &storedResponsePublicBody{reader: nil, limit: 8}
	response := &http.Response{Body: io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 70<<10) + "\n\n"))}
	body.source = response.Body
	body.reader = bufio.NewReaderSize(response.Body, 512)
	if _, err := body.readFrame(); !errors.Is(err, ErrStoredResponseTooLarge) {
		t.Fatal("unbounded frame was accepted", err)
	}
}

func TestStoredHTTPCommitFailureKeepsUsageAndCannotFailover(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			db := &captureTestDB{finishErr: errors.New("database unavailable")}
			service, attemptCtx, cleanup := storedHTTPFixture(t, db)
			defer cleanup()
			terminal := `{"id":"resp_remote","object":"response","status":"completed","model":"fixture","output":[],"usage":{"input_tokens":3,"output_tokens":2}}`
			body, media := terminal, "application/json"
			if stream {
				body, media = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+terminal+"}\n\n", "text/event-stream"
			}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(body))}
			if err := service.PrepareStoredResponseHTTPResponse(attemptCtx, &Account{ID: 4}, response); err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			var usage *OpenAIUsage
			var err error
			// The generic decoder receives the caller context, not the private
			// attempt context. The body must carry failure counters across it.
			if stream {
				result, readErr := service.handleStreamingResponse(context.Background(), response, c, account, time.Now(), "fixture", "fixture")
				err, usage = readErr, result.usage
			} else {
				result, readErr := service.handleNonStreamingResponse(context.Background(), response, c, account, "fixture", "fixture")
				err = readErr
				if result != nil {
					usage = result.usage
				}
			}
			var local *StoredResponseLocalError
			var failover *UpstreamFailoverError
			if !errors.As(err, &local) || errors.As(err, &failover) || usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 2 {
				t.Fatalf("local failure lost usage or became retryable: usage=%+v err=%v", usage, err)
			}
			if recorder.Body.Len() != 0 || service.isOpenAIAccountRuntimeBlocked(account) {
				t.Fatal("failed terminal escaped or penalized the account", recorder.Body.String())
			}
		})
	}
}
