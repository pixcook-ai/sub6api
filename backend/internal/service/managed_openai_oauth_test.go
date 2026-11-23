//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type managedOAuthTestRepo struct {
	AccountRepository
	account          *Account
	installs, events int
	setErrors        int
	reads            int
}

func (r *managedOAuthTestRepo) SetError(context.Context, int64, string) error {
	r.setErrors++
	return nil
}

func (r *managedOAuthTestRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, nil
}

func TestManagedOAuthTokenAcquisitionRevisionSurvivesRotationBeforeTransport(t *testing.T) {
	for _, mode := range []string{"http", "sse", "probe"} {
		t.Run(mode, func(t *testing.T) {
			a, r, s, id := managedOAuthFixture()
			ctx := context.Background()
			first := managedOAuthRequest(id, 0, 1)
			_, err := s.InstallManagedOAuthCredentials(ctx, a.ID, first)
			require.NoError(t, err)
			provider := NewOpenAITokenProvider(r, nil, nil)
			upstream := &managedOAuthHTTP{}
			gateway := &OpenAIGatewayService{accountRepo: r, httpUpstream: upstream, openAITokenProvider: provider, cfg: &config.Config{}}
			var requestCtx context.Context
			var token string
			if mode == "probe" {
				requestCtx, token, err = openAIProbeCredentialsForRequest(ctx, r, a)
			} else {
				requestCtx, token, _, err = gateway.credentialsForRequest(ctx, a)
			}
			require.NoError(t, err)
			require.Equal(t, first.AccessToken, token)
			// The required window: acquire r1/T, install r7/T, and only then enter transport.
			next := managedOAuthRequest(id, 1, 7)
			next.AccessToken = first.AccessToken
			next.ExpiresAt = first.ExpiresAt
			_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, next)
			require.NoError(t, err)
			readsBeforeTransport := r.reads
			upstream.beforeReturn = func() {
				require.Equal(t, readsBeforeTransport, r.reads, "transport must not reread DB to infer the revision")
			}
			if mode == "sse" {
				upstream.status = 200
				upstream.body = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"authentication_error\",\"code\":\"invalid_api_key\",\"message\":\"Unauthorized\"}}}\n\n"
			}
			request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+token)
			var resp *http.Response
			if mode == "probe" {
				resp, err = (&AccountTestService{accountRepo: r, httpUpstream: upstream}).doOpenAIAccountTestUpstream(request, "", a, false)
			} else {
				resp, err = gateway.doOpenAIUpstream(request, "", a)
			}
			require.NoError(t, err)
			defer resp.Body.Close()
			wireAttempt, _ := resp.Request.Context().Value(managedOAuthAttemptContextKey{}).(*Account)
			require.NotNil(t, wireAttempt)
			wireState, err := managedOAuthStateFromAccount(wireAttempt)
			require.NoError(t, err)
			require.EqualValues(t, 1, wireState.TokenRevision)
			if mode == "sse" {
				gin.SetMode(gin.TestMode)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				_, err = gateway.handleSSEToJSON(resp, c, a, []byte(upstream.body), "model", "model")
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.False(t, failure.ShouldRetryNextAccount())
			} else {
				require.Equal(t, 401, resp.StatusCode)
			}
			require.Equal(t, 1, upstream.calls)
			require.Zero(t, r.events)
			require.Zero(t, r.setErrors)
			state, err := s.GetManagedOAuthState(ctx, a.ID)
			require.NoError(t, err)
			require.True(t, state.Ready)
			require.False(t, state.AuthBlocked)
			require.EqualValues(t, 7, state.TokenRevision)
		})
	}
}

