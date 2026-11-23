package openai

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/httpclient"
)

const (
	BPSExcelClientID       = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	BPSExcelCallback       = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
	BPSAuthorizeURL        = "https://auth.openai.com/api/accounts/authorize"
	BPSTokenURL            = "https://auth.openai.com/oauth/token?unified=true"
	BPSOAuthUserAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	BPSOAuthAcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"
)

// BPSOAuthTransaction binds one official Excel authorization to its PKCE secret.
// Callers keep it server-side; only AuthorizationURL and expiry belong in UI.
type BPSOAuthTransaction struct {
	AuthorizationURL string    `json:"authorization_url"`
	RedirectURI      string    `json:"redirect_uri"`
	ExpiresAt        time.Time `json:"expires_at"`
	mu               sync.Mutex
	state            string
	verifier         string
	consumed         bool
	expiresAt        time.Time
}

func NewBPSOAuthTransaction() (*BPSOAuthTransaction, error) {
	state, err := GenerateState()
	if err != nil {
		return nil, err
	}
	verifier, err := GenerateCodeVerifier()
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"client_id": {BPSExcelClientID}, "response_type": {"code"},
		"redirect_uri": {BPSExcelCallback}, "scope": {"openid offline_access email profile organization.read"},
		"audience": {"https://api.openai.com/v1"}, "code_challenge_method": {"S256"},
		"code_challenge": {GenerateCodeChallenge(verifier)}, "state": {state}, "platform": {"OfficeOnline"},
	}
	expiry := time.Now().Add(10 * time.Minute)
	return &BPSOAuthTransaction{AuthorizationURL: BPSAuthorizeURL + "?" + query.Encode(), RedirectURI: BPSExcelCallback,
		ExpiresAt: expiry, expiresAt: expiry, state: state, verifier: verifier}, nil
}

// BPSOAuthIdentity contains cross-checked identity, never OAuth credentials.
type BPSOAuthIdentity struct {
	Subject     string `json:"subject"`
	WorkspaceID string `json:"workspace_id"`
	Email       string `json:"email"`
}

