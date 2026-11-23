package openai_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
)

type bpsTestTransport func(*http.Request) (*http.Response, error)

func (f bpsTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func bpsFixtureToken(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".synthetic"
}

func bpsTokenFixture(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"access_token": bpsFixtureToken(t, map[string]any{
			"client_id": "app_fnr0pYvVwwFDocDumLG3H2Bp", "sub": "subject-a", "exp": time.Now().Add(time.Hour).Unix(),
			"iss": "https://auth.openai.com", "aud": []string{"https://api.openai.com/v1"},
			"https://api.openai.com/auth":    map[string]string{"chatgpt_account_id": "workspace-a"},
			"https://api.openai.com/profile": map[string]string{"email": "alice@example.com"},
		}),
		"refresh_token": "synthetic-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "openid offline_access",
	}
}

func bpsLocalClient(t *testing.T, handler http.HandlerFunc) *openai.BPSOAuthClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, _ := url.Parse(server.URL)
	transport := server.Client().Transport
	client, err := openai.NewBPSOAuthClientWithHTTPClient(&http.Client{Transport: bpsTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://auth.openai.com/oauth/token?unified=true" {
			t.Fatal("token request destination changed")
		}
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		copy.URL = &u
		return transport.RoundTrip(copy)
	})})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestBPSOAuthCallbackExchangesBoundPKCEOnce(t *testing.T) {
	transaction, err := openai.NewBPSOAuthTransaction()
	if err != nil {
		t.Fatal(err)
	}
	authorization, _ := url.Parse(transaction.AuthorizationURL)
	calls := 0
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "" {
			t.Error("incorrect OAuth request shape")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if len(r.PostForm) != 5 || r.PostForm.Get("client_id") != "app_fnr0pYvVwwFDocDumLG3H2Bp" || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "synthetic-code" || r.PostForm.Get("redirect_uri") != transaction.RedirectURI {
			t.Error("incorrect fixed BPS code exchange")
		}
		if challenge := openai.GenerateCodeChallenge(r.PostForm.Get("code_verifier")); challenge != authorization.Query().Get("code_challenge") {
			t.Error("exchange did not use this transaction's PKCE secret")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(bpsTokenFixture(t))
	})
	callback := transaction.RedirectURI + "?" + url.Values{"state": {authorization.Query().Get("state")}, "code": {"synthetic-code"}}.Encode()
	token, err := client.ExchangeCallback(context.Background(), transaction, callback, openai.BPSOAuthIdentity{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if token.Subject != "subject-a" || token.WorkspaceID != "workspace-a" || token.Email != "alice@example.com" || token.RefreshToken != "synthetic-refresh" || token.ClientID != "app_fnr0pYvVwwFDocDumLG3H2Bp" || token.Scope != "openid offline_access" || token.ExpiresAt.Before(time.Now()) {
		t.Fatal("validated identity or token response fields lost")
	}
	if _, err := client.ExchangeCallback(context.Background(), transaction, callback, openai.BPSOAuthIdentity{}); err == nil || !strings.Contains(err.Error(), "consumed") {
		t.Fatal("authorization transaction was reusable")
	}
	if calls != 1 {
		t.Fatal("authorization code submitted more than once")
	}
}

func TestBPSOAuthRefreshPreservesIdentityAndUnrotatedRefreshToken(t *testing.T) {
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if len(r.PostForm) != 3 || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "synthetic-existing-refresh" || r.PostForm.Get("client_id") != "app_fnr0pYvVwwFDocDumLG3H2Bp" {
			t.Error("incorrect fixed BPS refresh form")
		}
		payload := bpsTokenFixture(t)
		delete(payload, "refresh_token")
		_ = json.NewEncoder(w).Encode(payload)
	})
	previous := openai.BPSOAuthToken{
		TokenResponse:    openai.TokenResponse{RefreshToken: "synthetic-existing-refresh"},
		BPSOAuthIdentity: openai.BPSOAuthIdentity{Subject: "subject-a", WorkspaceID: "workspace-a", Email: "alice@example.com"},
		ClientID:         "app_fnr0pYvVwwFDocDumLG3H2Bp",
	}
	refreshed, err := client.Refresh(context.Background(), previous)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RefreshToken != previous.RefreshToken || refreshed.BPSOAuthIdentity != previous.BPSOAuthIdentity || refreshed.ClientID != previous.ClientID {
		t.Fatal("refresh lost the existing grant identity or unrotated refresh token")
	}
}

