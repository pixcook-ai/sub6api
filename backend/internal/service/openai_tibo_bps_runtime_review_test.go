package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type tiboBPSReviewRoundTripper struct {
	target *url.URL
	next   http.RoundTripper
	t      *testing.T
}

func (r tiboBPSReviewRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Helper()
	require.Equal(r.t, OpenAIBPSResponsesURL, req.URL.String(), "the real request must target the fixed BPS URL")
	copied := req.Clone(req.Context())
	local := *req.URL
	local.Scheme, local.Host = r.target.Scheme, r.target.Host
	copied.URL, copied.Host = &local, r.target.Host
	return r.next.RoundTrip(copied)
}

func TestTiboBPSRuntimeRecoveryUsesRealHTTPAndPersistsHistory(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.account.Platform = PlatformOpenAIBPS
	tc.account.Schedulable = false
	tc.account.Extra[OpenAITiboQualityIsolatedExtraKey] = true
	repo, ok := tc.svc.accountRepo.(*tiboAccountRepo)
	require.True(t, ok)
	repo.setAccount(func(a *Account) { *a = *cloneTiboAccount(tc.account) })
	cache := &bpsMemoryStore{data: map[string][]byte{"business-session": []byte("unchanged")}}
	tc.svc.cache = cache
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read BPS probe: %v", err)
		}
		if req.URL.Path != "/basispoints/api/responses" || req.Header.Get("Authorization") != "Bearer test-token" || req.Header.Get("chatgpt-account-id") != "test-chatgpt" {
			t.Error("probe did not preserve the BPS endpoint and account credentials")
		}
		if req.Header.Get("originator") != "" {
			t.Error("BPS probe contains Codex identity")
		}
		if gjson.GetBytes(body, "reasoning_effort").String() != "xhigh" || gjson.GetBytes(body, "model").String() != openAITiboProbeModel {
			t.Error("BPS probe changed the fixed Tibo model or effort")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, bpsEvent("response.completed", map[string]any{"response": bpsResponse(bpsText("True"))}))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := server.Client().Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	tc.svc.httpUpstream = &tiboLocalHTTPUpstream{client: &http.Client{Transport: tiboBPSReviewRoundTripper{target: target, next: transport, t: t}}}
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	tc.svc.runOpenAITiboProbe(context.Background(), state, openAITiboRouteHTTP, nil, false)
	require.EqualValues(t, 1, requests.Load(), "paused, quality-isolated BPS OAuth still receives a private recovery probe")
	stored, err := repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	require.False(t, stored.Schedulable, "probing must preserve the administrator scheduling switch")
	statuses := tc.svc.openAITiboRouteStatuses(stored, time.Now())
	require.Len(t, statuses, 1)
	require.Len(t, statuses[0].RecentSamples, 1)
	require.Equal(t, string(openAITiboHealthy), statuses[0].RecentSamples[0].Verdict)
	require.Equal(t, map[string][]byte{"business-session": []byte("unchanged")}, cache.data, "a probe must not read or write business conversation state")
	result, err := tc.svc.Forward(context.Background(), nil, stored, []byte(`{"model":"gpt-6-astra","input":"same question"}`))
	require.Nil(t, result)
	var isolated *UpstreamFailoverError
	require.ErrorAs(t, err, &isolated)
	require.EqualValues(t, 1, requests.Load(), "ordinary traffic remains blocked")
}

func TestTiboBPSRuntimeProbeScopeDoesNotInheritBusinessBinding(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.account.Platform = PlatformOpenAIBPS
	businessScope := "business-scope-must-never-be-probed"
	ctx := context.WithValue(context.Background(), bpsRoutingContextKey{}, bpsRoutingContext{Scope: businessScope, Binding: &bpsSessionBinding{tc.account.ID, "test-chatgpt"}})
	first, err := tc.svc.buildOpenAITiboBPSProbeRequest(ctx, tc.account)
	require.NoError(t, err)
	second, err := tc.svc.buildOpenAITiboBPSProbeRequest(ctx, tc.account)
	require.NoError(t, err)
	firstBody, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	secondBody, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	firstTask := gjson.GetBytes(firstBody, "metadata.task_id").String()
	require.NotEqual(t, uuid.NewSHA1(uuid.NameSpaceURL, []byte(businessScope)).String(), firstTask)
	require.NotEqual(t, firstTask, gjson.GetBytes(secondBody, "metadata.task_id").String(), "each probe owns an independent upstream task")
}

type tiboBPSReviewEnrollmentRepo struct {
	AccountRepository
	platforms map[string][]Account
	fail      string
	listed    []string
}

func (r *tiboBPSReviewEnrollmentRepo) ListByPlatform(_ context.Context, platform string) ([]Account, error) {
	r.listed = append(r.listed, platform)
	if r.fail == platform {
		return nil, errors.New("temporary platform read failure")
	}
	return r.platforms[platform], nil
}

func TestTiboBPSRuntimeEnrollmentFailureDoesNotPruneEitherPlatform(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	bps := *cloneTiboAccount(tc.account)
	bps.ID++
	bps.Platform = PlatformOpenAIBPS
	repo := &tiboBPSReviewEnrollmentRepo{platforms: map[string][]Account{PlatformOpenAI: {*tc.account}, PlatformOpenAIBPS: {bps}}}
	tc.svc.accountRepo = repo
	tc.svc.enrollOpenAITiboAccounts(context.Background())
	require.Equal(t, []string{PlatformOpenAI, PlatformOpenAIBPS}, repo.listed)
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID))
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(bps.ID))
	// A partial list must not make either platform look deleted.
	repo.platforms[PlatformOpenAI] = nil
	repo.fail = PlatformOpenAIBPS
	tc.svc.openaiTiboEnrolledAt.Store(0)
	tc.svc.enrollOpenAITiboAccounts(context.Background())
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID))
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(bps.ID))
	// After a complete read, genuinely removed accounts can be pruned.
	repo.fail = ""
	tc.svc.openaiTiboEnrolledAt.Store(0)
	tc.svc.enrollOpenAITiboAccounts(context.Background())
	require.Nil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID))
	require.NotNil(t, tc.svc.loadOpenAITiboAccountState(bps.ID))
}

func TestTiboBPSRuntimeDisabledAccountIsRecheckedBeforeSend(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.account.Platform = PlatformOpenAIBPS
	repo, ok := tc.svc.accountRepo.(*tiboAccountRepo)
	require.True(t, ok)
	repo.setAccount(func(a *Account) { a.Platform, a.Status = PlatformOpenAIBPS, StatusDisabled })
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	tc.svc.runOpenAITiboProbe(context.Background(), state, openAITiboRouteHTTP, nil, false)
	require.Empty(t, tc.upstream.requests)
	_, wrote := repo.updateFor(tc.account.ID)
	require.False(t, wrote, "disabled accounts produce no quality samples")
}
