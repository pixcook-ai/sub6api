package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This contract exercises a real CLI against the gateway service and a local
// synthetic upstream. It does not exercise production authentication or telemetry.
func TestClaudeCLIConsumesGatewayAPIKeyPassthroughStream(t *testing.T) {
	binary := os.Getenv("CLAUDE_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("set CLAUDE_CLI_TEST_BINARY to a Claude CLI binary to run the local protocol contract")
	}
	gin.SetMode(gin.TestMode)
	t.Run("text", func(t *testing.T) {
		runClaudeCLIGatewayStreamContract(t, binary, "text")
	})
	t.Run("read_tool_roundtrip", func(t *testing.T) {
		runClaudeCLIGatewayStreamContract(t, binary, "read_tool_roundtrip")
	})
	t.Run("stream_overloaded_then_success", func(t *testing.T) {
		runClaudeCLIGatewayStreamContract(t, binary, "stream_overloaded_then_success")
	})
}

// Both credentials are synthetic: this checks OAuth request forwarding, not
// real account login, refresh, or the per-account identity cache.
func TestClaudeCLIConsumesGatewayOAuthStream(t *testing.T) {
	binary := os.Getenv("CLAUDE_CLI_TEST_BINARY")
	if binary == "" {
		t.Skip("set CLAUDE_CLI_TEST_BINARY to run the local OAuth protocol contract")
	}
	gin.SetMode(gin.TestMode)
	runClaudeCLIGatewayStreamContract(t, binary, "oauth_text")
}

