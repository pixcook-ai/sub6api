package service_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/claude"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const claudeMimicFixtureModel = "claude-sonnet-4-5-20250929"
const claudeMimicFixtureUpstreamToken = "synthetic-upstream-oauth-token"

// Recorded from the isolated Linux 2.1.330 OAuth CLI Messages probe.
const claudeMimicFixtureMessagesBeta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,thinking-token-count-2026-05-13,extended-cache-ttl-2025-04-11"

// Recorded from the isolated macOS 2.1.330 OAuth CLI count_tokens probe.
const claudeMimicFixtureCountBeta = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,token-counting-2024-11-01"

func TestClaudeOAuthMimicOutboundContract(t *testing.T) {
	const childEnv = "SUB6API_TEST_CLAUDE_MIMIC_CHILD"
	const cliVersion = "2.1.330"
	if os.Getenv(childEnv) != "1" {
		// The compatibility profile is initialized at startup. Isolate both the
		// profile and the settings cache from all other tests.
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		executable, err := os.Executable()
		require.NoError(t, err)
		profile, err := json.Marshal(map[string]any{
			"lang": "js", "package_version": "0.128.0", "os": "Linux", "arch": "x64",
			"runtime": "node", "runtime_version": "v26.12.1", "entrypoint": "sdk-cli",
			"model_betas": map[string][]string{
				claudeMimicFixtureModel: strings.Split(claudeMimicFixtureMessagesBeta, ","),
			},
			"count_tokens_model_betas": map[string][]string{
				claudeMimicFixtureModel: strings.Split(claudeMimicFixtureCountBeta, ","),
			},
		})
		require.NoError(t, err)
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestClaudeOAuthMimicOutboundContract$", "-test.v")
		cmd.Env = []string{childEnv + "=1", claude.CompatibilityProfileEnv + "=" + string(profile), claude.CLIVersionEnv + "=" + cliVersion}
		cmd.WaitDelay = time.Second
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		t.Logf("\n%s", output)
		return
	}

	gin.SetMode(gin.TestMode)
	for _, fixture := range []struct {
		encoding string
		compress func(io.Writer) (claudeMimicCompressor, error)
	}{
		{"gzip", func(w io.Writer) (claudeMimicCompressor, error) { return gzip.NewWriter(w), nil }},
		{"deflate", func(w io.Writer) (claudeMimicCompressor, error) { return zlib.NewWriter(w), nil }},
		{"br", func(w io.Writer) (claudeMimicCompressor, error) { return brotli.NewWriter(w), nil }},
		{"zstd", func(w io.Writer) (claudeMimicCompressor, error) { return zstd.NewWriter(w) }},
	} {
		t.Run(fixture.encoding, func(t *testing.T) {
			testClaudeOAuthMimicStream(t, cliVersion, fixture.encoding, fixture.compress)
		})
	}
	t.Run("beta_policy", testClaudeOAuthMimicBetaPolicy)
}

type claudeMimicCompressor interface {
	io.WriteCloser
	Flush() error
}