func TestBPSOAuthAuthorizationUsesOfficialClientAndCallback(t *testing.T) {
	transaction, err := openai.NewBPSOAuthTransaction()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(transaction.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/api/accounts/authorize" {
		t.Fatal("authorization endpoint differs from the official BPS endpoint")
	}
	want := map[string]string{
		"client_id":     "app_fnr0pYvVwwFDocDumLG3H2Bp",
		"redirect_uri":  "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback",
		"response_type": "code", "scope": "openid offline_access email profile organization.read",
		"audience": "https://api.openai.com/v1", "code_challenge_method": "S256", "platform": "OfficeOnline",
	}
	query := u.Query()
	for name, expected := range want {
		if query.Get(name) != expected {
			t.Errorf("incorrect BPS authorization field %s", name)
		}
	}
	if len(query.Get("state")) < 43 || len(query.Get("code_challenge")) != 43 || transaction.RedirectURI != want["redirect_uri"] {
		t.Fatal("missing state, S256 challenge, or official callback")
	}
	if transaction.ExpiresAt.Before(time.Now()) || transaction.ExpiresAt.After(time.Now().Add(11*time.Minute)) {
		t.Fatal("authorization transaction expiry is not bounded")
	}
	if query.Has("codex_cli_simplified_flow") {
		t.Fatal("Codex login options leaked into Excel authorization")
	}
}

func TestBPSOAuthInvalidCallbacksNeverSendCodeOrConsumeTransaction(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(string) string
	}{
		{"localhost", func(v string) string {
			return strings.Replace(v, openai.BPSExcelCallback, "http://localhost:1455/auth/callback", 1)
		}},
		{"lookalike host", func(v string) string {
			return strings.Replace(v, "bps.openai.com/", "bps.openai.com.attacker.invalid/", 1)
		}},
		{"encoded path", func(v string) string { return strings.Replace(v, "/auth/callback?", "/auth/%63allback?", 1) }},
		{"userinfo", func(v string) string { return strings.Replace(v, "https://", "https://user@", 1) }},
		{"fragment", func(v string) string { return v + "#fragment" }},
		{"empty fragment", func(v string) string { return v + "#" }},
		{"too long", func(v string) string { return v + "&extra=" + strings.Repeat("a", 17000) }},
		{"mismatched state", func(v string) string {
			u, _ := url.Parse(v)
			q := u.Query()
			q.Set("state", "unrelated-state")
			u.RawQuery = q.Encode()
			return u.String()
		}},
		{"duplicate state", func(v string) string { return v + "&state=another-state" }},
		{"duplicate code", func(v string) string { return v + "&code=another-code" }},
		{"malformed escaping", func(v string) string { return v + "&extra=%zz" }},
		{"empty code", func(v string) string {
			u, _ := url.Parse(v)
			q := u.Query()
			q.Set("code", "")
			u.RawQuery = q.Encode()
			return u.String()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := openai.NewBPSOAuthTransaction()
			if err != nil {
				t.Fatal(err)
			}
			authorization, _ := url.Parse(tx.AuthorizationURL)
			callback := openai.BPSExcelCallback + "?" + url.Values{"state": {authorization.Query().Get("state")}, "code": {"synthetic-code"}}.Encode()
			calls := 0
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(bpsTokenFixture(t))
			})
			_, err = client.ExchangeCallback(context.Background(), tx, tc.alter(callback), openai.BPSOAuthIdentity{})
			if err == nil || calls != 0 {
				t.Fatal("invalid callback reached the token endpoint")
			}
			if _, err = client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{}); err != nil || calls != 1 {
				t.Fatal("rejected callback consumed the valid transaction")
			}
		})
	}
}