func TestManagedOAuthTransportRejectsUnboundLegacyToken(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	ctx := context.Background()
	req := managedOAuthRequest(id, 0, 1)
	_, err := s.InstallManagedOAuthCredentials(ctx, a.ID, req)
	require.NoError(t, err)
	token, err := NewOpenAITokenProvider(r, nil, nil).GetAccessToken(ctx, a)
	require.NoError(t, err)
	next := managedOAuthRequest(id, 1, 2)
	next.AccessToken = token
	next.ExpiresAt = req.ExpiresAt
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, next)
	require.NoError(t, err)
	upstream := &managedOAuthHTTP{}
	gateway := &OpenAIGatewayService{accountRepo: r, httpUpstream: upstream}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	_, err = gateway.doOpenAIUpstream(request, "", a)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
	require.Zero(t, upstream.calls)
	require.Zero(t, r.events)
}
func (r *managedOAuthTestRepo) InstallManagedOAuthCredentialsIfMatch(_ context.Context, _ int64, before, next ManagedOAuthState, token, expiry string) (bool, error) {
	current, err := managedOAuthStateFromAccount(r.account)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(*current, before) {
		return false, nil
	}
	r.account.Extra[ManagedOAuthExtraKey] = next
	r.account.Credentials["access_token"] = token
	r.account.Credentials["expires_at"] = expiry
	r.installs++
	return true, nil
}
func (r *managedOAuthTestRepo) RecordManagedOAuthEventIfMatch(_ context.Context, _ int64, before, next ManagedOAuthState, token string) (bool, error) {
	current, err := managedOAuthStateFromAccount(r.account)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(*current, before) || r.account.GetCredential("access_token") != token {
		return false, nil
	}
	r.account.Extra[ManagedOAuthExtraKey] = next
	r.events++
	return true, nil
}
func managedOAuthFixture() (*Account, *managedOAuthTestRepo, *adminServiceImpl, ManagedOAuthIdentity) {
	id := ManagedOAuthIdentity{Contract: ManagedOAuthContract, BackendID: "backend", BindingRef: "binding", ProfileRef: "profile", Provider: "codex", Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: "epoch"}
	a := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: false, ErrorMessage: "admin note", Credentials: map[string]any{}, Extra: map[string]any{ManagedOAuthExtraKey: ManagedOAuthState{ManagedOAuthIdentity: id, Events: []ManagedOAuthAuthEvent{}}}}
	r := &managedOAuthTestRepo{account: a}
	return a, r, &adminServiceImpl{accountRepo: r}, id
}
func managedOAuthRequest(id ManagedOAuthIdentity, expected, revision int64) *ManagedOAuthInstallRequest {
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	claims, _ := json.Marshal(map[string]any{"sub": id.Subject, "client_id": openai.ClientID, "exp": expiry.Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": id.WorkspaceID}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + "." + uuid.NewString()
	return &ManagedOAuthInstallRequest{ManagedOAuthIdentity: id, ExpectedTokenRevision: expected, TokenRevision: revision, OperationID: uuid.NewString(), AccessToken: token, ExpiresAt: expiry.Format(time.RFC3339)}
}
func TestManagedOAuthInstallAndUnauthorizedRecoveryPreserveAdminState(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	ctx := context.Background()
	req := managedOAuthRequest(id, 0, 3)
	got, err := s.InstallManagedOAuthCredentials(ctx, a.ID, req)
	require.NoError(t, err)
	require.EqualValues(t, 3, got.TokenRevision)
	require.True(t, got.Ready)
	require.False(t, a.Schedulable)
	require.Equal(t, "admin note", a.ErrorMessage)
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, req)
	require.NoError(t, err)
	require.Equal(t, 1, r.installs)
	different := *req
	different.AccessToken += "changed"
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, &different)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, a, "old-rejected-token"))
	require.Zero(t, r.events)
	require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, a, req.AccessToken))
	require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, a, req.AccessToken))
	require.Equal(t, 1, r.events)
	page, err := s.ListManagedOAuthAuthEvents(ctx, a.ID, 0)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	require.EqualValues(t, 3, page.Events[0].FailedTokenRevision)
	require.EqualValues(t, 1, page.Events[0].EventID)
	require.Equal(t, "upstream_oauth", page.Events[0].Origin)
	provider := NewOpenAITokenProvider(r, nil, nil)
	_, err = provider.GetAccessToken(ctx, a)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
	next := managedOAuthRequest(id, 3, 9)
	got, err = s.InstallManagedOAuthCredentials(ctx, a.ID, next)
	require.NoError(t, err)
	require.False(t, got.AuthBlocked)
	require.Equal(t, id.IdentityEpoch, got.IdentityEpoch)
	require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, a, req.AccessToken))
	require.Equal(t, 1, r.events)
	token, err := provider.GetAccessToken(ctx, a)
	require.NoError(t, err)
	require.Equal(t, next.AccessToken, token)
	require.False(t, a.Schedulable)
	require.Equal(t, StatusActive, a.Status)
	require.Equal(t, "admin note", a.ErrorMessage)
	stateJSON, _ := json.Marshal(got)
	require.NotContains(t, string(stateJSON), next.AccessToken)
	require.NotContains(t, string(stateJSON), "operation_hash")
}
func TestManagedOAuthInstallRejectsIdentityAndRevisionMismatch(t *testing.T) {
	for _, field := range []string{"subject", "workspace", "client", "expiry", "revision", "identity", "refresh"} {
		t.Run(field, func(t *testing.T) {
			a, r, s, id := managedOAuthFixture()
			req := managedOAuthRequest(id, 0, 1)
			switch field {
			case "subject":
				req.Subject = "wrong"
			case "workspace":
				req.WorkspaceID = "wrong"
			case "client":
				parts := strings.Split(req.AccessToken, ".")
				raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
				raw = []byte(strings.ReplaceAll(string(raw), openai.ClientID, "other-client"))
				parts[1] = base64.RawURLEncoding.EncodeToString(raw)
				req.AccessToken = strings.Join(parts, ".")
			case "expiry":
				req.ExpiresAt = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
			case "revision":
				req.TokenRevision = managedOAuthMaxRevision + 1
			case "identity":
				req.BindingRef = "other"
			case "refresh":
				a.Credentials["refresh_token"] = "injected"
			}
			_, err := s.InstallManagedOAuthCredentials(context.Background(), a.ID, req)
			require.Error(t, err)
			require.Zero(t, r.installs)
		})
	}
}
func TestManagedOAuthNewAccountOnlyAndPaused(t *testing.T) {
	_, _, _, id := managedOAuthFixture()
	input := &CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ManagedOAuth: &id, Credentials: map[string]any{}}
	extra, err := prepareManagedOAuthCreate(input, nil)
	require.NoError(t, err)
	require.NotNil(t, input.Schedulable)
	require.False(t, *input.Schedulable)
	require.Contains(t, extra, ManagedOAuthExtraKey)
	input.Credentials["refresh_token"] = "rt"
	_, err = prepareManagedOAuthCreate(input, nil)
	require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	_, err = prepareManagedOAuthCreate(&CreateAccountInput{}, map[string]any{ManagedOAuthExtraKey: nil})
	require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	a, _, _, _ := managedOAuthFixture()
	require.ErrorIs(t, managedOAuthAuthFieldsUnchanged(a, &UpdateAccountInput{Credentials: map[string]any{"access_token": "x"}}), ErrManagedOAuthRefresh)
}