func testClaudeOAuthMimicStream(t *testing.T, cliVersion, encoding string, compress func(io.Writer) (claudeMimicCompressor, error)) {
	t.Helper()
	const genericUserAgent = "generic-messages-fixture/1.0"
	const prompt = "Synthetic OAuth protocol fixture."
	const text = "SYNTHETIC_OAUTH_SSE_OK"
	type observation struct {
		headers       http.Header
		body          []byte
		method        string
		path          string
		protocol      string
		isolatedAuth  bool
		privateFields bool
	}
	observed := make(chan observation, 1)
	allowLastFrames := make(chan struct{}, 1)
	defer close(allowLastFrames)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "invalid fixture request body", http.StatusBadRequest)
			return
		}
		auth := r.Header.Values("Authorization")
		headers := r.Header.Clone()
		headers.Del("Authorization")
		headers.Del("X-Api-Key")
		select {
		case observed <- observation{
			headers: headers, body: body, method: r.Method, path: r.URL.Path, protocol: r.Proto,
			isolatedAuth:  len(auth) == 1 && auth[0] == "Bearer "+claudeMimicFixtureUpstreamToken && len(r.Header.Values("X-Api-Key")) == 0,
			privateFields: gjson.GetBytes(body, "metadata.user_id").Exists() || bytes.Contains(body, []byte("x-anthropic-billing-header")),
		}:
		default:
			http.Error(w, "unexpected repeated fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req_mimic_loopback")
		w.Header().Set("Content-Encoding", encoding)
		compressed, err := compress(w)
		if err != nil {
			t.Errorf("initialize %s fixture: %v", encoding, err)
			return
		}
		defer compressed.Close()
		_, _ = io.WriteString(compressed, "event: message_start\n"+
			`data: {"type":"message_start","message":{"id":"msg_mimic","type":"message","role":"assistant","model":"`+claudeMimicFixtureModel+`","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":1}}}`+"\n\n")
		if err := compressed.Flush(); err != nil {
			t.Errorf("flush %s frame: %v", encoding, err)
			return
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush HTTP frame: %v", err)
			return
		}
		select {
		case <-allowLastFrames:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(compressed, "event: content_block_start\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
			"event: content_block_delta\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`+"\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`+"\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(upstream.Close)
	gateway, account := newClaudeMimicFixtureGateway(t, upstream.URL, claudeMimicContractSettings{})
	const message = `{"model":"claude-sonnet-4-5","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"` + prompt + `"}]}`
	parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(message)), service.PlatformAnthropic)
	require.NoError(t, err)
	require.Empty(t, parsed.MetadataUserID)
	type forwardResult struct {
		result *service.ForwardResult
		err    error
	}
	forwarded := make(chan forwardResult, 1)
	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) {
		ctx := service.WithHTTPUpstreamRedirectsDisabled(c.Request.Context())
		result, err := gateway.Forward(ctx, c, account, parsed)
		forwarded <- forwardResult{result: result, err: err}
	})
	downstream := httptest.NewServer(router)
	t.Cleanup(downstream.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/v1/messages", strings.NewReader(message))
	require.NoError(t, err)
	req.Header.Set("User-Agent", genericUserAgent)
	req.Header.Set("Authorization", "Bearer synthetic-client-token")
	req.Header.Set("X-Api-Key", "synthetic-client-api-key")
	resp, err := downstream.Client().Do(req)
	require.NoError(t, err, "gateway must deliver the first SSE frame before the upstream finishes")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	require.Equal(t, "req_mimic_loopback", resp.Header.Get("Request-Id"))
	require.Empty(t, resp.Header.Get("Content-Encoding"))
	stream := bufio.NewReader(resp.Body)
	firstEvent, err := stream.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "event: message_start\n", firstEvent)
	firstData, err := stream.ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, firstData, `"type":"message_start"`)
	separator, err := stream.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "\n", separator)
	allowLastFrames <- struct{}{}
	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	require.Contains(t, string(rest), text)
	require.Contains(t, string(rest), "event: message_stop\n")
	select {
	case forwarded := <-forwarded:
		require.NoError(t, forwarded.err)
		require.NotNil(t, forwarded.result)
		require.True(t, forwarded.result.Stream)
		require.Equal(t, 9, forwarded.result.Usage.InputTokens)
		require.Equal(t, 4, forwarded.result.Usage.OutputTokens)
	case <-ctx.Done():
		t.Fatal("gateway did not finish the SSE stream")
	}

	var wire observation
	select {
	case wire = <-observed:
	default:
		t.Fatal("the request did not traverse HTTP to the fixture receiver")
	}
	require.True(t, wire.isolatedAuth, "only the synthetic selected account credential may reach the upstream")
	require.False(t, wire.privateFields, "the protocol fixture must omit device identity and billing blocks")
	require.Equal(t, http.MethodPost, wire.method)
	require.Equal(t, "/v1/messages", wire.path)
	require.Equal(t, claudeMimicFixtureModel, gjson.GetBytes(wire.body, "model").String())
	require.True(t, gjson.GetBytes(wire.body, "stream").Bool())
	require.Equal(t, int64(32), gjson.GetBytes(wire.body, "max_tokens").Int())
	require.Contains(t, string(wire.body), prompt)
	// Defaults generated by the ordinary OAuth branch distinguish this from
	// the test that forwards a request already generated by the native CLI.
	require.Equal(t, "claude-cli/"+cliVersion+" (external, sdk-cli)", wire.headers.Get("User-Agent"))
	require.Empty(t, wire.headers.Values("X-Stainless-Helper-Method"))
	require.Equal(t, "[]", gjson.GetBytes(wire.body, "tools").Raw)
	require.Equal(t, 1.0, gjson.GetBytes(wire.body, "temperature").Float())
	for key, value := range map[string]string{
		"Accept":                      "application/json",
		"Anthropic-Version":           "2023-06-01",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "0.128.0",
		"X-Stainless-OS":              "Linux",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v26.12.1",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "600",
	} {
		require.Equal(t, value, wire.headers.Get(key), key)
	}
	require.Equal(t, []string{"gzip, deflate, br, zstd"}, wire.headers.Values("Accept-Encoding"))
	require.Equal(t, []string{claudeMimicFixtureMessagesBeta}, wire.headers.Values("Anthropic-Beta"))
	t.Logf("direct loopback protocol=%s; configured SDK fields and %s SSE verified on the wire", wire.protocol, encoding)
}

