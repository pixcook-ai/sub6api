package managedfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	pkghttputil "github.com/MACOS-DO/sub4api/internal/pkg/httputil"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type responsesClientTestUpstream struct{ calls int }

func (u *responsesClientTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	return http.DefaultClient.Do(req)
}

func TestResponsesPreflightClientAuthenticatesAndFreezesExactWire(t *testing.T) {
	wire := json.RawMessage(`{"model": "fixture", "input": "hello", "store":false}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/internal/bps-responses/v1/preflight" || r.Header.Get("X-Bps-Service-Key") != "private-test-key" {
			t.Error("invalid private preflight transport")
			w.WriteHeader(401)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			WireRequest   json.RawMessage `json:"wire_request"`
			ClientRequest json.RawMessage `json:"client_request"`
		}
		if err = json.Unmarshal(raw, &request); err != nil {
			t.Error(err)
			return
		}
		if string(request.WireRequest) != string(wire) || string(request.ClientRequest) != string(wire) {
			t.Error("exact request bytes changed")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":"bps-responses/1","identity":{"backend_id":"backend-1","subject_fingerprint":"sha256:%s","credential_generation":"generation-1"},"policy_version":"policy-v1","admission_id":"responses-%s","expires_at":%d,"wire_sha256":"00553099474cf8bc197e128334f56e96a648490e6a0278a3a595762eb08038d8","binding_sha256":"2e6316724f3a8c343f601103dc0d063496ccdfef5965c6f7dedffce361b4f5e3"}`, strings.Repeat("a", 64), strings.Repeat("b", 64), time.Now().Unix()+30)
	}))
	defer server.Close()
	upstream := &responsesClientTestUpstream{}
	client, err := NewResponsesPreflightClient(ResponsesClientConfig{ControlURL: server.URL, ServiceKey: "private-test-key", Expected: Identity{BackendID: "backend-1", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "generation-1"}, AccountID: 7, Concurrency: 1}, upstream)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release := pkghttputil.WithRequestBodyMemoryBudget(context.Background(), pkghttputil.NewBodyMemoryBudget(1<<20))
	defer release()
	result, err := client.Preflight(ctx, "user-1", "group-1", wire, wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := pkghttputil.ReserveRequestBodyMemory(ctx, 1<<20); err != nil {
		t.Fatal("successful preflight retained transient envelope budget")
	}
	if result.WireSHA256 != "00553099474cf8bc197e128334f56e96a648490e6a0278a3a595762eb08038d8" || upstream.calls != 1 {
		t.Fatal("invalid admission or preflight retry")
	}
}

func TestResponsesPreflightPolicyErrorStrictPublicContract(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"max_output_tokens","message":"The requested capability is not supported by this gateway."}}`
	cases := []struct {
		name, body string
		status     int
		headers    []string
		typed      bool
	}{
		{"policy", body, 400, []string{"rejected-v1"}, true},
		{"missing-header", body, 400, nil, false},
		{"duplicate-header", body, 400, []string{"rejected-v1", "rejected-v1"}, false},
		{"wrong-status", body, 503, []string{"rejected-v1"}, false},
		{"duplicate-key", strings.Replace(body, `"param":`, `"param":"private", "param":`, 1), 400, []string{"rejected-v1"}, false},
		{"case-alias", strings.Replace(body, `"param":`, `"Param":`, 1), 400, []string{"rejected-v1"}, false},
		{"raw-message", strings.Replace(body, responsesPolicyMessage, "private upstream token https://private.invalid", 1), 400, []string{"rejected-v1"}, false},
		{"unsafe-param", strings.Replace(body, "max_output_tokens", "https://private.invalid", 1), 400, []string{"rejected-v1"}, false},
		{"unknown-code", strings.Replace(body, "unsupported_parameter", "internal_private_code", 1), 400, []string{"rejected-v1"}, false},
		{"unknown-field", strings.Replace(body, `"param":`, `"native_id":"private", "param":`, 1), 400, []string{"rejected-v1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, h := range tc.headers {
					w.Header().Add("X-Bps-Request-Policy", h)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			upstream := &responsesClientTestUpstream{}
			client, err := NewResponsesPreflightClient(ResponsesClientConfig{ControlURL: server.URL, ServiceKey: "private-key", Expected: Identity{BackendID: "backend-1", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "gen-1"}, AccountID: 7, Concurrency: 1}, upstream)
			if err != nil {
				t.Fatal(err)
			}
			wire := []byte(`{"model":"fixture","input":"hello","store":false}`)
			_, err = client.Preflight(context.Background(), "user-1", "group-1", wire, wire)
			var policy *ResponsesPreflightError
			if err == nil || errors.As(err, &policy) != tc.typed || upstream.calls != 1 {
				t.Fatalf("invalid policy mapping err=%v calls=%d", err, upstream.calls)
			}
			if tc.typed && (policy.Status != 400 || policy.Type != "invalid_request_error" || policy.Code != "unsupported_parameter" || policy.Param != "max_output_tokens" || policy.Message != responsesPolicyMessage) {
				t.Fatal("public contract changed")
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "http") {
				t.Fatal("raw upstream diagnostic escaped")
			}
		})
	}
}

func TestResponsesPreflightClientRejectsEnvelopeMemoryOverload(t *testing.T) {
	upstream := &responsesClientTestUpstream{}
	client, err := NewResponsesPreflightClient(ResponsesClientConfig{ControlURL: "https://gateway.invalid", ServiceKey: "private-key", Expected: Identity{BackendID: "backend-1", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "gen-1"}, AccountID: 7, Concurrency: 1}, upstream)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release := pkghttputil.WithRequestBodyMemoryBudget(context.Background(), pkghttputil.NewBodyMemoryBudget(1))
	defer release()
	wire := []byte(`{"model":"fixture","input":"hello","store":false}`)
	_, err = client.Preflight(ctx, "user-1", "group-1", wire, wire)
	if !errors.Is(err, pkghttputil.ErrRequestBodyMemoryBudget) || upstream.calls != 0 {
		t.Fatalf("memory exhaustion must reject before transport: err=%v calls=%d", err, upstream.calls)
	}
	if err = pkghttputil.ReserveRequestBodyMemory(ctx, 1); err != nil {
		t.Fatal("failed envelope scope retained memory")
	}
}
