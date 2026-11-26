package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestTiboProbeRequiresExplicitEnablement(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIGatewayService{}
	require.False(t, svc.openAITiboRouteApplies(account))
	svc.cfg = &config.Config{}
	require.False(t, svc.openAITiboRouteApplies(account))
	on := true
	svc.cfg.Gateway.OpenAITiboRoute.Enabled = &on
	require.True(t, svc.openAITiboRouteApplies(account))
	svc.DisableOpenAITiboRouteForTest()
	require.False(t, svc.openAITiboRouteApplies(account))
}

func TestTiboProbeEligibilityExcludesIndependentBackends(t *testing.T) {
	for _, tt := range []struct {
		name        string
		kind        string
		platform    string
		extra       map[string]any
		credentials map[string]any
		want        bool
	}{
		{name: "OAuth", kind: AccountTypeOAuth, want: true},
		{name: "setup token", kind: AccountTypeSetupToken, want: true},
		{name: "API key", kind: AccountTypeAPIKey, want: true},
		{name: "CPR", kind: "cpr"},
		{name: "other platform", kind: AccountTypeOAuth, platform: PlatformAnthropic},
		{name: "synthetic UI dataset", kind: AccountTypeOAuth, extra: map[string]any{"synthetic_ui_test": true}},
		{name: "BPS declaration", kind: AccountTypeAPIKey, credentials: map[string]any{"bps_files_contract": "bps-files/1"}},
		{name: "partial BPS declaration", kind: AccountTypeAPIKey, credentials: map[string]any{"bps_files_control_url": "https://bps.example.invalid"}},
		{name: "BPS capability", kind: AccountTypeAPIKey, credentials: map[string]any{openAIEndpointCapabilitiesCredentialKey: []string{"responses", "bps_files"}}},
		{name: "Agent Identity", kind: AccountTypeOAuth, credentials: map[string]any{openAIAuthModeCredentialKey: OpenAIAuthModeAgentIdentity}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			platform := tt.platform
			if platform == "" {
				platform = PlatformOpenAI
			}
			account := &Account{Platform: platform, Type: tt.kind, Extra: tt.extra, Credentials: tt.credentials}
			require.Equal(t, tt.want, IsOpenAITiboAccount(account))
		})
	}
	require.False(t, IsOpenAITiboAccount(nil))
}

func TestTiboHTTPObservationRequiresSuccessfulUnambiguousCompletion(t *testing.T) {
	completion := string(cookieWSCompletion(openAITiboProbeModel, "True"))
	for _, tt := range []struct {
		name, body string
		verdict    openAITiboVerdict
	}{
		{"True", "data: " + completion + "\n\n", openAITiboHealthy},
		{"False", "data: " + string(cookieWSCompletion(openAITiboProbeModel, "False")) + "\n\n", openAITiboDegraded},
		{"different model", "data: " + string(cookieWSCompletion("gpt-6-luna", "True")) + "\n\n", openAITiboDegraded},
		{"empty", "", openAITiboUnknown},
		{"only done marker", "data: [DONE]\n\n", openAITiboUnknown},
		{"failed terminal", "data: {\"type\":\"response.failed\"}\n\n", openAITiboUnknown},
		{"failed before completion", "data: {\"type\":\"response.failed\"}\n\ndata: " + completion + "\n\n", openAITiboUnknown},
		{"error after completion", "data: " + completion + "\n\ndata: {\"type\":\"error\"}\n\n", openAITiboUnknown},
		{"duplicate completion", "data: " + completion + "\n\ndata: " + completion + "\n\n", openAITiboUnknown},
		{"incomplete status", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\",\"model\":\"gpt-6-astra\"}}\n\n", openAITiboUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := openAITiboSampleFromObservation(http.StatusOK, observeOpenAITiboHTTPProbe([]byte(tt.body)))
			require.Equal(t, tt.verdict, got.verdict)
		})
	}
}

