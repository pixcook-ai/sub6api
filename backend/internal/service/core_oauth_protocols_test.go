package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

func TestCoreOAuthBPSRegionErrorHasSafeProfileCode(t *testing.T) {
	err := coreOAuthBPSError(&openai.BPSOAuthError{Code: "bps_region_unsupported", StatusCode: http.StatusForbidden, Permanent: true})
	var classified *CoreOAuthProtocolError
	if !errors.As(err, &classified) || classified == nil || classified.SafeCode != "CORE_OAUTH_REGION_UNSUPPORTED" || classified.OutcomeUnknown || !classified.Permanent {
		t.Fatalf("region error classification = %#v", classified)
	}

	unknown := coreOAuthBPSError(&openai.BPSOAuthError{Code: "provider-private-code", StatusCode: http.StatusForbidden})
	if !errors.As(unknown, &classified) || classified == nil || classified.SafeCode != "" || classified.OutcomeUnknown || classified.Permanent {
		t.Fatalf("unknown provider error classification = %#v", classified)
	}
}

type coreOAuthExchangeFailureClient struct{ calls int }

func (c *coreOAuthExchangeFailureClient) ExchangeCode(context.Context, string, string, string, string, string) (*openai.TokenResponse, error) {
	c.calls++
	return nil, context.DeadlineExceeded
}
func (*coreOAuthExchangeFailureClient) RefreshToken(context.Context, string, string) (*openai.TokenResponse, error) {
	panic("unexpected refresh")
}
func (*coreOAuthExchangeFailureClient) RefreshTokenWithClientID(context.Context, string, string, string) (*openai.TokenResponse, error) {
	panic("unexpected refresh")
}

func TestCoreOAuthCodexExchangeSeparatesLocalStateFromUnknownOutcome(t *testing.T) {
	for _, validState := range []bool{false, true} {
		t.Run(fmt.Sprint(validState), func(t *testing.T) {
			client := &coreOAuthExchangeFailureClient{}
			protocol := NewCoreOAuthCodexProtocol(NewOpenAIOAuthService(nil, client))
			attempt, err := protocol.Begin(context.Background(), CoreOAuthTransport{})
			if err != nil {
				t.Fatal(err)
			}
			authorize, err := url.Parse(attempt.AuthorizationURL)
			if err != nil {
				t.Fatal(err)
			}
			state := "synthetic-wrong-state"
			wantCalls := 0
			if validState {
				state = authorize.Query().Get("state")
				wantCalls = 1
			}
			_, err = protocol.ExchangeCallback(context.Background(), attempt, attempt.RedirectURI+"?code=synthetic-code&state="+url.QueryEscape(state), CoreOAuthIdentity{}, CoreOAuthTransport{})
			var classified *CoreOAuthProtocolError
			if !errors.As(err, &classified) || classified.OutcomeUnknown != validState {
				t.Fatalf("state valid=%v: got %v, want unknown=%v", validState, err, validState)
			}
			if client.calls != wantCalls {
				t.Fatalf("token requests=%d, want %d", client.calls, wantCalls)
			}
		})
	}
}