func testClaudeOAuthMimicBetaPolicy(t *testing.T) {
	const blockedMessage = "beta blocked for the normalized model"
	const extraClientBeta = "synthetic-client-beta-2026-01-01"
	const incomingBeta = claude.BetaStructuredOutputs + "," + extraClientBeta
	const modelAlias = "claude-sonnet-4-5"
	const message = `{
		"model":"` + modelAlias + `", "max_tokens":32,
		"context_management":{"edits":[{"type":"clear_thinking_20251015"}]},
		"thinking":{"type":"adaptive","block_binding":"prefix"},
		"messages":[{"role":"user","content":"Synthetic beta policy fixture.","output_config":{"effort":"high"}}]
	}`
	filteredMessagesBeta := strings.Replace(claudeMimicFixtureMessagesBeta, ","+claude.BetaContextManagement, "", 1) + "," + claude.BetaStructuredOutputs
	for _, fixture := range []struct {
		name        string
		countTokens bool
		action      string
		ruleToken   string
		ruleModel   string
		incoming    string
		wantBeta    string
	}{
		{"messages_filtered", false, service.BetaPolicyActionFilter, claude.BetaContextManagement, claudeMimicFixtureModel, incomingBeta, filteredMessagesBeta},
		{"messages_filtered_by_inbound_model", false, service.BetaPolicyActionFilter, claude.BetaContextManagement, modelAlias, incomingBeta, filteredMessagesBeta},
		{"count_tokens_reference", true, service.BetaPolicyActionPass, claude.BetaContextManagement, claudeMimicFixtureModel, "", claudeMimicFixtureCountBeta},
		{"count_tokens_passed", true, service.BetaPolicyActionPass, claude.BetaContextManagement, claudeMimicFixtureModel, incomingBeta,
			claudeMimicFixtureCountBeta + "," + incomingBeta},
		{"count_tokens_filtered", true, service.BetaPolicyActionFilter, claude.BetaContextManagement, claudeMimicFixtureModel, incomingBeta,
			strings.Replace(claudeMimicFixtureCountBeta, ","+claude.BetaContextManagement, "", 1) + "," + incomingBeta},
		{"messages_blocked", false, service.BetaPolicyActionBlock, claude.BetaContextManagement, claudeMimicFixtureModel, "", ""},
		{"messages_structured_outputs_blocked", false, service.BetaPolicyActionBlock, claude.BetaStructuredOutputs, claudeMimicFixtureModel, incomingBeta, ""},
		{"messages_ignores_other_inbound_beta", false, service.BetaPolicyActionBlock, extraClientBeta, claudeMimicFixtureModel, incomingBeta,
			claudeMimicFixtureMessagesBeta + "," + claude.BetaStructuredOutputs},
		{"count_tokens_blocked", true, service.BetaPolicyActionBlock, claude.BetaContextManagement, claudeMimicFixtureModel, "", ""},
		{"count_tokens_inbound_blocked", true, service.BetaPolicyActionBlock, extraClientBeta, claudeMimicFixtureModel, incomingBeta, ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := "/v1/messages"
			if fixture.countTokens {
				path += "/count_tokens"
			}
			type observation struct {
				beta string
				body []byte
			}
			wire := make(chan observation, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
				if err != nil {
					http.Error(w, "invalid fixture request body", http.StatusBadRequest)
					return
				}
				select {
				case wire <- observation{beta: r.Header.Get("Anthropic-Beta"), body: body}:
				default:
					t.Error("unexpected repeated fixture request")
				}
				if r.URL.Path != path {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, path)
				}
				w.Header().Set("Content-Type", "application/json")
				if fixture.countTokens {
					_, _ = io.WriteString(w, `{"input_tokens":9}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"msg_policy","type":"message","role":"assistant","model":"`+claudeMimicFixtureModel+`","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":1}}`)
			}))
			t.Cleanup(upstream.Close)
			policy, err := json.Marshal(service.BetaPolicySettings{Rules: []service.BetaPolicyRule{{
				BetaToken:      fixture.ruleToken,
				Action:         fixture.action,
				Scope:          service.BetaPolicyScopeOAuth,
				ErrorMessage:   blockedMessage,
				ModelWhitelist: []string{fixture.ruleModel},
				FallbackAction: service.BetaPolicyActionPass,
			}}})
			require.NoError(t, err)
			gateway, account := newClaudeMimicFixtureGateway(t, upstream.URL, claudeMimicContractSettings{betaPolicy: string(policy)})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			ctx = service.WithHTTPUpstreamRedirectsDisabled(ctx)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(message))
			c.Request.Header.Set("User-Agent", "generic-messages-fixture/1.0")
			if fixture.incoming != "" {
				c.Request.Header.Set("Anthropic-Beta", fixture.incoming)
			}
			parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef([]byte(message)), service.PlatformAnthropic)
			require.NoError(t, err)
			if fixture.countTokens {
				err = gateway.ForwardCountTokens(ctx, c, account, parsed)
			} else {
				_, err = gateway.Forward(ctx, c, account, parsed)
			}
			if fixture.wantBeta == "" {
				var blocked *service.BetaBlockedError
				require.ErrorAs(t, err, &blocked)
				require.Equal(t, blockedMessage, blocked.Message)
				require.Empty(t, wire, "a blocked beta must prevent the HTTP request")
				if fixture.countTokens {
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
					require.Equal(t, blockedMessage, gjson.Get(recorder.Body.String(), "error.message").String())
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, recorder.Code)
			select {
			case got := <-wire:
				require.Equal(t, fixture.wantBeta, got.beta)
				require.Equal(t, claudeMimicFixtureModel, gjson.GetBytes(got.body, "model").String())
				require.Equal(t, fixture.action != service.BetaPolicyActionFilter, gjson.GetBytes(got.body, "context_management").Exists())
				require.False(t, gjson.GetBytes(got.body, "thinking.block_binding").Exists())
				require.False(t, gjson.GetBytes(got.body, "messages.0.output_config").Exists())
			default:
				t.Fatal("gateway did not send the fixture request")
			}
		})
	}
}