func bpsTestAuthorization(t *testing.T) (*openai.BPSOAuthTransaction, string) {
	t.Helper()
	tx, err := openai.NewBPSOAuthTransaction()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(tx.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	return tx, openai.BPSExcelCallback + "?" + url.Values{"state": {u.Query().Get("state")}, "code": {"synthetic-code"}}.Encode()
}

func bpsPreviousToken() openai.BPSOAuthToken {
	return openai.BPSOAuthToken{
		TokenResponse:    openai.TokenResponse{RefreshToken: "synthetic-existing-refresh"},
		BPSOAuthIdentity: openai.BPSOAuthIdentity{Subject: "subject-a", WorkspaceID: "workspace-a", Email: "alice@example.com"},
		ClientID:         openai.BPSExcelClientID,
	}
}

func bpsRequireError(t *testing.T, err error, code string, unknown, permanent bool) *openai.BPSOAuthError {
	t.Helper()
	var result *openai.BPSOAuthError
	if !errors.As(err, &result) || result.Code != code || result.OutcomeUnknown != unknown || result.Permanent != permanent || result.Error() != code {
		t.Fatalf("unexpected safe OAuth error: code=%q", code)
	}
	return result
}

func TestBPSOAuthAuthorizationRejectionConsumesWithoutLeakingDetails(t *testing.T) {
	tx, callback := bpsTestAuthorization(t)
	u, _ := url.Parse(callback)
	query := u.Query()
	query.Del("code")
	query.Set("error", "access_denied")
	query.Set("error_description", "synthetic-secret-do-not-return")
	u.RawQuery = query.Encode()
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("rejected authorization reached token endpoint") })
	_, err := client.ExchangeCallback(context.Background(), tx, u.String(), openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_authorization_rejected", false, false)
	_, err = client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_oauth_transaction_consumed", false, false)
}

func TestBPSOAuthConcurrentCallbacksExchangeExactlyOnce(t *testing.T) {
	tx, callback := bpsTestAuthorization(t)
	var calls atomic.Int32
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(bpsTokenFixture(t))
	})
	const count = 20
	var wait sync.WaitGroup
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
			continue
		}
		bpsRequireError(t, err, "bps_oauth_transaction_consumed", false, false)
	}
	if succeeded != 1 || calls.Load() != 1 {
		t.Fatal("concurrent callbacks exchanged the authorization code more than once")
	}
}

func TestBPSOAuthTransactionStateIsNotTakenFromPublicDTO(t *testing.T) {
	tx, callback := bpsTestAuthorization(t)
	tx.AuthorizationURL = "https://attacker.invalid/authorize"
	tx.RedirectURI = "http://localhost:1455/auth/callback"
	tx.ExpiresAt = time.Now().Add(-time.Hour)
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.PostForm.Get("redirect_uri") != openai.BPSExcelCallback {
			t.Error("public DTO changed fixed redirect")
		}
		_ = json.NewEncoder(w).Encode(bpsTokenFixture(t))
	})
	if _, err := client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{}); err != nil {
		t.Fatal(err)
	}
	_, err := client.ExchangeCallback(context.Background(), nil, callback, openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_oauth_transaction_invalid", false, false)
	_, err = client.ExchangeCallback(context.Background(), &openai.BPSOAuthTransaction{ExpiresAt: time.Now().Add(time.Hour)}, callback, openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_oauth_transaction_expired", false, false)
}

func TestBPSOAuthRefreshRequiresBoundGrantBeforeNetworking(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*openai.BPSOAuthToken)
	}{
		{"Codex client", func(v *openai.BPSOAuthToken) { v.ClientID = "app_EMoamEEZ73f0CkXaXp7hrann" }},
		{"missing subject", func(v *openai.BPSOAuthToken) { v.Subject = "" }},
		{"missing workspace", func(v *openai.BPSOAuthToken) { v.WorkspaceID = "" }},
		{"missing refresh token", func(v *openai.BPSOAuthToken) { v.RefreshToken = "" }},
		{"malformed refresh token", func(v *openai.BPSOAuthToken) { v.RefreshToken = "synthetic\nrefresh" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := bpsPreviousToken()
			tc.alter(&previous)
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid grant reached token endpoint") })
			_, err := client.Refresh(context.Background(), previous)
			bpsRequireError(t, err, "bps_profile_mismatch", false, false)
		})
	}
}