func runClaudeCLIGatewayStreamContract(t *testing.T, binary string, scenario string) {
	t.Helper()
	readTool := scenario == "read_tool_roundtrip"
	oauth := scenario == "oauth_text"
	const model = "claude-sonnet-4-5-20250929"
	const expectedText = "CLAUDE_GATEWAY_CONTRACT_OK"
	const finalResponse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_contract","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"CLAUDE_GATEWAY_CONTRACT_OK"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	workingDir := t.TempDir()
	const fixtureText = "CLAUDE_READ_FIXTURE_CONTENT"
	fixturePath := filepath.Join(workingDir, "fixture.txt")
	var toolResponse string
	if readTool {
		require.NoError(t, os.WriteFile(fixturePath, []byte(fixtureText), 0600))
		input, err := json.Marshal(map[string]string{"file_path": fixturePath})
		require.NoError(t, err)
		delta, err := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)},
		})
		require.NoError(t, err)
		toolResponse = "event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_contract_read","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_contract_read","name":"Read","input":{}}}` + "\n\n" +
			"event: content_block_delta\ndata: " + string(delta) + "\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}

	started := time.Now()
	var messageRequests atomic.Int64
	var forwardingFailures atomic.Int64
	var oauthHeadersPreserved atomic.Bool
	var streamErrorForwarded atomic.Bool
	var readResultForwarded atomic.Bool
	referenceDir := os.Getenv("CLAUDE_CLI_REFERENCE_DIR")
	var referenceMu sync.Mutex
	var exchanges []claudeCLIReferenceExchange
	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) {
		responseWriter := &claudeCLIContractResponseWriter{ResponseWriter: c.Writer}
		c.Writer = responseWriter
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<20))
		if err != nil {
			forwardingFailures.Add(1)
			c.Status(http.StatusBadRequest)
			return
		}
		if !gjson.GetBytes(body, "stream").Bool() {
			forwardingFailures.Add(1)
			c.Status(http.StatusBadRequest)
			return
		}
		cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
		requestNumber := messageRequests.Add(1)
		response := finalResponse
		if readTool && requestNumber == 1 {
			response = toolResponse
		}
		if scenario == "stream_overloaded_then_success" && requestNumber == 1 {
			response = strings.SplitN(finalResponse, "\n\n", 2)[0] + "\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Synthetic stream overload\"}}\n\n"
		}
		upstream := &anthropicHTTPUpstreamRecorder{
			resp: &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": {"text/event-stream"},
					"Request-Id":   {"req_contract"},
				},
				Body: io.NopCloser(strings.NewReader(response)),
			},
		}
		svc := &GatewayService{
			cfg:                  cfg,
			responseHeaderFilter: compileResponseHeaderFilter(cfg),
			httpUpstream:         upstream,
			rateLimitService:     &RateLimitService{},
			deferredService:      &DeferredService{},
		}
		parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
		if err != nil {
			forwardingFailures.Add(1)
			c.Status(http.StatusBadRequest)
			return
		}
		account := newAnthropicAPIKeyAccountForTest()
		if oauth {
			account.Type = AccountTypeOAuth
			account.Credentials = map[string]any{"access_token": "synthetic-upstream-oauth-token"}
			account.Extra["anthropic_passthrough"] = false
		}
		if _, err := svc.Forward(c.Request.Context(), c, account, parsed); err != nil {
			forwardingFailures.Add(1)
			t.Logf("turn=%d elapsed=%s Forward error=%T: %v", requestNumber, time.Since(started), err, err)
			if !c.Writer.Written() {
				c.Status(http.StatusInternalServerError)
			}
		}
		if oauth && upstream.lastReq != nil {
			outbound := upstream.lastReq.Header
			authorizationCount, credentialsIsolated := 0, true
			for key, values := range outbound {
				if strings.EqualFold(key, "Authorization") {
					authorizationCount += len(values)
					for _, value := range values {
						credentialsIsolated = credentialsIsolated && value == "Bearer synthetic-upstream-oauth-token"
					}
				}
				if strings.EqualFold(key, "X-Api-Key") {
					credentialsIsolated = false
				}
			}
			oauthHeadersPreserved.Store(
				c.GetHeader("Authorization") == "Bearer synthetic-client-oauth-token" &&
					authorizationCount == 1 && credentialsIsolated &&
					parsed.MetadataUserID != "" &&
					gjson.GetBytes(upstream.lastBody, "metadata.user_id").String() == parsed.MetadataUserID &&
					c.GetHeader("X-Stainless-OS") != "" && c.GetHeader("X-Stainless-Package-Version") != "" &&
					getHeaderRaw(outbound, "user-agent") == c.GetHeader("User-Agent") &&
					getHeaderRaw(outbound, "x-stainless-os") == c.GetHeader("X-Stainless-OS") &&
					getHeaderRaw(outbound, "x-stainless-package-version") == c.GetHeader("X-Stainless-Package-Version") &&
					strings.Contains(getHeaderRaw(outbound, "anthropic-beta"), "oauth-2025-04-20"))
		}
		if scenario == "stream_overloaded_then_success" && requestNumber == 1 {
			streamErrorForwarded.Store(c.Writer.Status() == http.StatusOK &&
				strings.Contains(responseWriter.body.String(), "event: error\n") &&
				strings.Contains(responseWriter.body.String(), `"type":"overloaded_error"`) &&
				!strings.Contains(responseWriter.body.String(), "event: message_stop"))
		}
		t.Logf("turn=%d elapsed=%s downstream_status=%d", requestNumber, time.Since(started), c.Writer.Status())
		if referenceDir != "" && upstream.lastReq != nil {
			exchange := claudeCLIReferenceExchange{
				Turn:     requestNumber,
				Inbound:  captureClaudeCLIReference(c.Request, body),
				Outbound: captureClaudeCLIReference(upstream.lastReq, upstream.lastBody),
			}
			referenceMu.Lock()
			exchanges = append(exchanges, exchange)
			referenceMu.Unlock()
		}
		for _, message := range gjson.GetBytes(upstream.lastBody, "messages").Array() {
			for _, block := range message.Get("content").Array() {
				if block.Get("type").String() == "tool_result" &&
					block.Get("tool_use_id").String() == "toolu_contract_read" &&
					!block.Get("is_error").Bool() &&
					strings.Contains(block.Get("content").String(), fixtureText) {
					readResultForwarded.Store(true)
				}
			}
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()

	// Only the gateway's loopback address bypasses the rejecting proxy. This
	// protects against incidental CLI background traffic during the contract.
	rejectProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer rejectProxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	tools, prompt := "", "Return the local protocol fixture response."
	if readTool {
		tools = "Read"
		prompt = fmt.Sprintf("Read %s, then reply briefly.", fixturePath)
	}
	args := []string{
		"--setting-sources", "", "--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`, "--tools", tools,
		"--allowedTools", tools, "--permission-mode", "dontAsk",
		"--no-session-persistence", "--model", model,
		"--output-format", "json", "--system-prompt", "Reply briefly.",
		"-p", prompt,
	}
	if !oauth {
		args = append([]string{"--bare"}, args...)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workingDir
	cmd.WaitDelay = 2 * time.Second
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "SystemRoot"} {
		if value, exists := os.LookupEnv(key); exists {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env,
		"CLAUDE_CONFIG_DIR="+t.TempDir(),
		"ANTHROPIC_BASE_URL="+server.URL,
		"HTTP_PROXY="+rejectProxy.URL,
		"HTTPS_PROXY="+rejectProxy.URL,
		"ALL_PROXY="+rejectProxy.URL,
		"NO_PROXY=127.0.0.1,localhost",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"TERM=dumb",
	)
	if oauth {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=synthetic-client-oauth-token")
	} else {
		cmd.Env = append(cmd.Env, "ANTHROPIC_API_KEY=local-contract-key")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	server.Close()
	t.Logf("scenario=%s elapsed=%s requests=%d forward_errors=%d process_error=%v", scenario, time.Since(started), messageRequests.Load(), forwardingFailures.Load(), err)
	require.NoError(t, ctx.Err(), "CLI must finish within the contract deadline")
	require.NoError(t, err, "CLI failed or timed out: %s", stderr.String())
	require.Greater(t, messageRequests.Load(), int64(0), "CLI must send its request through the gateway service")
	if scenario == "text" || readTool || oauth {
		require.Zero(t, forwardingFailures.Load(), "gateway must finish the stream successfully")
	}
	var result struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	require.False(t, result.IsError)
	require.Equal(t, expectedText, result.Result)
	if oauth {
		require.True(t, oauthHeadersPreserved.Load(), "OAuth forwarding must preserve native CLI protocol headers and use the selected upstream credential")
	}
	if scenario == "stream_overloaded_then_success" {
		require.True(t, streamErrorForwarded.Load(), "the CLI must receive the actual upstream SSE error through Forward")
		require.Equal(t, int64(2), messageRequests.Load(), "CLI must retry the failed stream once and finish the successful response")
		require.Equal(t, int64(1), forwardingFailures.Load(), "only the initial error stream should be incomplete")
	}
	if readTool {
		require.GreaterOrEqual(t, messageRequests.Load(), int64(2))
		require.True(t, readResultForwarded.Load(), "gateway must forward the real CLI Read result to the next upstream turn")
	}
	if referenceDir != "" {
		sort.Slice(exchanges, func(i, j int) bool { return exchanges[i].Turn < exchanges[j].Turn })
		mode := "bare_api_key_passthrough"
		if oauth {
			mode = "oauth_synthetic_account"
		}
		reference := struct {
			Platform  string                       `json:"platform"`
			Mode      string                       `json:"mode"`
			Exchanges []claudeCLIReferenceExchange `json:"exchanges"`
		}{runtime.GOOS + "/" + runtime.GOARCH, mode, exchanges}
		encoded, err := json.MarshalIndent(reference, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(referenceDir, 0700))
		require.NoError(t, writeClaudeCLIReference(filepath.Join(referenceDir, filepath.Base(t.Name())+".json"), encoded))
	}
}

// Capture the bytes actually sent by Forward while retaining Gin's flush support.
type claudeCLIContractResponseWriter struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *claudeCLIContractResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	_, _ = w.body.Write(body[:n])
	return n, err
}