func TestManagedOAuthCreatePersistsEmptyAuthorizationPaused(t *testing.T) {
	_, _, _, identity := managedOAuthFixture()
	repo := &createSchedulableRepoStub{}
	s := &adminServiceImpl{accountRepo: repo}
	created, err := s.CreateAccount(context.Background(), &CreateAccountInput{Name: "managed-new", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{}, ManagedOAuth: &identity, SkipDefaultGroupBind: true})
	require.NoError(t, err)
	require.False(t, repo.created.Schedulable)
	require.False(t, created.Schedulable)
	require.Empty(t, repo.created.GetCredential("access_token"))
	require.Empty(t, repo.created.GetCredential("refresh_token"))
	state, err := managedOAuthStateFromAccount(&repo.created)
	require.NoError(t, err)
	require.EqualValues(t, 0, state.TokenRevision)
	require.False(t, managedOAuthPublic(created, state).Initialized)
	require.False(t, created.IsOpenAIResponsesWebSocketV2Enabled())
}
func TestManagedOAuthAllRefreshEntriesFailClosed(t *testing.T) {
	a, _, _, _ := managedOAuthFixture()
	a.Extra[ManagedOAuthExtraKey] = nil
	a.Credentials["refresh_token"] = "never-consume"
	refresher := &OpenAITokenRefresher{}
	require.False(t, refresher.CanRefresh(a))
	require.False(t, refresher.NeedsRefresh(a, time.Hour))
	_, err := refresher.Refresh(context.Background(), a)
	require.ErrorIs(t, err, ErrManagedOAuthRefresh)
	_, err = (&OpenAIOAuthService{}).RefreshAccountToken(context.Background(), a)
	require.ErrorIs(t, err, ErrManagedOAuthRefresh)
	require.False(t, canRecoverOpenAIOAuth(a))
	a.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
	a.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
	require.False(t, a.IsOpenAIResponsesWebSocketV2Enabled())
	require.Equal(t, OpenAIWSIngressModeOff, a.ResolveOpenAIResponsesWebSocketV2Mode(OpenAIWSIngressModeCtxPool))
	require.False(t, a.IsCredentialUsableForShadow())
}