// BPSOAuthToken is for server-side encrypted persistence, not an HTTP response.
// Token claims are consistency evidence, not local JWT signature verification.
type BPSOAuthToken struct {
	TokenResponse
	BPSOAuthIdentity
	ClientID  string    `json:"client_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BPSOAuthError retains policy codes only; causes and upstream bodies may hold secrets.
type BPSOAuthError struct {
	Code           string
	StatusCode     int
	Permanent      bool
	OutcomeUnknown bool
}

func (e *BPSOAuthError) Error() string    { return e.Code }
func bpsError(code string) *BPSOAuthError { return &BPSOAuthError{Code: code} }

type BPSOAuthClient struct{ client *http.Client }

// NewBPSOAuthClient uses the existing shared HTTP/proxy facilities. The auth
// origin and callback cannot be configured by a request or environment variable.
func NewBPSOAuthClient(proxyURL string) (*BPSOAuthClient, error) {
	client, err := httpclient.GetClient(httpclient.Options{ProxyURL: proxyURL, Timeout: 30 * time.Second})
	if err != nil {
		return nil, bpsError("bps_oauth_client_configuration_invalid")
	}
	return NewBPSOAuthClientWithHTTPClient(client)
}

// NewBPSOAuthClientWithHTTPClient injects HTTP transport without changing any
// OAuth destination. The supplied client is copied and never mutated.
func NewBPSOAuthClientWithHTTPClient(client *http.Client) (*BPSOAuthClient, error) {
	if client == nil {
		return nil, bpsError("bps_oauth_client_configuration_invalid")
	}
	copy := *client
	copy.Jar = nil
	if copy.Timeout <= 0 || copy.Timeout > 30*time.Second {
		copy.Timeout = 30 * time.Second
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &BPSOAuthClient{client: &copy}, nil
}

// ExchangeCallback accepts only the exact official callback URL for this
// transaction. A valid submission consumes the transaction before networking,
// including ambiguous outcomes; the same code is never automatically retried.
func (c *BPSOAuthClient) ExchangeCallback(ctx context.Context, transaction *BPSOAuthTransaction, callback string, expected BPSOAuthIdentity) (*BPSOAuthToken, error) {
	if transaction == nil {
		return nil, bpsError("bps_oauth_transaction_invalid")
	}
	transaction.mu.Lock()
	if transaction.consumed {
		transaction.mu.Unlock()
		return nil, bpsError("bps_oauth_transaction_consumed")
	}
	if transaction.expiresAt.IsZero() || !time.Now().Before(transaction.expiresAt) {
		transaction.consumed, transaction.verifier = true, ""
		transaction.mu.Unlock()
		return nil, bpsError("bps_oauth_transaction_expired")
	}
	u, err := url.Parse(callback)
	if err != nil || len(callback) > 16384 || strings.Contains(callback, "#") || !strings.HasPrefix(callback, BPSExcelCallback+"?") || strings.SplitN(callback, "?", 2)[0] != BPSExcelCallback ||
		u.Scheme != "https" || u.Host != "bps.openai.com" || u.User != nil || u.Fragment != "" || u.RawPath != "" ||
		u.Path != "/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback" {
		transaction.mu.Unlock()
		return nil, bpsError("bps_callback_mismatch")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		transaction.mu.Unlock()
		return nil, bpsError("bps_callback_invalid")
	}
	for _, values := range query {
		if len(values) != 1 {
			transaction.mu.Unlock()
			return nil, bpsError("bps_duplicate_callback_parameter")
		}
	}
	state := query.Get("state")
	if len(state) != len(transaction.state) || transaction.state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(transaction.state)) != 1 {
		transaction.mu.Unlock()
		return nil, bpsError("bps_oauth_state_mismatch")
	}
	if query.Has("error") {
		transaction.consumed, transaction.verifier = true, ""
		transaction.mu.Unlock()
		return nil, bpsError("bps_authorization_rejected")
	}
	code := query.Get("code")
	if !bpsSecretText(code, 8192) || transaction.verifier == "" {
		transaction.mu.Unlock()
		return nil, bpsError("bps_invalid_authorization_code")
	}
	verifier := transaction.verifier
	transaction.consumed, transaction.verifier = true, ""
	transaction.mu.Unlock()
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {BPSExcelClientID}, "redirect_uri": {BPSExcelCallback}, "code": {code}, "code_verifier": {verifier}}
	payload, err := c.requestTokens(ctx, form, false)
	if err != nil {
		return nil, err
	}
	return validateBPSTokens(payload, nil, expected)
}

// Refresh performs one refresh attempt. Persistence and single-writer authority
// belong to the core profile service; this provider never retries a rotated RT.
func (c *BPSOAuthClient) Refresh(ctx context.Context, previous BPSOAuthToken) (*BPSOAuthToken, error) {
	if previous.ClientID != BPSExcelClientID || !bpsSecretText(previous.RefreshToken, 65536) || previous.Subject == "" || previous.WorkspaceID == "" {
		return nil, bpsError("bps_profile_mismatch")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {BPSExcelClientID}, "refresh_token": {previous.RefreshToken}}
	payload, err := c.requestTokens(ctx, form, true)
	if err != nil {
		return nil, err
	}
	result, err := validateBPSTokens(payload, &previous, previous.BPSOAuthIdentity)
	if err != nil {
		return nil, &BPSOAuthError{Code: "bps_refresh_outcome_unknown", OutcomeUnknown: true}
	}
	return result, nil
}

func bpsSecretText(value string, limit int) bool {
	return value != "" && len(value) <= limit && !strings.ContainsFunc(value, func(r rune) bool { return r <= 32 || r == 127 })
}

func (c *BPSOAuthClient) requestTokens(ctx context.Context, form url.Values, refresh bool) ([]byte, error) {
	unknown := func() error {
		code := "bps_token_exchange_outcome_unknown"
		if refresh {
			code = "bps_refresh_outcome_unknown"
		}
		return &BPSOAuthError{Code: code, OutcomeUnknown: true}
	}
	if c == nil || c.client == nil || ctx == nil {
		return nil, bpsError("bps_oauth_client_configuration_invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, BPSTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, bpsError("bps_token_request_invalid")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", BPSOAuthUserAgent)
	req.Header.Set("Accept-Language", BPSOAuthAcceptLanguage)
	response, err := c.client.Do(req)
	if err != nil {
		return nil, unknown()
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, unknown()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(raw, &envelope)
		var objectFailure struct {
			Code string `json:"code"`
		}
		var failureCode string
		if json.Unmarshal(envelope.Error, &objectFailure) == nil {
			failureCode = objectFailure.Code
		} else {
			_ = json.Unmarshal(envelope.Error, &failureCode)
		}
		if failureCode == "invalid_grant" {
			return nil, &BPSOAuthError{Code: "bps_invalid_grant", StatusCode: response.StatusCode, Permanent: true}
		}
		if !refresh && failureCode == "unsupported_country_region_territory" {
			return nil, &BPSOAuthError{Code: "bps_region_unsupported", StatusCode: response.StatusCode, Permanent: true}
		}
		if refresh && response.StatusCode >= 500 {
			return nil, unknown()
		}
		return nil, &BPSOAuthError{Code: "bps_token_endpoint_rejected", StatusCode: response.StatusCode}
	}
	return raw, nil
}

func bpsClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, bpsError("bps_invalid_token_claims")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, bpsError("bps_invalid_token_claims")
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return nil, bpsError("bps_invalid_token_claims")
	}
	return claims, nil
}

func bpsClaimObject(claims map[string]any, key string) map[string]any {
	value, _ := claims[key].(map[string]any)
	return value
}

func bpsIdentityField(code string, required bool, values ...any) (string, error) {
	result := ""
	for _, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" || strings.TrimSpace(text) != text || len(text) > 1024 || strings.ContainsFunc(text, func(r rune) bool { return r < 32 || r == 127 }) {
			return "", bpsError(code)
		}
		if result != "" && result != text {
			return "", bpsError(code)
		}
		result = text
	}
	if required && result == "" {
		return "", bpsError(code)
	}
	return result, nil
}

func bpsExpected(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func validateBPSTokens(raw []byte, previous *BPSOAuthToken, expected BPSOAuthIdentity) (*BPSOAuthToken, error) {
	var response TokenResponse
	var payload map[string]any
	if json.Unmarshal(raw, &response) != nil || json.Unmarshal(raw, &payload) != nil || payload == nil || !bpsSecretText(response.AccessToken, 65536) {
		return nil, bpsError("bps_invalid_token_response")
	}
	if response.TokenType != "" && !strings.EqualFold(response.TokenType, "bearer") {
		return nil, bpsError("bps_invalid_token_response")
	}
	if value, present := payload["refresh_token"]; previous != nil && (!present || value == nil) {
		response.RefreshToken = previous.RefreshToken
	}
	if !bpsSecretText(response.RefreshToken, 65536) || (response.IDToken != "" && !bpsSecretText(response.IDToken, 65536)) {
		return nil, bpsError("bps_invalid_token_response")
	}
	access, err := bpsClaims(response.AccessToken)
	if err != nil {
		return nil, err
	}
	id := map[string]any{}
	if response.IDToken != "" {
		id, err = bpsClaims(response.IDToken)
		if err != nil {
			return nil, err
		}
	}
	confirmed := false
	if v, ok := payload["client_id"]; ok {
		if v != BPSExcelClientID {
			return nil, bpsError("bps_client_mismatch")
		}
		confirmed = true
	}
	now := time.Now()
	expiry := time.Time{}
	for index, claims := range []map[string]any{access, id} {
		for _, key := range []string{"client_id", "azp"} {
			if value, ok := claims[key]; ok {
				if value != BPSExcelClientID {
					return nil, bpsError("bps_client_mismatch")
				}
				confirmed = true
			}
		}
		if issuer, ok := claims["iss"]; ok && issuer != "https://auth.openai.com" && issuer != "https://auth.openai.com/" {
			return nil, bpsError("bps_issuer_mismatch")
		}
		if value, ok := claims["exp"]; ok {
			seconds, valid := value.(float64)
			if !valid || seconds <= float64(now.Unix()) || seconds > 253402300799 {
				return nil, bpsError("bps_token_expired")
			}
			if index == 0 {
				expiry = time.Unix(int64(seconds), 0)
			}
		}
	}
	for index, pair := range []struct {
		claims   map[string]any
		audience string
	}{{access, "https://api.openai.com/v1"}, {id, BPSExcelClientID}} {
		value, exists := pair.claims["aud"]
		if !exists {
			if index == 1 && response.IDToken != "" {
				return nil, bpsError("bps_client_mismatch")
			}
			continue
		}
		matched := value == pair.audience
		if list, ok := value.([]any); ok {
			if index == 1 && len(list) != 1 {
				return nil, bpsError("bps_client_mismatch")
			}
			for _, item := range list {
				if item == pair.audience {
					matched = true
				}
			}
		}
		if !matched {
			return nil, bpsError("bps_audience_mismatch")
		}
		if index == 1 {
			confirmed = true
		}
	}
	if !confirmed {
		return nil, bpsError("bps_client_mismatch")
	}
	if verified, ok := id["email_verified"]; ok && verified != true {
		return nil, bpsError("bps_email_unverified")
	}
	auth := bpsClaimObject(access, "https://api.openai.com/auth")
	if auth == nil {
		auth = bpsClaimObject(access, "auth")
	}
	idAuth := bpsClaimObject(id, "https://api.openai.com/auth")
	if idAuth == nil {
		idAuth = bpsClaimObject(id, "auth")
	}
	subject, err := bpsIdentityField("bps_subject_mismatch", true, access["sub"], id["sub"])
	if err != nil {
		return nil, err
	}
	workspace, err := bpsIdentityField("bps_workspace_mismatch", true, auth["chatgpt_account_id"], idAuth["chatgpt_account_id"], payload["chatgpt_account_id"])
	if err != nil {
		return nil, err
	}
	emails := []any{bpsClaimObject(access, "https://api.openai.com/profile")["email"], access["email"], id["email"], payload["email"]}
	for i, value := range emails {
		if text, ok := value.(string); ok {
			emails[i] = strings.ToLower(strings.TrimSpace(text))
		}
	}
	email, err := bpsIdentityField("bps_email_mismatch", false, emails...)
	if err != nil {
		return nil, err
	}
	if _, err := bpsIdentityField("bps_subject_mismatch", true, subject, bpsExpected(expected.Subject)); err != nil {
		return nil, err
	}
	if _, err := bpsIdentityField("bps_workspace_mismatch", true, workspace, bpsExpected(expected.WorkspaceID)); err != nil {
		return nil, err
	}
	if expected.Email != "" && !strings.EqualFold(email, strings.TrimSpace(expected.Email)) {
		return nil, bpsError("bps_email_mismatch")
	}
	if value, present := payload["expires_in"]; present {
		seconds, valid := value.(float64)
		if !valid || seconds <= 0 || seconds > 366*86400 {
			return nil, bpsError("bps_invalid_token_expiry")
		}
	}
	if response.ExpiresIn > 0 {
		candidate := now.Add(time.Duration(response.ExpiresIn) * time.Second)
		if expiry.IsZero() || candidate.Before(expiry) {
			expiry = candidate
		}
	}
	if expiry.IsZero() || !now.Before(expiry) {
		return nil, bpsError("bps_invalid_token_expiry")
	}
	return &BPSOAuthToken{TokenResponse: response, BPSOAuthIdentity: BPSOAuthIdentity{Subject: subject, WorkspaceID: workspace, Email: email}, ClientID: BPSExcelClientID, ExpiresAt: expiry}, nil
}