func TestTiboAPIProbeUsesSelectedCredentialsAndBoundedFailure(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			response := cookieWSHTTPResponse("True")
			response.StatusCode = status
			tc := newTiboRouteCase(t, false, false, response)
			tc.account.Type = AccountTypeAPIKey
			tc.account.Credentials = map[string]any{"api_key": "selected-key", "base_url": "https://selected.example.invalid/v1"}
			got := tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account)
			require.Len(t, tc.upstream.requests, 1)
			req := tc.upstream.requests[0]
			require.Equal(t, "https://selected.example.invalid/v1/responses", req.URL.String())
			require.Equal(t, "Bearer selected-key", req.Header.Get("Authorization"))
			require.Contains(t, string(tc.upstream.bodies[0]), openAITiboProbePrompt)
			if status == http.StatusOK {
				require.Equal(t, openAITiboHealthy, got.verdict)
			} else {
				require.Equal(t, openAITiboUnknown, got.verdict)
			}
		})
	}
}

func TestTiboProbeBodyLimitRejectsSuccessfulPrefix(t *testing.T) {
	body := "data: " + string(cookieWSCompletion(openAITiboProbeModel, "True")) + "\n\n"
	body += strings.Repeat(" ", openAITiboProbeBodyLimit)
	require.Equal(t, openAITiboUnknown, openAITiboSampleFromObservation(http.StatusOK, observeOpenAITiboHTTPProbe([]byte(body))).verdict)
}

// The real repository verifies this identity again under its account row lock.
// This fixture observes only the public persistence boundary after actual probing.
type tiboProbeIdentityRepository struct {
	*tiboAccountRepo
	sample TiboAutoGroupSample
}

func (r *tiboProbeIdentityRepository) PersistTiboProbeResult(ctx context.Context, accountID int64, updates map[string]any, sample TiboAutoGroupSample) error {
	r.sample = sample
	return r.UpdateExtra(ctx, accountID, updates)
}

func TestTiboRequestSnapshotUsesCurrentCredentialIdentity(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	oldIdentity := OpenAITiboProbeIdentity(tc.account)
	repo := &tiboProbeIdentityRepository{tiboAccountRepo: checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo)}
	tc.svc.accountRepo = repo
	repo.setAccount(func(a *Account) { a.Credentials["access_token"] = "fresh-current-token" })
	current, err := repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	currentIdentity := OpenAITiboProbeIdentity(current)
	require.NotEqual(t, oldIdentity, currentIdentity)

	require.Equal(t, openAITiboUnknown, tc.svc.openAITiboRequestVerdicts(context.Background(), tc.account), "old request snapshot must not inherit the freshly probed binding")
	tc.svc.openaiTiboProbeWG.Wait()
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
	require.Len(t, tc.upstream.requests, 1)
	require.Equal(t, "Bearer fresh-current-token", tc.upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, currentIdentity, repo.sample.ProbeIdentity)
	require.Equal(t, "healthy", repo.sample.Verdict)
	require.False(t, repo.sample.StartedAt.IsZero())
	require.False(t, repo.sample.CompletedAt.Before(repo.sample.StartedAt))
}

func TestTiboRequestSnapshotCannotBypassCurrentProxyBinding(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"))
	repo := &tiboProbeIdentityRepository{tiboAccountRepo: checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo)}
	tc.svc.accountRepo = repo
	proxyID := int64(771)
	repo.setAccount(func(a *Account) { a.ProxyID, a.Proxy = &proxyID, nil })

	require.Equal(t, openAITiboUnknown, tc.svc.openAITiboRequestVerdicts(context.Background(), tc.account))
	tc.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, tc.upstream.requests, "stale no-proxy snapshot cannot fall back to direct transport")
	require.Empty(t, repo.sample.ProbeIdentity, "no definitive sample reaches automation")
}

