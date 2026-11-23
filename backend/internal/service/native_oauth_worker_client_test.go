package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const workerTestKey = "synthetic-control-key-12345678901234567890"

type coreWorkerTestUpstream struct {
	client      *http.Client
	calls       int
	failPut     bool
	proxy       string
	accountID   int64
	concurrency int
}

func (u *coreWorkerTestUpstream) Do(r *http.Request, p string, a int64, c int) (*http.Response, error) {
	u.calls++
	u.proxy = p
	u.accountID = a
	u.concurrency = c
	if !HTTPUpstreamRedirectsDisabled(r.Context()) || r.GetBody != nil {
		return nil, fmt.Errorf("unsafe request")
	}
	if _, ok := r.Context().Deadline(); !ok {
		return nil, fmt.Errorf("missing timeout")
	}
	if u.failPut && r.Method == http.MethodPut {
		return nil, errors.New("secret upstream URL/token/body")
	}
	client := *u.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(r)
}
func (u *coreWorkerTestUpstream) DoWithTLS(r *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, a, c)
}
func coreWorkerFixture(url string) (*OpenAIGatewayService, *Account, ManagedDeploymentBinding, *CoreOAuthCredentialSnapshot, *coreWorkerTestUpstream) {
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	p := CoreOAuthProfile{ProfileRef: "11111111-1111-4111-8111-111111111111", Provider: CoreOAuthProviderExcel, Authorized: true, Subject: "synthetic-subject", WorkspaceID: "synthetic-workspace", IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 2, ExpiresAt: &expiry, ClientID: coreOAuthClientID(CoreOAuthProviderExcel)}
	b := ManagedDeploymentBinding{Provider: p.Provider, AccountID: 7, BackendID: "synthetic-backend", BindingRef: "bind_33333333-3333-4333-8333-333333333333", ProfileRef: "oauth_" + p.ProfileRef, Subject: p.Subject, WorkspaceID: p.WorkspaceID, IdentityEpoch: p.IdentityEpoch}
	a := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3, Credentials: map[string]any{"base_url": url + "/v1", "api_key": "synthetic-inference-key", "bps_files_service_key": "synthetic-files-key", "core_oauth_control_url": url, "core_oauth_control_key": workerTestKey}}
	u := &coreWorkerTestUpstream{client: &http.Client{}}
	s := &OpenAIGatewayService{httpUpstream: u, cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	return s, a, b, &CoreOAuthCredentialSnapshot{Profile: p, AccessToken: "synthetic-access-token"}, u
}
func coreWorkerState(b ManagedDeploymentBinding, p *CoreOAuthCredentialSnapshot, initialized bool) map[string]any {
	state := map[string]any{"contract": "tosub2-managed-oauth/v1", "backend_id": b.BackendID, "binding_ref": "", "profile_ref": "", "provider": "bps_excel", "subject": "", "workspace_id": "", "identity_epoch": "", "token_revision": 0, "initialized": initialized, "credential_generation": "", "expires_at": time.Time{}, "last_event_id": 0, "last_operation_id": ""}
	if initialized {
		state["binding_ref"] = b.BindingRef
		state["profile_ref"] = b.ProfileRef
		state["subject"] = b.Subject
		state["workspace_id"] = b.WorkspaceID
		state["identity_epoch"] = b.IdentityEpoch
		state["token_revision"] = p.Profile.TokenRevision
		state["credential_generation"] = b.IdentityEpoch
		state["expires_at"] = p.Profile.ExpiresAt
		state["last_operation_id"] = "44444444-4444-4444-8444-444444444444"
	}
	return state
}
func TestCoreOAuthWorkerConfigurationClonesAndRejectsUnsafeKeysURLs(t *testing.T) {
	s, a, _, _, _ := coreWorkerFixture("http://127.0.0.1:31415")
	got, err := s.configureCoreOAuthWorkerAccount(a, "", "")
	require.NoError(t, err)
	require.NotSame(t, a, got)
	got.Credentials["new"] = true
	require.NotContains(t, a.Credentials, "new")
	for _, tc := range []struct{ url, key string }{{"http://127.0.0.1:31416", workerTestKey}, {"http://evil:pass@127.0.0.1:31415", workerTestKey}, {"http://127.0.0.1:31415?secret=x", workerTestKey}, {"http://127.0.0.1:31415/#x", workerTestKey}, {"http://127.0.0.1:31415/%2fsecret", workerTestKey}, {"http://127.0.0.1:31415", "short"}} {
		_, err = s.configureCoreOAuthWorkerAccount(a, tc.url, tc.key)
		require.Error(t, err)
		require.NotContains(t, err.Error(), tc.url)
	}
	a.Credentials["api_key"] = workerTestKey
	_, err = s.configureCoreOAuthWorkerAccount(a, "", "")
	require.Error(t, err)
	a.Credentials["api_key"] = "inference"
	a.Credentials["bps_files_service_key"] = workerTestKey
	_, err = s.configureCoreOAuthWorkerAccount(a, "", "")
	require.Error(t, err)
}
func TestCoreOAuthWorkerPublishATOnlyAndReceipt(t *testing.T) {
	var b ManagedDeploymentBinding
	var p *CoreOAuthCredentialSnapshot
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/_control/v1/credentials", r.URL.Path)
		require.Equal(t, "Bearer "+workerTestKey, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("X-BPS-Files-Service-Key"))
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(coreWorkerState(b, p, false))
			return
		}
		puts++
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var request map[string]any
		require.NoError(t, json.Unmarshal(raw, &request))
		require.Len(t, request, 13)
		require.Equal(t, p.AccessToken, request["access_token"])
		require.NotContains(t, string(raw), "refresh_token")
		require.NotContains(t, string(raw), "id_token")
		require.Equal(t, float64(0), request["expected_token_revision"])
		require.Equal(t, b.ProfileRef, request["profile_ref"])
		state := coreWorkerState(b, p, true)
		state["last_operation_id"] = request["operation_id"]
		_ = json.NewEncoder(w).Encode(state)
	}))
	defer server.Close()
	s, a, binding, snapshot, u := coreWorkerFixture(server.URL)
	b = binding
	p = snapshot
	require.NoError(t, s.publishCoreOAuthWorker(context.Background(), a, b, p))
	require.Equal(t, 1, puts)
	require.Equal(t, 2, u.calls)
	require.Equal(t, int64(7), u.accountID)
	require.Equal(t, 3, u.concurrency)
}
func TestCoreOAuthWorkerPublishFailsClosedWithoutRetry(t *testing.T) {
	for _, scenario := range []string{"wrong identity", "bad receipt", "unknown put", "redirect", "duplicate field", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			var b ManagedDeploymentBinding
			var p *CoreOAuthCredentialSnapshot
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "redirect" {
					w.Header().Set("Location", "http://example.invalid/secret")
					w.WriteHeader(307)
					return
				}
				if scenario == "oversized" {
					_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
					return
				}
				if scenario == "duplicate field" {
					_, _ = io.WriteString(w, `{"contract":"x","contract":"x"}`)
					return
				}
				if r.Method == http.MethodGet {
					state := coreWorkerState(b, p, scenario == "wrong identity")
					if scenario == "wrong identity" {
						state["subject"] = "other"
					}
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				puts++
				state := coreWorkerState(b, p, true)
				state["last_operation_id"] = "wrong-operation"
				_ = json.NewEncoder(w).Encode(state)
			}))
			defer server.Close()
			s, a, binding, snapshot, u := coreWorkerFixture(server.URL)
			b = binding
			p = snapshot
			u.failPut = scenario == "unknown put"
			err := s.publishCoreOAuthWorker(context.Background(), a, b, p)
			require.Error(t, err)
			require.NotContains(t, err.Error(), server.URL)
			require.NotContains(t, err.Error(), p.AccessToken)
			require.NotContains(t, err.Error(), "secret upstream")
			require.LessOrEqual(t, puts, 1)
			require.LessOrEqual(t, u.calls, 2)
		})
	}
}
func TestCoreOAuthWorkerSameRevisionRequiresExactExpiry(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			var b ManagedDeploymentBinding
			var p *CoreOAuthCredentialSnapshot
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				state := coreWorkerState(b, p, true)
				if mismatch {
					state["expires_at"] = time.Now().Add(2 * time.Hour)
				}
				_ = json.NewEncoder(w).Encode(state)
			}))
			defer server.Close()
			s, a, binding, snapshot, u := coreWorkerFixture(server.URL)
			b = binding
			p = snapshot
			err := s.publishCoreOAuthWorker(context.Background(), a, b, p)
			if mismatch {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, u.calls)
		})
	}
}
func TestCoreOAuthWorkerUnauthorizedValidatesWholeBoundedTail(t *testing.T) {
	for _, scenario := range []string{"valid", "forged origin", "wrong epoch", "bad cursor", "unconfirmed gap", "confirmed gap", "oversized page"} {
		t.Run(scenario, func(t *testing.T) {
			var b ManagedDeploymentBinding
			var p *CoreOAuthCredentialSnapshot
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer "+workerTestKey, r.Header.Get("Authorization"))
				event := map[string]any{"event_id": 1, "origin": "upstream_oauth", "failed_token_revision": p.Profile.TokenRevision, "identity_epoch": b.IdentityEpoch, "profile_ref": b.ProfileRef, "binding_ref": b.BindingRef, "backend_id": b.BackendID, "created_at": time.Now().UTC()}
				state := coreWorkerState(b, p, true)
				state["last_event_id"] = 1
				if scenario == "confirmed gap" || scenario == "unconfirmed gap" {
					state["last_event_id"] = 200
					state["token_revision"] = 200
				}
				if r.URL.Path == "/_control/v1/credentials" {
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				require.Equal(t, "/_control/v1/auth-events", r.URL.Path)
				require.Equal(t, "after=0", r.URL.RawQuery)
				if scenario == "forged origin" {
					event["origin"] = "ingress"
				}
				if scenario == "wrong epoch" {
					event["identity_epoch"] = "other"
				}
				page := map[string]any{"events": []any{event}, "next_cursor": 1, "oldest_event_id": 1, "cursor_gap": false}
				if scenario == "bad cursor" {
					page["next_cursor"] = 2
				}
				if scenario == "confirmed gap" || scenario == "unconfirmed gap" {
					events := make([]any, 0, 128)
					for id := 73; id <= 200; id++ {
						if scenario == "unconfirmed gap" && id == 100 {
							continue
						}
						row := coreOAuthCopyMap(event)
						row["event_id"], row["failed_token_revision"] = id, id
						events = append(events, row)
					}
					page["events"] = events
					page["cursor_gap"] = true
					page["oldest_event_id"] = 73
					page["next_cursor"] = 200
				}
				if scenario == "oversized page" {
					events := make([]any, 129)
					for i := range events {
						events[i] = event
					}
					page["events"] = events
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			s, a, binding, snapshot, _ := coreWorkerFixture(server.URL)
			b = binding
			p = snapshot
			got, err := s.coreOAuthWorkerUnauthorized(context.Background(), a, b)
			if scenario == "confirmed gap" {
				require.NoError(t, err)
				require.Len(t, got, 128)
				require.Equal(t, int64(73), got[0].FailedRevision)
				require.Equal(t, CoreOAuthUnauthorizedInput{ProfileRef: p.Profile.ProfileRef, IdentityEpoch: b.IdentityEpoch, FailedRevision: 200, StatusCode: 401, Origin: "upstream_oauth"}, got[127])
			} else if scenario == "valid" {
				require.NoError(t, err)
				require.Equal(t, []CoreOAuthUnauthorizedInput{{ProfileRef: p.Profile.ProfileRef, IdentityEpoch: b.IdentityEpoch, FailedRevision: p.Profile.TokenRevision, StatusCode: 401, Origin: "upstream_oauth"}}, got)
			} else {
				require.Error(t, err)
				require.Empty(t, got)
			}
		})
	}
}