func newClaudeMimicFixtureGateway(t *testing.T, upstreamURL string, repo claudeMimicContractSettings) (*service.GatewayService, *service.Account) {
	t.Helper()
	endpoint, err := url.Parse(upstreamURL)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true // only for this loopback fixture
	settings := service.NewSettingService(repo, cfg)
	transport := &claudeMimicLoopbackTransport{HTTPUpstream: repository.NewHTTPUpstream(cfg), host: endpoint.Host}
	gateway := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, nil, nil,
		cfg, nil, nil, nil, &service.RateLimitService{}, nil, nil,
		transport, nil, nil, nil, nil, nil, settings, &service.TLSFingerprintProfileService{},
		nil, nil, nil, nil, nil,
	)
	account := &service.Account{
		ID:          1,
		Name:        "loopback-oauth-fixture",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": claudeMimicFixtureUpstreamToken},
		Extra:       map[string]any{"custom_base_url_enabled": true, "custom_base_url": upstreamURL},
	}
	return gateway, account
}

type claudeMimicContractSettings struct {
	service.SettingRepository
	betaPolicy string
}

func (claudeMimicContractSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{
		service.SettingKeyEnableFingerprintUnification:           "false",
		service.SettingKeyEnableClaudeOAuthSystemPromptInjection: "false",
		service.SettingKeyEnableCCHSigning:                       "false",
	}, nil
}

func (r claudeMimicContractSettings) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyBetaPolicySettings && r.betaPolicy != "" {
		return r.betaPolicy, nil
	}
	return "", service.ErrSettingNotFound
}

// The guard observes no request data. It prevents a routing regression from
// sending synthetic credentials outside loopback, while delegating I/O to the
// production repository transport.
type claudeMimicLoopbackTransport struct {
	service.HTTPUpstream
	host string
}

func (s *claudeMimicLoopbackTransport) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != s.host || proxyURL != "" {
		return nil, errors.New("OAuth protocol fixture must use its direct loopback upstream")
	}
	return s.HTTPUpstream.Do(req, proxyURL, accountID, concurrency)
}

func (s *claudeMimicLoopbackTransport) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	if req.URL.Scheme != "http" || req.URL.Host != s.host || proxyURL != "" {
		return nil, errors.New("OAuth protocol fixture must use its direct loopback upstream")
	}
	return s.HTTPUpstream.DoWithTLS(req, proxyURL, accountID, concurrency, profile)
}