func (w *claudeCLIContractResponseWriter) WriteString(body string) (int, error) {
	n, err := w.ResponseWriter.WriteString(body)
	_, _ = w.body.WriteString(body[:n])
	return n, err
}

func writeClaudeCLIReference(path string, encoded []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".claude-reference-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

type claudeCLIReferenceExchange struct {
	Turn     int64                     `json:"turn"`
	Inbound  claudeCLIRequestReference `json:"inbound"`
	Outbound claudeCLIRequestReference `json:"outbound"`
}

type claudeCLIRequestReference struct {
	Method        string            `json:"method"`
	Endpoint      string            `json:"endpoint"`
	CLIVersion    string            `json:"cli_version,omitempty"`
	Headers       map[string]string `json:"headers"`
	Configuration map[string]any    `json:"configuration"`
	BodyShape     any               `json:"body_shape"`
}

// captureClaudeCLIReference handles only the controlled synthetic fixtures in
// this test. Object keys are retained, so it is not a production-data redactor.
func captureClaudeCLIReference(req *http.Request, body []byte) claudeCLIRequestReference {
	headerValues := make(map[string]string)
	for _, name := range []string{
		"user-agent", "content-type", "accept", "accept-encoding",
		"anthropic-version", "anthropic-beta",
		"x-stainless-lang", "x-stainless-package-version", "x-stainless-os",
		"x-stainless-arch", "x-stainless-runtime", "x-stainless-runtime-version",
		"x-stainless-retry-count", "x-stainless-timeout", "x-stainless-helper-method",
	} {
		if value := getHeaderRaw(req.Header, name); value != "" {
			headerValues[name] = value
		}
	}
	configuration := make(map[string]any)
	for _, path := range []string{
		"model", "stream", "max_tokens", "temperature", "top_p", "top_k",
		"thinking.type", "thinking.budget_tokens", "output_config.effort",
	} {
		value := gjson.GetBytes(body, path)
		if value.Exists() && !value.IsObject() && !value.IsArray() {
			configuration[path] = value.Value()
		}
	}
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	return claudeCLIRequestReference{
		Method:        req.Method,
		Endpoint:      req.URL.Path,
		CLIVersion:    ExtractCLIVersion(getHeaderRaw(req.Header, "user-agent")),
		Headers:       headerValues,
		Configuration: configuration,
		BodyShape:     claudeCLIJSONShape(decoded),
	}
}

func claudeCLIJSONShape(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		shape := make(map[string]any, len(typed))
		for key, child := range typed {
			shape[key] = claudeCLIJSONShape(child)
		}
		return shape
	case []any:
		shape := make([]any, len(typed))
		for i, child := range typed {
			shape[i] = claudeCLIJSONShape(child)
		}
		return shape
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	default:
		return "null"
	}
}

