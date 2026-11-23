//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
)

type coreWorkerRecoveryStore struct {
	CoreOAuthRepository
	row CoreOAuthSealedProfile
}

func (s *coreWorkerRecoveryStore) ReadProfile(_ context.Context, ref string) (*CoreOAuthSealedProfile, error) {
	if ref != s.row.ProfileRef {
		return nil, ErrCoreOAuthProfileNotFound
	}
	row := s.row
	return &row, nil
}

func (s *coreWorkerRecoveryStore) CompareAndSwapProfile(_ context.Context, ref string, version int64, row CoreOAuthSealedProfile) (bool, error) {
	if ref != s.row.ProfileRef || version != s.row.Version {
		return false, nil
	}
	s.row = row
	return true, nil
}

type coreWorkerRecoveryProtocol struct {
	CoreOAuthProtocol
	refreshes int
}

type coreWorkerRecoveryBody struct {
	io.ReadCloser
	closed atomic.Bool
}

func (b *coreWorkerRecoveryBody) Close() error {
	err := b.ReadCloser.Close()
	b.closed.Store(true)
	return err
}

func (p *coreWorkerRecoveryProtocol) Refresh(_ context.Context, grant CoreOAuthGrant, _ CoreOAuthTransport) (*CoreOAuthGrant, error) {
	p.refreshes++
	grant.AccessToken, grant.RefreshToken = "synthetic-rotated-access", "synthetic-rotated-refresh"
	return &grant, nil
}

func TestCoreOAuthWorkerUnauthorizedReleasesInferenceConnectionWithoutReplay(t *testing.T) {
	var binding ManagedDeploymentBinding
	var snapshot *CoreOAuthCredentialSnapshot
	var inferenceCalls atomic.Int32
	var inferenceBody atomic.Pointer[coreWorkerRecoveryBody]
	const body = `{"error":{"message":"synthetic expired token"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/responses":
			inferenceCalls.Add(1)
			w.Header().Set("X-Synthetic-Worker-Error", "expired")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, body)
		case "/_control/v1/auth-events":
			require.True(t, inferenceBody.Load().closed.Load(), "underlying inference body must close before polling control events")
			require.Equal(t, "after=0", r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(coreWorkerEvents{Events: []coreWorkerEvent{{EventID: 1, Origin: "upstream_oauth", FailedTokenRevision: snapshot.Profile.TokenRevision, IdentityEpoch: binding.IdentityEpoch, ProfileRef: binding.ProfileRef, BindingRef: binding.BindingRef, BackendID: binding.BackendID, CreatedAt: time.Now().UTC()}}, NextCursor: 1, OldestEventID: 1})
		case "/_control/v1/credentials":
			state := coreWorkerState(binding, snapshot, true)
			state["last_event_id"] = 1
			_ = json.NewEncoder(w).Encode(state)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	gateway, account, bound, grant, upstream := coreWorkerFixture(server.URL)
	binding, snapshot = bound, grant
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 1
	defer transport.CloseIdleConnections()
	upstream.client = &http.Client{Transport: transport, Timeout: 300 * time.Millisecond}
	store := &coreWorkerRecoveryStore{}
	protocol := &coreWorkerRecoveryProtocol{}
	core, err := NewCoreOAuthService(store, &plainEncryptor{}, map[string]CoreOAuthProtocol{CoreOAuthProviderExcel: protocol}, nil)
	require.NoError(t, err)
	sealed, err := core.seal(&coreOAuthRecord{Contract: CoreOAuthContract, ProfileRef: snapshot.Profile.ProfileRef, Version: 1, Provider: CoreOAuthProviderExcel, IdentityEpoch: binding.IdentityEpoch, TokenRevision: snapshot.Profile.TokenRevision, Grant: &coreOAuthStoredGrant{AccessToken: snapshot.AccessToken, RefreshToken: "synthetic-refresh", ClientID: snapshot.Profile.ClientID, Subject: binding.Subject, WorkspaceID: binding.WorkspaceID, ExpiresAt: *snapshot.Profile.ExpiresAt}})
	require.NoError(t, err)
	store.row = *sealed
	gateway.coreOAuth = core
	ctx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(t.Context()), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", nil)
	require.NoError(t, err)
	response, err := upstream.Do(request, "", account.ID, account.Concurrency)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	originalBody := &coreWorkerRecoveryBody{ReadCloser: response.Body}
	inferenceBody.Store(originalBody)
	response.Body = originalBody
	gateway.recoverCoreOAuthRuntimeUnauthorized(request, account, binding, response)
	require.Equal(t, 1, protocol.refreshes, "worker event must refresh while the original 401 response is still owned by the caller")
	profile, err := core.GetProfile(ctx, snapshot.Profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, snapshot.Profile.TokenRevision+1, profile.TokenRevision)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	require.Equal(t, "expired", response.Header.Get("X-Synthetic-Worker-Error"))
	gotBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(gotBody), "normal error mapping must retain the original 401 body")
	require.EqualValues(t, 1, inferenceCalls.Load(), "recovery must never replay inference")
}

func TestCoreOAuthWorkerUnauthorizedRetainsBoundedPartialErrorBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   string
	}{
		{"partial read error", io.MultiReader(strings.NewReader("partial error"), iotest.ErrReader(errors.New("synthetic read error"))), "partial error"},
		{"body limit", strings.NewReader(strings.Repeat("x", int(openAIUpstreamErrorBodyReadLimit)+32)), strings.Repeat("x", int(openAIUpstreamErrorBodyReadLimit))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &coreWorkerRecoveryBody{ReadCloser: io.NopCloser(tc.reader)}
			response := &http.Response{StatusCode: http.StatusUnauthorized, Body: original}
			gateway := &OpenAIGatewayService{coreOAuth: &CoreOAuthService{}}
			request := httptest.NewRequest(http.MethodPost, "http://synthetic.invalid/v1/responses", nil)
			gateway.recoverCoreOAuthRuntimeUnauthorized(request, &Account{ID: 7}, ManagedDeploymentBinding{Provider: CoreOAuthProviderExcel}, response)
			require.True(t, original.closed.Load())
			got, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
			require.NoError(t, response.Body.Close())
		})
	}
}