func TestBPSOAuthRefreshFailureIsSafeAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		body               string
		code               string
		unknown, permanent bool
	}{
		{"revoked grant", 400, `{"error":"invalid_grant","error_description":"synthetic-secret"}`, "bps_invalid_grant", false, true},
		{"server failure", 503, `{"error":"server_error","description":"synthetic-secret"}`, "bps_refresh_outcome_unknown", true, false},
		{"malformed success", 200, `synthetic-secret-invalid-json`, "bps_refresh_outcome_unknown", true, false},
		{"oversized success", 200, strings.Repeat("x", (1<<20)+1), "bps_refresh_outcome_unknown", true, false},
		{"rate limited", 429, `{"error":"rate_limited","secret":"synthetic-secret"}`, "bps_token_endpoint_rejected", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := client.Refresh(context.Background(), bpsPreviousToken())
			bpsRequireError(t, err, tc.code, tc.unknown, tc.permanent)
			if calls != 1 {
				t.Fatal("refresh was retried")
			}
		})
	}
	client, err := openai.NewBPSOAuthClientWithHTTPClient(&http.Client{Transport: bpsTestTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic-secret-transport-error")
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Refresh(context.Background(), bpsPreviousToken())
	bpsRequireError(t, err, "bps_refresh_outcome_unknown", true, false)
}

func TestBPSOAuthClientNeverFollowsRedirectsOrUsesCookieJar(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(openai.BPSTokenURL)
	jar.SetCookies(origin, []*http.Cookie{{Name: "synthetic_session", Value: "synthetic-secret"}})
	calls := 0
	supplied := &http.Client{Jar: jar, Transport: bpsTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != openai.BPSTokenURL || r.Header.Get("Cookie") != "" {
			t.Error("token endpoint or cookie boundary changed")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://attacker.invalid/token"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}
	client, err := openai.NewBPSOAuthClientWithHTTPClient(supplied)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Refresh(context.Background(), bpsPreviousToken())
	bpsRequireError(t, err, "bps_token_endpoint_rejected", false, false)
	if calls != 1 || supplied.Jar != jar || supplied.CheckRedirect != nil || supplied.Timeout != 0 {
		t.Fatal("client was mutated or redirect followed")
	}
}

func bpsAlterTokenClaims(t *testing.T, payload map[string]any, key string, alter func(map[string]any)) {
	t.Helper()
	token, ok := payload[key].(string)
	if !ok {
		t.Fatal("synthetic token field missing")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid synthetic token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		t.Fatal("invalid synthetic claims")
	}
	alter(claims)
	payload[key] = bpsFixtureToken(t, claims)
}

func TestBPSOAuthReturnedClaimsMustAgreeWithBoundIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		alter      func(*testing.T, map[string]any)
	}{
		{"response client", "bps_client_mismatch", func(t *testing.T, p map[string]any) { p["client_id"] = "another-client" }},
		{"access client", "bps_client_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["client_id"] = "another-client" })
		}},
		{"access authorized party", "bps_client_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["azp"] = "another-client" })
		}},
		{"missing client evidence", "bps_client_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { delete(c, "client_id") })
		}},
		{"access issuer", "bps_issuer_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["iss"] = "https://attacker.invalid" })
		}},
		{"access audience", "bps_audience_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["aud"] = []string{"another-audience"} })
		}},
		{"expired access", "bps_token_expired", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Second).Unix() })
		}},
		{"missing subject", "bps_subject_mismatch", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { delete(c, "sub") })
		}},
		{"workspace conflict", "bps_workspace_mismatch", func(t *testing.T, p map[string]any) { p["chatgpt_account_id"] = "workspace-other" }},
		{"email conflict", "bps_email_mismatch", func(t *testing.T, p map[string]any) { p["email"] = "other@example.com" }},
		{"missing refresh grant", "bps_invalid_token_response", func(t *testing.T, p map[string]any) { delete(p, "refresh_token") }},
		{"nonbearer token", "bps_invalid_token_response", func(t *testing.T, p map[string]any) { p["token_type"] = "Basic" }},
		{"malformed access token", "bps_invalid_token_claims", func(t *testing.T, p map[string]any) { p["access_token"] = "synthetic.invalid-payload.signature" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bpsTokenFixture(t)
			tc.alter(t, payload)
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(payload) })
			tx, callback := bpsTestAuthorization(t)
			_, err := client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
			bpsRequireError(t, err, tc.code, false, false)
		})
	}
}