func TestTiboSyntheticProbeQualityIsolationMarkerIsPrivateAndAccountBound(t *testing.T) {
	account := &Account{ID: 77, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	ctx := withOpenAITiboSyntheticProbe(context.Background(), account.ID)
	require.True(t, isOpenAITiboSyntheticProbe(ctx, account))
	require.False(t, isOpenAITiboSyntheticProbe(ctx, &Account{ID: 78, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}))
	require.False(t, isOpenAITiboSyntheticProbe(context.Background(), account))
	require.False(t, isOpenAITiboSyntheticProbe(context.WithValue(context.Background(), struct{ name string }{"tibo_probe"}, true), account))
	require.False(t, isOpenAITiboSyntheticProbe(context.TODO(), account))
}

// The production sender is exercised through an actual local HTTP server; this
// adapter changes only the client transport implementation at its existing seam.
type tiboLocalHTTPUpstream struct{ client *http.Client }

func (u *tiboLocalHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *tiboLocalHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestTiboHTTPProbeRecoversIsolatedAccountWithoutAllowingBusiness(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read synthetic request: %v", err)
		}
		if r.URL.Path != "/v1/responses" || !strings.Contains(string(raw), openAITiboProbePrompt) {
			t.Errorf("unexpected request at synthetic HTTP boundary")
		}
		if r.Header.Get("Authorization") != "Bearer probe-key" {
			t.Errorf("probe did not use selected credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(cookieWSCompletion(openAITiboProbeModel, "True")) + "\n\n"))
	}))
	defer server.Close()
	on := true
	cfg := &config.Config{}
	cfg.Gateway.OpenAITiboRoute.Enabled = &on
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	account := &Account{ID: 9901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true,
		Concurrency: 1, Credentials: map[string]any{"api_key": "probe-key", "base_url": server.URL},
		Extra: map[string]any{OpenAITiboQualityIsolatedExtraKey: true}}
	repo := &tiboAccountRepo{account: cloneTiboAccount(account), updates: map[int64]map[string]any{}}
	svc := &OpenAIGatewayService{cfg: cfg, accountRepo: repo, httpUpstream: &tiboLocalHTTPUpstream{client: server.Client()}}
	got := svc.probeOpenAITiboHTTP(context.Background(), account)
	require.Equal(t, openAITiboHealthy, got.verdict)
	require.EqualValues(t, 1, requests.Load(), "an isolated account still reaches the real probe HTTP boundary")

	// Neither the same prompt nor a client-controlled header grants the private
	// provenance required at the final shared send boundary.
	body, err := json.Marshal(openAITiboProbePayload())
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer probe-key")
	req.Header.Set("X-Tibo-Probe", "true")
	resp, err := svc.doOpenAIUpstream(req, "", account)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, GatewayFailureReason("openai_tibo_quality_isolated"), failover.Reason)
	require.EqualValues(t, 1, requests.Load(), "unmarked traffic cannot reach upstream while isolated")

	result, err := svc.Forward(context.Background(), nil, account, body)
	require.Nil(t, result)
	require.ErrorAs(t, err, &failover)
	require.EqualValues(t, 1, requests.Load(), "ordinary Forward remains isolated even with the exact probe payload")
}

type tiboRebindAtPersistRepository struct {
	*tiboAccountRepo
	rebind bool
}

func (r *tiboRebindAtPersistRepository) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, _ TiboAutoGroupSample) error {
	if r.rebind {
		r.rebind = false
		r.setAccount(func(a *Account) { a.Credentials["access_token"] = "new-identity-token" })
		return ErrOpenAITiboProbeIdentityChanged
	}
	return r.UpdateExtra(ctx, id, updates)
}

func TestTiboRejectedIdentitySampleNeverPublishesMemory(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := &tiboRebindAtPersistRepository{tiboAccountRepo: checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo), rebind: true}
	tc.svc.accountRepo = repo
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	oldIdentity := OpenAITiboProbeIdentity(tc.account)
	done := make(chan struct{})
	state.identity = oldIdentity
	state.http = openAITiboRouteState{inflight: true, done: done, probes: []time.Time{time.Now()}}
	sample := openAITiboProbeSample{verdict: openAITiboDegraded, probeIdentity: oldIdentity, startedAt: time.Now()}
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, sample)
	select {
	case <-done:
	default:
		t.Fatal("identity rejection left the completed probe waiting")
	}
	require.Empty(t, state.http.verdict)
	require.Empty(t, state.http.recent)
	require.True(t, state.http.persistedAt.IsZero())
	require.True(t, state.http.persistedSampleAt.IsZero())
	require.Len(t, state.http.probes, 1, "sent traffic is not refunded after a credential change")
	current, err := repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	require.Equal(t, OpenAITiboProbeIdentity(current), state.identity)
	require.NotEqual(t, oldIdentity, state.identity)

	fresh := openAITiboProbeSample{verdict: openAITiboHealthy, probeIdentity: state.identity, startedAt: time.Now()}
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, current, fresh)
	require.Equal(t, openAITiboHealthy, state.http.verdict)
	require.Equal(t, []openAITiboVerdict{openAITiboHealthy}, recentVerdicts(state.http.recent))
	require.False(t, state.http.persistedAt.IsZero())
}