func TestManagedOAuthCannotCreateSparkShadow(t *testing.T) {
	a, _, s, _ := managedOAuthFixture()
	_, err := s.CreateShadow(context.Background(), a.ID, ShadowOptions{})
	require.ErrorIs(t, err, ErrManagedOAuthRefresh)
}

type managedOAuthWSDialer struct {
	calls int
}

func (d *managedOAuthWSDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	d.calls++
	return nil, 0, nil, ErrLiveUnavailable
}

func TestManagedOAuthTicketAndLiveFailClosedBeforeNetwork(t *testing.T) {
	for _, marker := range []string{"valid", "null", "malformed"} {
		t.Run(marker, func(t *testing.T) {
			a, repo, controller, identity := managedOAuthFixture()
			{
				// A ready credential must still be rejected by the endpoint guard.
				_, err := controller.InstallManagedOAuthCredentials(context.Background(), a.ID, managedOAuthRequest(identity, 0, 1))
				require.NoError(t, err)
				state, err := controller.GetManagedOAuthState(context.Background(), a.ID)
				require.NoError(t, err)
				require.True(t, state.Ready)
			}
			repo.reads = 0
			switch marker {
			case "null":
				a.Extra[ManagedOAuthExtraKey] = nil
			case "malformed":
				a.Extra[ManagedOAuthExtraKey] = "invalid"
			}
			upstream := &managedOAuthHTTP{}
			dialer := &managedOAuthWSDialer{}
			gateway := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openaiWSPassthroughDialer: dialer}
			ctx := context.Background()
			require.False(t, a.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
			require.False(t, isOpenAICodexTicketAccount(a))
			_, _, _, _, err := gateway.fireOpenAICodexTicketProbe(ctx, a, "token", "gpt-test", "", ModelTraceChallenge{}, time.Second)
			require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
			challengeCalls := 0
			_, err = gateway.runCodexTicketAttemptWithChallengeGenerator(ctx, a, "gpt-test", "manual", func() (ModelTraceChallenge, error) {
				challengeCalls++
				return ModelTraceChallenge{}, nil
			})
			require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
			require.Zero(t, challengeCalls)
			require.Zero(t, repo.reads, "direct managed ticket paths reject before DB or challenge work")
			// A stale ordinary scheduler snapshot must not bypass the DB reread guard.
			staleOrdinary := &Account{ID: a.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			_, err = gateway.runCodexTicketAttemptWithChallengeGenerator(ctx, staleOrdinary, "gpt-test", "manual", func() (ModelTraceChallenge, error) {
				challengeCalls++
				return ModelTraceChallenge{}, nil
			})
			require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
			require.Zero(t, challengeCalls)
			_, err = gateway.createUpstreamLiveCall(ctx, a, &LiveCallRequest{}, "")
			require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
			record := &LiveCallRecord{AccountID: a.ID, CallID: "call-test"}
			_, err = gateway.liveSidebandHeaders(ctx, a, record)
			require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
			_, err = gateway.dialLiveSideband(ctx, record)
			require.ErrorIs(t, err, ErrLiveUnavailable)
			require.Zero(t, upstream.calls)
			require.Zero(t, dialer.calls)
		})
	}
	ordinary := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.True(t, ordinary.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityLive))
	require.True(t, isOpenAICodexTicketAccount(ordinary))
}