func TestClaudeCLIReferenceRedactsPrivateValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.330")
	req.Header.Set("X-Stainless-OS", "Linux")
	req.Header.Set("Authorization", "Bearer private-credential")
	req.Header.Set("Cookie", "private-cookie")
	req.Header.Set("X-Client-Request-Id", "private-request-id")
	body := []byte(`{"model":"claude-sonnet-4-5-20250929","stream":true,"max_tokens":1024,"metadata":{"user_id":"private-user-id"},"system":"private-system-prompt","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"private-tool-id","content":"/private/fixture-path"}]}]}`)
	reference := captureClaudeCLIReference(req, body)
	require.Equal(t, "2.1.330", reference.CLIVersion)
	require.Equal(t, "Linux", reference.Headers["x-stainless-os"])
	require.Equal(t, true, reference.Configuration["stream"])
	require.Equal(t, float64(1024), reference.Configuration["max_tokens"])
	encoded, err := json.Marshal(reference)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-")
	require.NotContains(t, string(encoded), "/private/")
	require.Equal(t, "string", gjson.GetBytes(encoded, "body_shape.metadata.user_id").String())
	require.Equal(t, "string", gjson.GetBytes(encoded, "body_shape.messages.0.content.0.tool_use_id").String())
}

func TestClaudeCLIReferenceAtomicReplaceRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reference.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0644))
	require.NoError(t, os.Chmod(path, 0644))
	require.NoError(t, writeClaudeCLIReference(path, []byte(`{"platform":"linux/amd64"}`)))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"platform":"linux/amd64"}`, string(content))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestClaudeCLIReferenceFailedReplaceRemovesTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing-directory")
	require.NoError(t, os.Mkdir(path, 0700))
	require.Error(t, writeClaudeCLIReference(path, []byte(`{}`)))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "existing-directory", entries[0].Name())
	require.True(t, entries[0].IsDir())
}
