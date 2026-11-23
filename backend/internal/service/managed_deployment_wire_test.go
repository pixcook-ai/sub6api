package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func managedWireTestContext() context.Context {
	return WithManagedDeploymentSession(context.Background(), &ManagedDeploymentSession{DeploymentID: "deployment", GroupID: 1, UserID: 2, APIKeyID: 3, SessionKey: "persistent-session", Binding: ManagedDeploymentBinding{Provider: "bps_excel", AccountID: 11, BackendID: "worker", BindingRef: "binding", ProfileRef: "profile", IdentityEpoch: "epoch", WorkspaceID: "workspace", Subject: "subject"}})
}
func TestManagedDeploymentWireAffinityScopesOnlyProtocolState(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.1","prompt_cache_key":"shared","client_metadata":{"session_id":"shared","thread_id":"thread","conversation_id":"shared"},"previous_response_id":"resp_native","input":[{"type":"item_reference","id":"item_native"},{"type":"reasoning","encrypted_content":"opaque-native"}],"metadata":{"session_id":"user-data"}}`)
	ctx := managedWireTestContext()
	scoped, err := scopeManagedDeploymentWirePayload(ctx, raw)
	require.NoError(t, err)
	second, err := scopeManagedDeploymentWirePayload(ctx, raw)
	require.NoError(t, err)
	require.Equal(t, scoped, second)
	expected, _ := ScopeManagedDeploymentAffinity(ctx, "shared")
	for _, path := range []string{"prompt_cache_key", "client_metadata.session_id", "client_metadata.conversation_id"} {
		require.Equal(t, expected, gjson.GetBytes(scoped, path).String())
	}
	require.NotEqual(t, "thread", gjson.GetBytes(scoped, "client_metadata.thread_id").String())
	require.Equal(t, "resp_native", gjson.GetBytes(scoped, "previous_response_id").String())
	require.Equal(t, "item_native", gjson.GetBytes(scoped, "input.0.id").String())
	require.Equal(t, "opaque-native", gjson.GetBytes(scoped, "input.1.encrypted_content").String())
	require.Equal(t, "user-data", gjson.GetBytes(scoped, "metadata.session_id").String())
	ordinary, err := scopeManagedDeploymentWirePayload(context.Background(), raw)
	require.NoError(t, err)
	require.Equal(t, raw, ordinary)
	base, _ := ManagedDeploymentSessionFromContext(ctx)
	variants := []ManagedDeploymentSession{*base, *base, *base, *base, *base, *base}
	variants[0].APIKeyID++
	variants[1].UserID++
	variants[2].GroupID++
	variants[3].SessionKey = "other-session"
	variants[4].Binding.IdentityEpoch = "other-epoch"
	variants[5].Binding.Provider = "codex"
	for _, variant := range variants {
		other, err := scopeManagedDeploymentWirePayload(WithManagedDeploymentSession(context.Background(), &variant), raw)
		require.NoError(t, err)
		require.NotEqual(t, expected, gjson.GetBytes(other, "prompt_cache_key").String())
	}
	empty, err := scopeManagedDeploymentWirePayload(ctx, []byte(`{"type":"response.create","model":"gpt-5.1"}`))
	require.NoError(t, err)
	fallback, _ := ScopeManagedDeploymentAffinity(ctx, "")
	require.Equal(t, fallback, gjson.GetBytes(empty, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(empty, "client_metadata").Exists(), "BPS request validation rejects an invented Codex metadata extension")
	for _, invalid := range []string{`{"client_metadata":[]}`, `{"client_metadata":{"session_id":5}}`, `{"prompt_cache_key":"one","prompt_cache_key":"two"}`} {
		_, err := scopeManagedDeploymentWirePayload(ctx, []byte(invalid))
		require.ErrorIs(t, err, ErrManagedDeploymentInvalid)
	}
	cancel := []byte(`{"type":"response.cancel","response_id":"resp_native"}`)
	preserved, err := scopeManagedDeploymentWirePayload(ctx, cancel)
	require.NoError(t, err)
	require.Equal(t, cancel, preserved)
	update, err := scopeManagedDeploymentWirePayload(ctx, []byte(`{"type":"session.update","session":{"client_metadata":{"session_id":"shared"}}}`))
	require.NoError(t, err)
	require.Equal(t, expected, gjson.GetBytes(update, "session.client_metadata.session_id").String())
}

func TestManagedDeploymentFinalHTTPWireScopesAffinity(t *testing.T) {
	raw := `{"model":"gpt-5.1","prompt_cache_key":"shared","client_metadata":{"session_id":"shared"},"previous_response_id":"resp_native","input":[{"type":"item_reference","id":"item_native"},{"type":"reasoning","encrypted_content":"opaque-native"}]}`
	type capture struct {
		header http.Header
		body   []byte
	}
	observed := make(chan capture, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		observed <- capture{r.Header.Clone(), body}
		_, _ = io.WriteString(w, `{"id":"resp_new"}`)
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	service := &OpenAIGatewayService{httpUpstream: &managedNoReplayHTTPUpstream{client: server.Client(), target: target}}
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	originalCtx := managedWireTestContext()
	original, _ := ManagedDeploymentSessionFromContext(originalCtx)
	other := *original
	other.APIKeyID++
	contexts := []context.Context{originalCtx, originalCtx, WithManagedDeploymentSession(context.Background(), &other), context.Background()}
	var first capture
	for index, ctx := range contexts {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(raw))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		for _, header := range []string{"session-id", "session_id", "conversation_id"} {
			request.Header.Set(header, "shared")
		}
		response, err := service.doOpenAIUpstream(request, "", account)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		got := <-observed
		if index == 3 {
			require.Equal(t, raw, string(got.body))
			require.Equal(t, "shared", got.header.Get("session-id"))
			continue
		}
		expected, _ := ScopeManagedDeploymentAffinity(ctx, "shared")
		for _, header := range []string{"session-id", "session_id", "conversation_id"} {
			require.Equal(t, expected, got.header.Get(header))
		}
		require.Equal(t, expected, gjson.GetBytes(got.body, "prompt_cache_key").String())
		require.Equal(t, expected, gjson.GetBytes(got.body, "client_metadata.session_id").String())
		require.Equal(t, "resp_native", gjson.GetBytes(got.body, "previous_response_id").String())
		require.Equal(t, "item_native", gjson.GetBytes(got.body, "input.0.id").String())
		require.Equal(t, "opaque-native", gjson.GetBytes(got.body, "input.1.encrypted_content").String())
		if index == 0 {
			first = got
		} else if index == 1 {
			require.Equal(t, first.body, got.body)
			require.Equal(t, first.header.Get("session-id"), got.header.Get("session-id"))
		} else {
			require.NotEqual(t, first.header.Get("session-id"), got.header.Get("session-id"))
		}
	}
}

func TestManagedDeploymentWirePreservesNonInferenceBodies(t *testing.T) {
	for _, path := range []string{"/v1/responses/resp_x/cancel", "/v1/responses/input_tokens", "/v1/files", "/v1/chat/completions"} {
		request, err := http.NewRequestWithContext(managedWireTestContext(), http.MethodPost, "http://loopback.invalid"+path, strings.NewReader("body-must-remain-verbatim"))
		require.NoError(t, err)
		scoped, err := scopeManagedDeploymentHTTPRequest(request)
		require.NoError(t, err)
		body, err := io.ReadAll(scoped.Body)
		require.NoError(t, err)
		require.Equal(t, "body-must-remain-verbatim", string(body))
		require.NotEmpty(t, scoped.Header.Get("session_id"))
	}
}

func TestManagedDeploymentSignedResourceClaimsExactScopedFinalWire(t *testing.T) {
	var calls, authorized atomic.Int32
	identity := managedfiles.Identity{BackendID: "backend-test", SubjectFingerprint: "sha256:" + strings.Repeat("1", 64), CredentialGeneration: "generation-test"}
	key := strings.Repeat("k", 32)
	observed := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		claims, err := managedfiles.VerifySessionBinding(key, r.Header.Get(managedfiles.SessionBindingHeader), body, r.Method, r.URL.Path, identity, time.Now())
		if err != nil {
			t.Error(err)
			http.Error(w, "bad signed digest", 400)
			return
		}
		require.Equal(t, "owner-2", claims.OwnerRef)
		require.Equal(t, "group-1", claims.GroupRef)
		require.Equal(t, "conversation-test", claims.ConversationRef)
		observed <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_signed","output":[]}`)
	}))
	defer server.Close()
	account := managedSchedulerTestAccount(11, 1)
	account.Credentials["base_url"] = server.URL
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_service_key"] = key
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	config, err := BPSFilesConfigForAccount(&account)
	require.NoError(t, err)
	state := &bpsSessionBindingState{caller: context.Background(), accountID: account.ID, config: config, owner: "owner-2", group: "group-1", conversation: "conversation-test", authorize: func(context.Context) error { authorized.Add(1); return nil }}
	state.negotiated.Store(true)
	ctx := context.WithValue(managedWireTestContext(), bpsSessionBindingKey{}, state)
	raw := `{"model":"gpt-5.1","prompt_cache_key":"shared","client_metadata":{"session_id":"shared"},"input":[{"type":"input_text","text":"hello"}]}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("session-id", "shared")
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	service := &OpenAIGatewayService{httpUpstream: &managedNoReplayHTTPUpstream{client: server.Client(), target: target}}
	response, err := service.doOpenAIUpstream(request, "", &account)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.EqualValues(t, 1, calls.Load())
	require.Positive(t, authorized.Load())
	actual := <-observed
	expected, _ := ScopeManagedDeploymentAffinity(ctx, "shared")
	require.Equal(t, expected, gjson.GetBytes(actual, "prompt_cache_key").String())
	require.Equal(t, expected, gjson.GetBytes(actual, "client_metadata.session_id").String())
}

type managedCleanupSource struct {
	io.Reader
	closes int
}

func (s *managedCleanupSource) Close() error { s.closes++; return nil }
func TestOpenAIResourceCleanupWaitsForBodyCloseAndRunsOnce(t *testing.T) {
	source := &managedCleanupSource{Reader: strings.NewReader("terminal")}
	cleanups := 0
	body := &openAIResourceCleanupBody{ReadCloser: source, cleanup: func() error { cleanups++; return nil }}
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "terminal", string(raw))
	require.Zero(t, cleanups, "read/filter/capture must finish before attempt cleanup")
	require.NoError(t, body.Close())
	require.NoError(t, body.Close())
	require.Equal(t, 1, cleanups)
	require.Equal(t, 1, source.closes)
}