type managedOAuthHTTP struct {
	HTTPUpstream
	calls        int
	status       int
	body         string
	beforeReturn func()
}

func (u *managedOAuthHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	if u.beforeReturn != nil {
		u.beforeReturn()
	}
	status := u.status
	if status == 0 {
		status = 401
	}
	body := u.body
	if body == "" {
		body = `{"error":"unauthorized"}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func TestManagedOAuthLate401SameBearerDoesNotBlockNewRevision(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	ctx := context.Background()
	first := managedOAuthRequest(id, 0, 1)
	_, err := s.InstallManagedOAuthCredentials(ctx, a.ID, first)
	require.NoError(t, err)
	attempt, err := readManagedOAuthCredentials(ctx, r, a)
	require.NoError(t, err)
	next := managedOAuthRequest(id, 1, 8)
	next.AccessToken = first.AccessToken
	next.ExpiresAt = first.ExpiresAt
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, next)
	require.NoError(t, err)
	require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, attempt, first.AccessToken))
	require.Zero(t, r.events)
	state, err := s.GetManagedOAuthState(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, state.Ready)
	require.EqualValues(t, 8, state.TokenRevision)
}

func TestManagedOAuthBypassesCacheAndNeverDisablesExpiredAccount(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	req := managedOAuthRequest(id, 0, 1)
	_, err := s.InstallManagedOAuthCredentials(context.Background(), a.ID, req)
	require.NoError(t, err)
	cache := newOpenAITokenCacheStub()
	cache.tokens[OpenAITokenCacheKey(a)] = "stale-token"
	provider := NewOpenAITokenProvider(r, cache, nil)
	token, err := provider.GetAccessToken(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, req.AccessToken, token)
	require.Zero(t, cache.getCalled)
	state, err := managedOAuthStateFromAccount(a)
	require.NoError(t, err)
	state.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	a.Extra[ManagedOAuthExtraKey] = *state
	_, err = provider.GetAccessToken(context.Background(), a)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
	require.Zero(t, r.setErrors)
	require.Equal(t, StatusActive, a.Status)
	require.False(t, a.Schedulable)
}

func TestManagedOAuthSemantic401RecordsAttemptAndStopsReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"stream", "passthrough-stream", "json", "passthrough-json"} {
		t.Run(mode, func(t *testing.T) {
			a, r, s, id := managedOAuthFixture()
			req := managedOAuthRequest(id, 0, 1)
			_, err := s.InstallManagedOAuthCredentials(context.Background(), a.ID, req)
			require.NoError(t, err)
			body := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_test\",\"status\":\"failed\",\"error\":{\"type\":\"authentication_error\",\"code\":\"invalid_api_key\",\"message\":\"Unauthorized\"}}}\n\n"
			upstream := &managedOAuthHTTP{status: 200, body: body}
			gateway := &OpenAIGatewayService{accountRepo: r, httpUpstream: upstream, cfg: &config.Config{}}
			requestCtx, token, _, err := gateway.credentialsForRequest(context.Background(), a)
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+token)
			resp, err := gateway.doOpenAIUpstream(request, "", a)
			require.NoError(t, err)
			defer resp.Body.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			switch mode {
			case "stream":
				_, err = gateway.handleStreamingResponse(context.Background(), resp, c, a, time.Now(), "model", "model")
			case "passthrough-stream":
				_, err = gateway.handleStreamingResponsePassthrough(context.Background(), resp, c, a, time.Now(), "model", "model")
			case "json":
				_, err = gateway.handleSSEToJSON(resp, c, a, []byte(body), "model", "model")
			case "passthrough-json":
				_, err = gateway.handlePassthroughSSEToJSON(resp, c, a, []byte(body), "model", "model")
			}
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, 401, failure.StatusCode)
			require.False(t, failure.ShouldRetryNextAccount())
			require.False(t, failure.RetryableOnSameAccount)
			require.False(t, failure.ShouldReportAccountScheduleFailure())
			require.Equal(t, 1, upstream.calls)
			require.Equal(t, 1, r.events)
			require.False(t, a.Schedulable)
			require.Zero(t, r.setErrors)
		})
	}
}

func TestManagedOAuthEventsExposeRetentionGap(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	ctx := context.Background()
	state, err := s.GetManagedOAuthState(ctx, a.ID)
	require.NoError(t, err)
	require.False(t, state.Initialized)
	for revision := int64(1); revision <= 130; revision++ {
		req := managedOAuthRequest(id, revision-1, revision)
		_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, req)
		require.NoError(t, err)
		require.NoError(t, recordManagedOAuthUnauthorized(ctx, r, a, req.AccessToken))
	}
	page, err := s.ListManagedOAuthAuthEvents(ctx, a.ID, 0)
	require.NoError(t, err)
	require.True(t, page.CursorGap)
	require.Len(t, page.Events, 128)
	require.EqualValues(t, 3, page.Events[0].EventID)
	require.EqualValues(t, 130, page.NextCursor)
	page, err = s.ListManagedOAuthAuthEvents(ctx, a.ID, 129)
	require.NoError(t, err)
	require.False(t, page.CursorGap)
	require.Len(t, page.Events, 1)
}
func TestManagedOAuthHTTP401DoesNotRefreshOrReplay(t *testing.T) {
	a, r, s, id := managedOAuthFixture()
	req := managedOAuthRequest(id, 0, 1)
	_, err := s.InstallManagedOAuthCredentials(context.Background(), a.ID, req)
	require.NoError(t, err)
	upstream := &managedOAuthHTTP{}
	gateway := &OpenAIGatewayService{accountRepo: r, httpUpstream: upstream, openAITokenProvider: NewOpenAITokenProvider(r, nil, nil)}
	requestCtx, token, _, err := gateway.credentialsForRequest(context.Background(), a)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.invalid/responses", strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := gateway.doOpenAIUpstream(request, "", a)
	require.NoError(t, err)
	require.Equal(t, 401, response.StatusCode)
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, 1, r.events)
	require.Equal(t, 1, r.installs)
	require.NoError(t, response.Body.Close())
}
func TestManagedOAuthBulkCredentialsRejectsWholeTargetSet(t *testing.T) {
	managed, _, _, _ := managedOAuthFixture()
	ordinary := &Account{ID: 78, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, filtered := range []bool{false, true} {
		t.Run(map[bool]string{false: "ids", true: "filter"}[filtered], func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{ordinary, managed}, listData: []Account{*ordinary, *managed}}
			s := &adminServiceImpl{accountRepo: repo}
			input := &BulkUpdateAccountsInput{AccountIDs: []int64{ordinary.ID, managed.ID}, Credentials: map[string]any{"refresh_token": "never-write"}}
			if filtered {
				input.AccountIDs = nil
				input.Filters = &BulkUpdateAccountFilters{Platform: PlatformOpenAI}
			}
			_, err := s.BulkUpdateAccounts(context.Background(), input)
			require.ErrorIs(t, err, ErrManagedOAuthRefresh)
			require.Zero(t, repo.bulkUpdateCalls)
			require.Empty(t, repo.updatedAccounts)
		})
	}
	s := &adminServiceImpl{}
	_, err := s.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{78}, Extra: map[string]any{ManagedOAuthExtraKey: nil}})
	require.ErrorIs(t, err, ErrManagedOAuthRefresh)
}