func TestTiboCredentialChangeCannotSeedPriorHistory(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			tc := newTiboRouteCase(t, false, false)
			repo := checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo)
			oldIdentity := OpenAITiboProbeIdentity(tc.account)
			at := time.Now().Add(-time.Minute)
			history := map[string]any{"model": openAITiboProbeModel, "verdict": "degraded", "checked_at": at.Format(time.RFC3339Nano),
				"recent_samples": []map[string]any{{"verdict": "degraded", "at": at.Format(time.RFC3339Nano)}}}
			if !legacy {
				history["probe_identity"] = oldIdentity
			}
			repo.setAccount(func(a *Account) {
				a.Extra[OpenAIAstraPolicyExtraKey] = history
				a.Credentials["access_token"] = "replacement-token"
			})
			tc.svc.enrollOpenAITiboAccounts(context.Background())
			current, err := repo.GetByID(context.Background(), tc.account.ID)
			require.NoError(t, err)
			state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
			require.NotNil(t, state)
			require.Equal(t, OpenAITiboProbeIdentity(current), state.identity)
			require.Empty(t, state.http.verdict)
			require.Empty(t, state.http.recent)
			statuses := tc.svc.openAITiboRouteStatuses(current, time.Now())
			require.Len(t, statuses, 1)
			require.Empty(t, statuses[0].RecentSamples)
			require.Equal(t, "unknown", current.OpenAIOAuthModelPolicy().Verdict)
		})
	}
}

type tiboCanceledObservationRepository struct {
	AccountRepository
	cancel      context.CancelFunc
	hadDeadline bool
}

func (r *tiboCanceledObservationRepository) GetByID(ctx context.Context, _ int64) (*Account, error) {
	_, r.hadDeadline = ctx.Deadline()
	r.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestTiboObservationCanceledReadAlwaysReleasesInflight(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &tiboCanceledObservationRepository{cancel: cancel}
	tc.svc.accountRepo = repo
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	done := make(chan struct{})
	state.http.inflight, state.http.done = true, done
	sample := openAITiboProbeSample{verdict: openAITiboHealthy, probeIdentity: OpenAITiboProbeIdentity(tc.account), startedAt: time.Now()}
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account, sample, parent)
	require.True(t, repo.hadDeadline)
	select {
	case <-done:
	default:
		t.Fatal("canceled identity read left the completed probe waiting")
	}
	require.False(t, state.http.inflight)
	require.Empty(t, state.http.recent)
	require.True(t, state.http.persistedAt.IsZero())
}

func TestTiboStandaloneRecordAndPassiveHistoryRemainIdentityBound(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	tc.svc.accountRepo = nil
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	identity := OpenAITiboProbeIdentity(tc.account)
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account,
		openAITiboProbeSample{verdict: openAITiboHealthy, probeIdentity: identity, startedAt: time.Now()})
	require.Equal(t, openAITiboHealthy, state.http.verdict)
	require.Len(t, state.http.recent, 1)

	// The legacy Cookie-WS route name does not launch an HTTP confirmation;
	// the quality state itself remains the same single HTTP observation store.
	tc.svc.forceOpenAITiboDegraded(tc.account, openAITiboRouteCookieWS)
	require.Equal(t, openAITiboDegraded, state.http.verdict)
	require.Equal(t, identity, state.identity)
	require.Len(t, state.http.recent, 2)

	persisted := newTiboRouteCase(t, false, false)
	persisted.svc.forceOpenAITiboDegraded(persisted.account, openAITiboRouteCookieWS)
	repo := checkedFixtureValue[*tiboAccountRepo](persisted.svc.accountRepo)
	updates, ok := repo.updateFor(persisted.account.ID)
	require.True(t, ok)
	raw := updates[openAITiboVerdictExtraKey(openAITiboRouteCookieWS)]
	require.Equal(t, OpenAITiboProbeIdentity(persisted.account), openAITiboExtraField(raw, "probe_identity"))
}