func TestBPSOAuthRefreshConflictingIdentityHasUnknownOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*testing.T, map[string]any)
	}{
		{"subject", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) { c["sub"] = "subject-other" })
		}},
		{"workspace", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) {
				c["https://api.openai.com/auth"] = map[string]string{"chatgpt_account_id": "workspace-other"}
			})
		}},
		{"email", func(t *testing.T, p map[string]any) {
			bpsAlterTokenClaims(t, p, "access_token", func(c map[string]any) {
				c["https://api.openai.com/profile"] = map[string]string{"email": "other@example.com"}
			})
		}},
		{"client", func(t *testing.T, p map[string]any) { p["client_id"] = "another-client" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bpsTokenFixture(t)
			tc.alter(t, payload)
			calls := 0
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; _ = json.NewEncoder(w).Encode(payload) })
			_, err := client.Refresh(context.Background(), bpsPreviousToken())
			bpsRequireError(t, err, "bps_refresh_outcome_unknown", true, false)
			if calls != 1 {
				t.Fatal("ambiguous refresh was retried")
			}
		})
	}
}

func TestBPSOAuthExplicitInvalidTokenFieldsDoNotFallBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]any)
	}{
		{"zero lifetime", func(p map[string]any) { p["expires_in"] = 0 }},
		{"null lifetime", func(p map[string]any) { p["expires_in"] = nil }},
		{"empty refresh token", func(p map[string]any) { p["refresh_token"] = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bpsTokenFixture(t)
			tc.alter(payload)
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(payload) })
			_, err := client.Refresh(context.Background(), bpsPreviousToken())
			bpsRequireError(t, err, "bps_refresh_outcome_unknown", true, false)
		})
	}
}

func bpsAddIDToken(t *testing.T, payload map[string]any) {
	t.Helper()
	payload["id_token"] = bpsFixtureToken(t, map[string]any{
		"sub": "subject-a", "aud": openai.BPSExcelClientID, "iss": "https://auth.openai.com/", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "workspace-a"}, "email": "Alice@Example.com", "email_verified": true,
	})
}

func TestBPSOAuthIDTokenMustAgreeWithAccessGrant(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		alter      func(map[string]any)
	}{
		{"different subject", "bps_subject_mismatch", func(c map[string]any) { c["sub"] = "subject-other" }},
		{"different workspace", "bps_workspace_mismatch", func(c map[string]any) {
			c["https://api.openai.com/auth"] = map[string]string{"chatgpt_account_id": "workspace-other"}
		}},
		{"different email", "bps_email_mismatch", func(c map[string]any) { c["email"] = "other@example.com" }},
		{"unverified email", "bps_email_unverified", func(c map[string]any) { c["email_verified"] = false }},
		{"different issuer", "bps_issuer_mismatch", func(c map[string]any) { c["iss"] = "https://attacker.invalid" }},
		{"expired identity token", "bps_token_expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Second).Unix() }},
		{"different audience", "bps_audience_mismatch", func(c map[string]any) { c["aud"] = "another-client" }},
		{"multiple audiences", "bps_client_mismatch", func(c map[string]any) { c["aud"] = []string{openai.BPSExcelClientID, "another-client"} }},
		{"missing audience", "bps_client_mismatch", func(c map[string]any) { delete(c, "aud") }},
		{"malformed audience", "bps_audience_mismatch", func(c map[string]any) { c["aud"] = map[string]string{"client": openai.BPSExcelClientID} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bpsTokenFixture(t)
			bpsAddIDToken(t, payload)
			bpsAlterTokenClaims(t, payload, "id_token", tc.alter)
			client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(payload) })
			tx, callback := bpsTestAuthorization(t)
			_, err := client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
			bpsRequireError(t, err, tc.code, false, false)
		})
	}
}

