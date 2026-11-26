package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type tiboRouteCase struct {
	svc      *OpenAIGatewayService
	account  *Account
	upstream *httpUpstreamRecorder
}

// tiboAccountRepo is a fake database row. Like the real repository it owns
// its row and hands out copies, so a background probe persisting a verdict
// never writes the Extra map a request is reading. Tests change the row with
// setAccount and read writes with updateFor.
type tiboAccountRepo struct {
	AccountRepository
	mu      sync.Mutex
	account *Account
	updates map[int64]map[string]any
	gets    atomic.Int32
}

func cloneTiboAccount(account *Account) *Account {
	copied := *account
	copied.Extra = maps.Clone(account.Extra)
	copied.Credentials = maps.Clone(account.Credentials)
	return &copied
}

func (r *tiboAccountRepo) setAccount(mutate func(*Account)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mutate(r.account)
}

func (r *tiboAccountRepo) updateFor(id int64) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update, ok := r.updates[id]
	return update, ok
}

func (r *tiboAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.gets.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil || r.account.ID != id {
		return nil, errOpenAITiboAccountUnavailable
	}
	return cloneTiboAccount(r.account), nil
}

func (r *tiboAccountRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil {
		return nil, nil
	}
	return []Account{*cloneTiboAccount(r.account)}, nil
}

func (r *tiboAccountRepo) UpdateExtra(_ context.Context, id int64, extra map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = extra
	if r.account != nil && r.account.ID == id {
		next := maps.Clone(r.account.Extra)
		if next == nil {
			next = map[string]any{}
		}
		maps.Copy(next, extra)
		r.account.Extra = next
	}
	return nil
}

func cookieWSCompletion(model, answer string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"model": model, "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": answer}}}},
	}})
	return b
}

func cookieWSHTTPResponse(answer string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(cookieWSCompletion(openAITiboProbeModel, answer)) + "\n\n"))}
}

func newTiboRouteCase(t *testing.T, _, _ bool, responses ...*http.Response) *tiboRouteCase {
	t.Helper()
	// A test can construct another case while the first probe is still running.
	// Gin mode is process-global and is not needed to construct probe contexts.
	upstream := &httpUpstreamRecorder{responses: responses}
	on := true
	cfg := &config.Config{}
	cfg.Gateway.OpenAITiboRoute.Enabled = &on
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, toolCorrector: NewCodexToolCorrector()}
	account := &Account{
		ID: 23141, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 20, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-chatgpt"},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	svc.accountRepo = &tiboAccountRepo{account: cloneTiboAccount(account), updates: map[int64]map[string]any{}}
	return &tiboRouteCase{svc: svc, account: account, upstream: upstream}
}

func (tc *tiboRouteCase) forward(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	// Probe independently, then exercise the route with a model allowed by its latest point.
	tc.svc.openAITiboRequestVerdicts(context.Background(), tc.account)
	requestAccount := cloneTiboAccount(tc.account)
	if repo, ok := tc.svc.accountRepo.(*tiboAccountRepo); ok {
		if update, wrote := repo.updateFor(tc.account.ID); wrote {
			requestAccount.Extra[OpenAIAstraPolicyExtraKey] = update[OpenAIAstraPolicyExtraKey]
		}
	}
	model := "gpt-6-luna"
	if requestAccount.AllowsOpenAIOAuthModelPolicy("gpt-6-astra") {
		model = "gpt-6-astra"
	}
	result, err := tc.svc.Forward(context.Background(), c, requestAccount, []byte(`{"model":"`+model+`","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, rec, c
}

func requireTiboProbe(t *testing.T, body []byte) {
	t.Helper()
	require.Contains(t, string(body), openAITiboProbePrompt)
	require.Contains(t, string(body), `"model":"gpt-6-astra"`)
	require.Contains(t, string(body), `"effort":"xhigh"`)
}

// tiboSafeUpstream is a concurrency-safe synthetic HTTP boundary. It decodes
// this fork's wire encoding before separating probe and business requests.
type tiboSafeUpstream struct {
	mu         sync.Mutex
	probeGate  chan struct{}
	probes     int
	businesses int
}

func (u *tiboSafeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	body := tiboDecodeUpstreamBody(req.Header, raw)
	if strings.Contains(string(body), openAITiboProbePrompt) {
		if u.probeGate != nil {
			select {
			case <-u.probeGate:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		u.mu.Lock()
		u.probes++
		u.mu.Unlock()
		return cookieWSHTTPResponse("True"), nil
	}
	u.mu.Lock()
	u.businesses++
	u.mu.Unlock()
	return cookieWSHTTPResponse("http ok"), nil
}

func (u *tiboSafeUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// checkedFixtureValue makes fixture shape failures explicit before mutation.
func checkedFixtureValue[T any](value any) T {
	result, ok := value.(T)
	if !ok {
		panic("invalid test fixture type")
	}
	return result
}

// Decode the wire body in probe fixtures without changing existing upstream mocks.
func tiboDecodeUpstreamBody(h http.Header, raw []byte) []byte {
	if !strings.EqualFold(h.Get("Content-Encoding"), "zstd") {
		return raw
	}
	decoder, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	defer decoder.Close()
	body, err := io.ReadAll(decoder)
	if err != nil {
		panic(err)
	}
	return body
}
