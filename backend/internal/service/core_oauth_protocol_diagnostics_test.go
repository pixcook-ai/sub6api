package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

func TestCoreOAuthBPSFailureDiagnosticsWhitelist(t *testing.T) {
	const secret = "synthetic-code-state-token-email-subject-secret"
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tt := range []struct {
		name, code, stage string
		err               error
		status            int
		classification    CoreOAuthProtocolError
	}{
		{"endpoint rejection", "bps_token_endpoint_rejected", "token_endpoint", &openai.BPSOAuthError{Code: "bps_token_endpoint_rejected", StatusCode: 403}, 403, CoreOAuthProtocolError{}},
		{"claims rejection", "bps_subject_mismatch", "token_validation", &openai.BPSOAuthError{Code: "bps_subject_mismatch"}, 0, CoreOAuthProtocolError{}},
		{"known terminal rejection", "bps_invalid_grant", "token_endpoint", fmt.Errorf(secret+": %w", &openai.BPSOAuthError{Code: "bps_invalid_grant", StatusCode: 400, Permanent: true}), 400, CoreOAuthProtocolError{Permanent: true}},
		{"uncertain exchange", "bps_token_exchange_outcome_unknown", "token_endpoint", &openai.BPSOAuthError{Code: "bps_token_exchange_outcome_unknown", OutcomeUnknown: true}, 0, CoreOAuthProtocolError{OutcomeUnknown: true}},
		{"untrusted typed code", "bps_oauth_unclassified", "unknown", &openai.BPSOAuthError{Code: secret, StatusCode: 987654321, Permanent: true}, 0, CoreOAuthProtocolError{Permanent: true}},
		{"untyped error", "bps_oauth_unclassified", "unknown", errors.New(secret), 0, CoreOAuthProtocolError{OutcomeUnknown: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output.Reset()
			var classified *CoreOAuthProtocolError
			if err := coreOAuthBPSError(tt.err); !errors.As(err, &classified) || *classified != tt.classification {
				t.Fatalf("protocol failure classification changed")
			}
			if strings.Contains(output.String(), secret) {
				t.Fatal("OAuth diagnostic leaked untrusted error material")
			}
			var event map[string]any
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatalf("expected one structured failure diagnostic: %v", err)
			}
			want := map[string]any{
				"level": "WARN", "msg": "core_oauth.bps_protocol_failed",
				"provider": "openai_bps", "stage": tt.stage, "code": tt.code,
				"http_status": float64(tt.status), "permanent": tt.classification.Permanent, "outcome_unknown": tt.classification.OutcomeUnknown,
			}
			delete(event, "time")
			if len(event) != len(want) {
				t.Fatalf("unexpected diagnostic fields: got %d, want %d", len(event), len(want))
			}
			for key, expected := range want {
				if event[key] != expected {
					t.Errorf("diagnostic %s = %v, want %v", key, event[key], expected)
				}
			}
		})
	}
}