func TestBPSOAuthExpectedIdentityCannotBeRebound(t *testing.T) {
	for _, identity := range []openai.BPSOAuthIdentity{
		{Subject: "subject-other"}, {WorkspaceID: "workspace-other"}, {Email: "other@example.com"},
	} {
		client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(bpsTokenFixture(t)) })
		tx, callback := bpsTestAuthorization(t)
		if _, err := client.ExchangeCallback(context.Background(), tx, callback, identity); err == nil {
			t.Fatal("returned grant rebound an existing identity")
		}
	}
}

func TestBPSOAuthRefreshRetainsReturnedFieldsAndUsesEarlierExpiry(t *testing.T) {
	before := time.Now()
	payload := bpsTokenFixture(t)
	bpsAddIDToken(t, payload)
	payload["refresh_token"] = "synthetic-rotated-refresh"
	payload["expires_in"] = 120
	client := bpsLocalClient(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(payload) })
	result, err := client.Refresh(context.Background(), bpsPreviousToken())
	if err != nil {
		t.Fatal(err)
	}
	if result.RefreshToken != "synthetic-rotated-refresh" || result.IDToken != payload["id_token"] || result.Scope != payload["scope"] || result.TokenType != "Bearer" || result.ExpiresIn != 120 || result.ExpiresAt.Before(before.Add(119*time.Second)) || result.ExpiresAt.After(time.Now().Add(121*time.Second)) {
		t.Fatal("rotated grant fields or bounded expiry lost")
	}
	bpsAlterTokenClaims(t, payload, "access_token", func(c map[string]any) { delete(c, "exp") })
	if _, err = client.Refresh(context.Background(), bpsPreviousToken()); err != nil {
		t.Fatal("positive expires_in did not supply expiry")
	}
	delete(payload, "expires_in")
	_, err = client.Refresh(context.Background(), bpsPreviousToken())
	bpsRequireError(t, err, "bps_refresh_outcome_unknown", true, false)
}

func TestBPSOAuthAmbiguousCodeExchangeCannotBeRetried(t *testing.T) {
	calls := 0
	client, err := openai.NewBPSOAuthClientWithHTTPClient(&http.Client{Transport: bpsTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("synthetic-secret-transport-error")
	})})
	if err != nil {
		t.Fatal(err)
	}
	tx, callback := bpsTestAuthorization(t)
	_, err = client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_token_exchange_outcome_unknown", true, false)
	_, err = client.ExchangeCallback(context.Background(), tx, callback, openai.BPSOAuthIdentity{})
	bpsRequireError(t, err, "bps_oauth_transaction_consumed", false, false)
	if calls != 1 {
		t.Fatal("ambiguous authorization code was retried")
	}
}

func TestBPSOAuthInvalidClientConfigurationIsSafe(t *testing.T) {
	_, err := openai.NewBPSOAuthClientWithHTTPClient(nil)
	bpsRequireError(t, err, "bps_oauth_client_configuration_invalid", false, false)
	_, err = openai.NewBPSOAuthClient("://synthetic-user:synthetic-password@invalid-proxy")
	bpsRequireError(t, err, "bps_oauth_client_configuration_invalid", false, false)
}