func TestTiboReadOnlyRoutesIgnoreAnotherCredentialIdentity(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	oldIdentity := OpenAITiboProbeIdentity(tc.account)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	tc.svc.recordOpenAITiboSample(state, openAITiboRouteHTTP, tc.account,
		openAITiboProbeSample{verdict: openAITiboDegraded, probeIdentity: oldIdentity, startedAt: time.Now()})
	require.Equal(t, openAITiboDegraded, state.http.verdict)
	require.Equal(t, 2, tc.svc.openAITiboSchedulerTier(tc.account, openAITiboProbeModel, time.Now()))
	repo := checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo)
	repo.setAccount(func(a *Account) { a.Credentials["access_token"] = "replacement-before-enrollment" })
	current, err := repo.GetByID(context.Background(), tc.account.ID)
	require.NoError(t, err)
	// Make the stale state due to prove the mismatch read does not start a probe.
	state.http.nextProbeAt = time.Now().Add(-time.Minute)
	require.Equal(t, 1, tc.svc.openAITiboSchedulerTier(current, openAITiboProbeModel, time.Now()))
	require.Equal(t, openAITiboUnknown, tc.svc.openAITiboRequestVerdicts(context.Background(), current))
	tc.svc.openaiTiboProbeWG.Wait()
	require.Empty(t, tc.upstream.requests)
	require.Equal(t, oldIdentity, state.identity, "a possibly stale read snapshot cannot reverse-adopt the state binding")
	require.Equal(t, openAITiboDegraded, state.http.verdict, "read-only mismatch handling does not mutate committed quality state")
}

type tiboBlockedPassiveRepository struct {
	*tiboAccountRepo
	entered chan struct{}
	release chan struct{}
}

func (r *tiboBlockedPassiveRepository) PersistTiboObservation(ctx context.Context, id int64, updates map[string]any, _ string) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.UpdateExtra(ctx, id, updates)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *tiboBlockedPassiveRepository) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, _ TiboAutoGroupSample) error {
	return r.UpdateExtra(ctx, id, updates)
}

func TestTiboPassivePersistencePreservesConcurrentProbeBudgetAndRefund(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	repo := &tiboBlockedPassiveRepository{tiboAccountRepo: checkedFixtureValue[*tiboAccountRepo](tc.svc.accountRepo), entered: make(chan struct{}), release: make(chan struct{})}
	tc.svc.accountRepo = repo
	probeGate := make(chan struct{})
	tc.svc.httpUpstream = &tiboSafeUpstream{probeGate: probeGate}
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	baselineProbe := time.Now().Add(-time.Minute)
	state.identity = OpenAITiboProbeIdentity(tc.account)
	state.http.probes = []time.Time{baselineProbe}
	probeCtx, cancelProbe := context.WithCancel(context.Background())
	passiveDone := make(chan struct{})
	var releaseOnce sync.Once
	releasePassive := func() { releaseOnce.Do(func() { close(repo.release) }) }
	defer func() {
		releasePassive()
		close(probeGate)
		cancelProbe()
		<-passiveDone
		tc.svc.openaiTiboProbeWG.Wait()
	}()
	go func() {
		tc.svc.forceOpenAITiboDegraded(tc.account, openAITiboRouteHTTP)
		close(passiveDone)
	}()
	select {
	case <-repo.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("passive observation never reached persistence")
	}
	// The passive copy predates this reservation; publishing it must retain
	// both the old spent probe and the new, independently owned reservation.
	state.mu.Lock()
	started := tc.svc.startOpenAITiboProbeCtxLocked(probeCtx, state, openAITiboRouteHTTP, tc.account, true, tc.svc.openAITiboRouteConfig(), time.Now())
	reserved := append([]time.Time(nil), state.http.probes...)
	state.mu.Unlock()
	require.NotNil(t, started)
	require.Len(t, reserved, 2)
	releasePassive()
	select {
	case <-passiveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("passive observation did not complete")
	}
	state.mu.Lock()
	published := append([]time.Time(nil), state.http.probes...)
	inflight := state.http.inflight
	state.mu.Unlock()
	require.True(t, inflight)
	require.Equal(t, reserved, published, "passive publication cannot lose a concurrent probe reservation")

	// Cancellation before an actual completed sample refunds exactly the new
	// reservation. Losing it during publication would wrongly refund baseline.
	cancelProbe()
	tc.svc.openaiTiboProbeWG.Wait()
	select {
	case <-started:
	default:
		t.Fatal("aborted probe did not release its single-flight waiter")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	require.False(t, state.http.inflight)
	require.Equal(t, []time.Time{baselineProbe}, state.http.probes)
}
