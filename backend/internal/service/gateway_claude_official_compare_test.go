package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This adapter exercises the final request through an actual HTTP client/server.
// Production request construction is unchanged; only the cloned URL destination
// is replaced with the test listener. It does not test TLS or account validity.
type claudeOfficialLoopbackUpstream struct {
	client *http.Client
	target *url.URL
}

func (u *claudeOfficialLoopbackUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	clonedURL := *req.URL
	clonedURL.Scheme, clonedURL.Host = u.target.Scheme, u.target.Host
	cloned.URL = &clonedURL
	return u.client.Do(cloned)
}

func (u *claudeOfficialLoopbackUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

type claudeOfficialWireRequest struct {
	path, method string
	headers      http.Header
	body         []byte
	err          error
}

type claudeOfficialCapturedRequest struct {
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

func loadClaudeOfficialCapturedRequest(t *testing.T, mode string) claudeOfficialCapturedRequest {
	t.Helper()
	filename := "claude-2.1.330-attribution-" + mode + ".json"
	root := filepath.Join("testdata", "claude-official")
	raw, err := os.ReadFile(filepath.Join(root, filename))
	require.NoError(t, err)
	manifestRaw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	require.NoError(t, err)
	var manifest struct {
		Package        string `json:"package"`
		Version        string `json:"version"`
		Integrity      bool   `json:"integrity_verified"`
		ArchiveMatches bool   `json:"executable_matches_verified_archive"`
		Captures       []struct {
			Fixture   string `json:"fixture"`
			SHA256    string `json:"fixture_sha256"`
			Completed bool   `json:"completed"`
		} `json:"captures"`
	}
	require.NoError(t, json.Unmarshal(manifestRaw, &manifest))
	require.Equal(t, "@anthropic-ai/claude-code-darwin-arm64", manifest.Package)
	require.Equal(t, "2.1.330", manifest.Version)
	require.True(t, manifest.Integrity)
	require.True(t, manifest.ArchiveMatches)
	verified := false
	for _, capture := range manifest.Captures {
		if capture.Fixture == filename {
			digest := sha256.Sum256(raw)
			require.Equal(t, capture.SHA256, hex.EncodeToString(digest[:]))
			require.True(t, capture.Completed)
			verified = true
		}
	}
	require.True(t, verified, "official fixture must be listed in the verified runtime manifest")
	var fixture struct {
		ExitCode int                             `json:"exit_code"`
		Requests []claudeOfficialCapturedRequest `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Zero(t, fixture.ExitCode)
	require.Len(t, fixture.Requests, 1)
	return fixture.Requests[0]
}

func TestGatewayService_ClaudeOfficialRuntimeLocalHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"messages", "count_tokens"} {
		for _, fixtureMode := range []string{"default", "off"} {
			for _, removeUA := range []bool{false, true} {
				clientKind := "native"
				if removeUA {
					clientKind = "no_ua_body_recognized"
					if fixtureMode == "off" {
						clientKind = "no_ua_mimic"
					}
				}
				t.Run(endpoint+"/"+fixtureMode+"/"+clientKind, func(t *testing.T) {
					runClaudeOfficialLocalHTTP(t, endpoint, fixtureMode, removeUA, false)
				})
			}
		}
	}
	// These are explicit synthetic variants derived in memory from the captured
	// default request, not claims that the official isolated run emitted them.
	for _, endpoint := range []string{"messages", "count_tokens"} {
		for _, removeUA := range []bool{false, true} {
			name := endpoint + "/synthetic_source_fields/native"
			if removeUA {
				name = endpoint + "/synthetic_source_fields/no_ua_body_recognized"
			}
			t.Run(name, func(t *testing.T) {
				runClaudeOfficialLocalHTTP(t, endpoint, "default", removeUA, true)
			})
		}
	}
}

func runClaudeOfficialLocalHTTP(t *testing.T, endpoint, fixtureMode string, removeUA, provenanceVariant bool) {
	t.Helper()
	fixture := loadClaudeOfficialCapturedRequest(t, fixtureMode)
	body := append([]byte(nil), fixture.Body...)
	if provenanceVariant {
		billing := gjson.GetBytes(body, "system.0.text").String()
		var err error
		body, err = sjson.SetBytes(body, "system.0.text", billing+" cc_gateway=env; cch=source_cch_001; future_source=keep_exact;")
		require.NoError(t, err)
		body, err = sjson.SetBytes(body, "metadata.future_source_field", map[string]any{"keep": "unchanged"})
		require.NoError(t, err)
	}
	incoming := append([]byte(nil), body...)
	received := make(chan claudeOfficialWireRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wire, readErr := io.ReadAll(req.Body)
		received <- claudeOfficialWireRequest{path: req.URL.RequestURI(), method: req.Method, headers: req.Header.Clone(), body: wire, err: readErr}
		if strings.Contains(req.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"input_tokens\":32}")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req_local_official_gateway_compare")
		model, _ := json.Marshal(gjson.GetBytes(wire, "model").String())
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_local_compare\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%s,\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":32,\"output_tokens\":0}}}\n\n", model)
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"CAPTURE_OK\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:     &claudeOfficialLoopbackUpstream{client: server.Client(), target: target},
		rateLimitService: &RateLimitService{}, deferredService: &DeferredService{},
	}
	account := &Account{ID: 987001, Name: "official-loopback-only", Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic_gateway_oauth_token"}, Status: StatusActive, Schedulable: true}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	path := "/v1/messages?beta=true"
	if endpoint == "count_tokens" {
		path = "/v1/messages/count_tokens?beta=true"
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for name, value := range fixture.Headers {
		// These transport/auth values were redacted or describe the original
		// receiver. The gateway derives its own upstream authentication/length.
		switch strings.ToLower(name) {
		case "authorization", "host", "content-length", "connection":
			continue
		}
		c.Request.Header.Set(name, value)
	}
	if removeUA {
		c.Request.Header.Del("User-Agent")
	}
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	if endpoint == "count_tokens" {
		require.NoError(t, svc.ForwardCountTokens(context.Background(), c, account, parsed))
		require.JSONEq(t, "{\"input_tokens\":32}", recorder.Body.String())
	} else {
		require.True(t, parsed.Stream, "official fixture must exercise its original streaming request")
		result, err := svc.Forward(context.Background(), c, account, parsed)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Contains(t, recorder.Body.String(), "CAPTURE_OK")
	}
	var wire claudeOfficialWireRequest
	select {
	case wire = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("no final HTTP request reached the real local receiver")
	}
	require.NoError(t, wire.err)
	require.Equal(t, http.MethodPost, wire.method)
	require.Equal(t, path, wire.path)
	require.Equal(t, "Bearer synthetic_gateway_oauth_token", wire.headers.Get("Authorization"))
	require.JSONEq(t, gjson.GetBytes(incoming, "metadata").Raw, gjson.GetBytes(wire.body, "metadata").Raw,
		"without an identity-service fixture, this comparison must not invent identity mutations")
	isMimic := removeUA && fixtureMode == "off"
	if !isMimic {
		// This capture lacks the beta required for its message-level effort.
		// Account for that existing normalization only; all other fields stay strict.
		const messageOutputConfigBeta = "mid-conversation-output-config-2026-07-01"
		require.NotContains(t, strings.Split(c.GetHeader("anthropic-beta"), ","), messageOutputConfigBeta)
		require.NotContains(t, strings.Split(wire.headers.Get("anthropic-beta"), ","), messageOutputConfigBeta)
		require.JSONEq(t, `{"effort":"medium"}`, gjson.GetBytes(incoming, "messages.1.output_config").Raw)
		expectedMessages, err := sjson.Delete(gjson.GetBytes(incoming, "messages").Raw, "1.output_config")
		require.NoError(t, err)
		require.JSONEq(t, expectedMessages, gjson.GetBytes(wire.body, "messages").Raw)
		require.JSONEq(t, gjson.GetBytes(incoming, "output_config").Raw, gjson.GetBytes(wire.body, "output_config").Raw)
		require.JSONEq(t, gjson.GetBytes(incoming, "tools").Raw, gjson.GetBytes(wire.body, "tools").Raw)
		var inputSystem, outputSystem []string
		for _, block := range gjson.GetBytes(incoming, "system").Array() {
			if !strings.HasPrefix(block.Get("text").String(), "x-anthropic-billing-header:") {
				inputSystem = append(inputSystem, block.Raw)
			}
		}
		for _, block := range gjson.GetBytes(wire.body, "system").Array() {
			if !strings.HasPrefix(block.Get("text").String(), "x-anthropic-billing-header:") {
				outputSystem = append(outputSystem, block.Raw)
			}
		}
		require.Len(t, outputSystem, len(inputSystem))
		for i := range inputSystem {
			require.JSONEq(t, inputSystem[i], outputSystem[i], "non-attribution system block %d", i)
		}
	} else {
		require.Contains(t, getHeaderRaw(wire.headers, "User-Agent"), "claude-cli/")
		require.Len(t, gjson.GetBytes(wire.body, "tools").Array(), len(gjson.GetBytes(incoming, "tools").Array()))
		require.Equal(t, claudeCodeSystemPrompt, gjson.GetBytes(wire.body, "system.1.text").String())
	}
	if endpoint == "count_tokens" {
		require.False(t, gjson.GetBytes(wire.body, "stream").Exists())
		require.False(t, gjson.GetBytes(wire.body, "max_tokens").Exists())
	}
	if provenanceVariant {
		for _, field := range []string{"cc_gateway=env;", "cch=source_cch_001;", "future_source=keep_exact;"} {
			require.Contains(t, gjson.GetBytes(wire.body, "system.0.text").String(), field)
		}
	}
	diagnostic := GetClaudeOAuthCompatibilityDiagnostic(account.ID)
	require.NotNil(t, diagnostic)
	require.Equal(t, "transparent", diagnostic.Mode)
	require.Equal(t, "unverified", diagnostic.IntegrityStatus)
	require.Equal(t, endpoint, diagnostic.Endpoint)
	require.Equal(t, provenanceVariant, diagnostic.CCGatewayPresent)
	require.Equal(t, provenanceVariant, diagnostic.CCHPresent)
	require.False(t, diagnostic.MetadataChanged)
	t.Logf("official=%s endpoint=%s mimic=%v attribution=%d->%d body_changed=%v beta_changed=%v integrity=%s",
		fixtureMode, endpoint, isMimic, diagnostic.IncomingAttributionBlocks, diagnostic.OutgoingAttributionBlocks,
		diagnostic.BodyChanged, diagnostic.BetaChanged, diagnostic.IntegrityStatus)
}
