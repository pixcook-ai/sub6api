package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

func TestCoreOAuthCodexRefreshErrorClassification(t *testing.T) {
	invalidGrant := []byte(`{"error":"invalid_grant"}`)
	for _, tt := range []struct {
		name string
		err  error
		want CoreOAuthProtocolError
	}{
		{"wrapped invalid grant", fmt.Errorf("refresh: %w", openai.NewRefreshTokenError(http.StatusBadRequest, invalidGrant)), CoreOAuthProtocolError{Permanent: true}},
		{"unauthorized", openai.NewRefreshTokenError(http.StatusUnauthorized, nil), CoreOAuthProtocolError{Permanent: true}},
		{"rate limited", openai.NewRefreshTokenError(http.StatusTooManyRequests, nil), CoreOAuthProtocolError{}},
		{"server error with terminal-looking code", openai.NewRefreshTokenError(http.StatusBadGateway, []byte(`{"error":"refresh_token_reused"}`)), CoreOAuthProtocolError{OutcomeUnknown: true}},
		{"untyped invalid grant", errors.New("invalid_grant"), CoreOAuthProtocolError{OutcomeUnknown: true}},
		{"timeout", context.DeadlineExceeded, CoreOAuthProtocolError{OutcomeUnknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := coreOAuthCodexRefreshError(tt.err); *got != tt.want {
				t.Fatalf("classification = %+v, want %+v", *got, tt.want)
			}
		})
	}
}
