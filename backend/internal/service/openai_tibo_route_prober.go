package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Background Tibo probing hangs off the harvester tick. Eligible ChatGPT
// OAuth accounts are enrolled and probed when due; each (account, route)
// probes at most once at a time, within max_probes_per_hour, and the process
// runs at most probe_concurrency probes.
const (
	openAITiboVerdictExtraKeyPrefix = "codex_tibo_verdict:"
	// A probe that could not start (semaphore wait timeout, account no longer
	// eligible) retries after this delay without counting as a sample.
	openAITiboProbeAbortRetry = time.Minute
	openAITiboPinPruneEvery   = time.Minute
)

func (s *OpenAIGatewayService) openAITiboProbeSem() chan struct{} {
	s.openaiTiboProbeSemOnce.Do(func() {
		s.openaiTiboProbeSem = make(chan struct{}, s.openAITiboRouteConfig().probeConcurrency)
	})
	return s.openaiTiboProbeSem
}

// startOpenAITiboProbeLocked starts one probe for route unless one is already
// running. It returns the channel closed when the probe finishes, or nil when
// no probe started (budget exhausted, or a non-blocking start found the
// process-wide semaphore full). account may be nil for background probes; the
// probe then reloads the authoritative account. Caller holds state.mu.
func (s *OpenAIGatewayService) startOpenAITiboProbeLocked(state *openAITiboAccountState, route openAITiboRoute, account *Account, blocking bool, cfg openAITiboSettings, now time.Time) <-chan struct{} {
	return s.startOpenAITiboProbeCtxLocked(context.Background(), state, route, account, blocking, cfg, now)
}

func (s *OpenAIGatewayService) startOpenAITiboProbeCtxLocked(parent context.Context, state *openAITiboAccountState, route openAITiboRoute, account *Account, blocking bool, cfg openAITiboSettings, now time.Time) <-chan struct{} {
	// Serialize Add with Stop's transition to waiting. All callers already hold
	// the account state lock; Stop never takes that lock while holding this one.
	s.openaiTiboLifecycleMu.Lock()
	defer s.openaiTiboLifecycleMu.Unlock()
	if s.openaiTiboStopped {
		return nil
	}
	if s.openaiTiboContext != nil {
		parent = s.openaiTiboContext
	}
	r := state.route(route)
	if r.inflight {
		return r.done
	}
	sem := s.openAITiboProbeSem()
	acquired := false
	if !blocking {
		select {
		case sem <- struct{}{}:
			acquired = true
		default:
			// The next tick retries; never block a request or the tick.
			return nil
		}
	}
	if !r.takeBudget(now, cfg) {
		if acquired {
			<-sem
		}
		return nil
	}
	var snapshot *Account
	if account != nil {
		copied := *account
		copied.Extra = maps.Clone(account.Extra)
		copied.Credentials = maps.Clone(account.Credentials)
		snapshot = &copied
	}
	done := make(chan struct{})
	r.inflight, r.done = true, done
	s.openaiTiboProbeWG.Add(1)
	go func() {
		defer s.openaiTiboProbeWG.Done()
		s.runOpenAITiboProbe(parent, state, route, snapshot, acquired)
	}()
	return done
}

// runOpenAITiboProbe runs detached from any request (a cancelled request must
// not discard the verdict); parent is the harvester context for tick probes
// and context.Background otherwise.
func (s *OpenAIGatewayService) runOpenAITiboProbe(parent context.Context, state *openAITiboAccountState, route openAITiboRoute, account *Account, acquired bool) {
	ctx, cancel := context.WithTimeout(parent, openAITiboProbeTimeout)
	defer cancel()
	if parent.Err() != nil {
		if acquired {
			<-s.openAITiboProbeSem()
		}
		s.abortOpenAITiboProbe(state, route, nil)
		return
	}
	sem := s.openAITiboProbeSem()
	if !acquired {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			s.abortOpenAITiboProbe(state, route, nil)
			return
		}
	}
	defer func() { <-sem }()
	requestTriggered := account != nil
	if s.accountRepo != nil || account == nil {
		// Request-triggered snapshots are subject to the same identity fence as
		// background probes. A token/proxy edit after selection must be sampled
		// using the actual current binding, not a stale scheduler copy.
		latest, err := s.latestOpenAITiboAccount(ctx, state.accountID)
		if err != nil {
			s.abortOpenAITiboProbe(state, route, nil)
			return
		}
		account = latest
	}
	if !requestTriggered {
		if hold := s.openAITiboHoldFor(s.withOpenAIQuotaAutoPauseContext(ctx), account, time.Now()); hold.parked || !hold.until.IsZero() {
			// Manual OAuth pauses may still probe under current quota policy.
			s.abortOpenAITiboProbe(state, route, &hold)
			return
		}
	} else if skip := openAITiboAccountSkipReason(account, time.Now()); skip != "" {
		s.abortOpenAITiboProbe(state, route, nil)
		return
	}
	probeIdentity := OpenAITiboProbeIdentity(account)
	state.mu.Lock()
	adoptOpenAITiboIdentityLocked(state, probeIdentity)
	state.mu.Unlock()
	startedAt := time.Now()
	sample := s.probeOpenAITiboHTTP(ctx, account)
	sample.startedAt = startedAt
	sample.probeIdentity = probeIdentity
	if parent.Err() != nil {
		// Harvester shutdown interrupted the probe; this is not a sample.
		s.abortOpenAITiboProbe(state, route, nil)
		return
	}
	s.recordOpenAITiboSample(state, route, account, sample, parent)
}

// openAITiboHoldFor also checks the gateway switch and the model's runtime
// cooldown for paused OAuth accounts. Callers attach the current quota settings.
func (s *OpenAIGatewayService) openAITiboHoldFor(ctx context.Context, account *Account, now time.Time) openAITiboHold {
	if !s.openAITiboRouteApplies(account) {
		return openAITiboHold{parked: true}
	}
	hold := openAITiboAccountHold(ctx, account, now)
	if !hold.parked && hold.until.IsZero() && !account.Schedulable && account.IsOpenAIOAuth() {
		// Probes normalize the fixed model without business model mappings.
		model := normalizeOpenAIModelForUpstream(account, openAITiboProbeModel)
		hold.parked = s.getOpenAIAccountModelTransientState().isBlocked(account.ID, openAIAccountModelTransientModel(model), now)
	}
	return hold
}

// abortOpenAITiboProbe ends a probe that produced no sample and refunds its
// budget. With an authoritative hold, the route waits for the hold (re-synced
// every enrollment) instead of retrying every minute; without one (read
// error, stale snapshot, shutdown) it retries after openAITiboProbeAbortRetry.
func (s *OpenAIGatewayService) abortOpenAITiboProbe(state *openAITiboAccountState, route openAITiboRoute, hold *openAITiboHold) {
	state.mu.Lock()
	r := state.route(route)
	if n := len(r.probes); n > 0 {
		// Only one probe per route runs, so the last start is this one.
		r.probes = r.probes[:n-1]
	}
	done := r.done
	r.inflight, r.done = false, nil
	if hold != nil {
		r.applyHold(*hold)
	} else if retry := time.Now().Add(openAITiboProbeAbortRetry); retry.After(r.nextProbeAt) {
		r.nextProbeAt = retry
	}
	state.mu.Unlock()
	if done != nil {
		close(done)
	}
}

// discardOpenAITiboSample releases a completed, already spent probe without
// exposing its rejected observation. Unlike abort-before-send, it never refunds
// the hourly budget. A new credential binding starts with empty quality state.
func (s *OpenAIGatewayService) discardOpenAITiboSample(state *openAITiboAccountState, route openAITiboRoute, current *Account, rejectedIdentity string) {
	state.mu.Lock()
	if current != nil && (state.identity == "" || state.identity == rejectedIdentity) {
		adoptOpenAITiboIdentityLocked(state, OpenAITiboProbeIdentity(current))
	}
	r := state.route(route)
	done := r.done
	r.inflight, r.done = false, nil
	r.nextProbeAt = time.Now().Add(openAITiboProbeAbortRetry)
	state.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func cloneOpenAITiboRouteState(r *openAITiboRouteState) openAITiboRouteState {
	next := *r
	next.pending = append([]openAITiboVerdict(nil), r.pending...)
	next.recent = append([]openAITiboRecentSample(nil), r.recent...)
	next.probes = append([]time.Time(nil), r.probes...)
	next.flips = append([]time.Time(nil), r.flips...)
	return next
}

// recordOpenAITiboSample stages an observation privately. A rejected repository
// identity fence cannot leak the old credential's verdict or history into memory.
// Readers keep their previous committed state while the bounded write completes.
func (s *OpenAIGatewayService) recordOpenAITiboSample(state *openAITiboAccountState, route openAITiboRoute, account *Account, sample openAITiboProbeSample, parents ...context.Context) {
	state.observationMu.Lock()
	defer state.observationMu.Unlock()
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	recordCtx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	identity := sample.probeIdentity
	if identity == "" && !sample.verdict.definitive() {
		// Internal non-definitive fixtures never invoke automatic actions.
		identity = OpenAITiboProbeIdentity(account)
	}
	if recordCtx.Err() != nil {
		s.discardOpenAITiboSample(state, route, nil, identity)
		return
	}
	current := account
	if s.accountRepo != nil {
		latest, err := s.latestOpenAITiboAccount(recordCtx, state.accountID)
		if err != nil {
			s.discardOpenAITiboSample(state, route, nil, identity)
			return
		}
		current = latest
	}
	if identity == "" || identity == "invalid" || identity != OpenAITiboProbeIdentity(current) {
		s.discardOpenAITiboSample(state, route, current, identity)
		return
	}
	cfg := s.openAITiboRouteConfig()
	now := time.Now()
	state.mu.Lock()
	adoptOpenAITiboIdentityLocked(state, identity)
	r := state.route(route)
	next := cloneOpenAITiboRouteState(r)
	before := next.effective(now, cfg)
	transition := next.observe(sample, now, cfg)
	next.scheduleNext(route, now, cfg)
	if transition.flipped && transition.from != "" {
		next.flips = append(pruneOpenAITiboWindow(next.flips, now), now)
	}
	after := next.effective(now, cfg)
	persist := openAITiboShouldPersist(&next, transition, now, cfg) || (sample.verdict.definitive() && len(next.recent) > 0)
	record := openAITiboVerdictRecordFor(&next)
	record.ProbeIdentity = identity
	samples := append([]openAITiboRecentSample(nil), next.recent...)
	state.mu.Unlock()
	if persist {
		var fresh []TiboAutoGroupSample
		if route == openAITiboRouteHTTP && sample.verdict.definitive() && isOpenAITiboAccount(current) {
			startedAt := sample.startedAt
			if startedAt.IsZero() {
				startedAt = now
			}
			fresh = []TiboAutoGroupSample{{Verdict: string(sample.verdict), ProbeIdentity: identity, StartedAt: startedAt, CompletedAt: now}}
		}
		if err := s.persistOpenAITiboVerdict(state.accountID, route, record, samples, fresh...); err != nil {
			current = nil
			if errors.Is(err, ErrOpenAITiboProbeIdentityChanged) {
				if latest, readErr := s.latestOpenAITiboAccount(recordCtx, state.accountID); readErr == nil {
					current = latest
				} else {
					state.mu.Lock()
					if state.identity == identity {
						old := state.http
						state.http = openAITiboRouteState{inflight: old.inflight, done: old.done, probes: old.probes}
						state.identity = ""
					}
					state.mu.Unlock()
				}
			}
			s.discardOpenAITiboSample(state, route, current, identity)
			return
		}
		for _, sample := range samples {
			if sample.At.After(next.persistedSampleAt) {
				next.persistedSampleAt = sample.At
			}
		}
		if record.CheckedAt.After(next.persistedAt) {
			next.persistedAt = record.CheckedAt
		}
	}
	state.mu.Lock()
	r = state.route(route)
	if state.identity != identity {
		state.mu.Unlock()
		s.discardOpenAITiboSample(state, route, nil, identity)
		return
	}
	// Enrollment can update an authoritative availability hold during the write.
	next.holdUntil, next.parked = r.holdUntil, r.parked
	done := r.done
	next.inflight, next.done = false, nil
	*r = next
	flipsHour, probesHour := countOpenAITiboWindow(r.flips, now), countOpenAITiboWindow(r.probes, now)
	state.mu.Unlock()
	if done != nil {
		defer close(done)
	}
	s.openaiTiboStats.noteProbe(transition.flipped && transition.from != "")
	fields := []zap.Field{
		zap.Int64("account_id", state.accountID), zap.String("route", string(route)),
		zap.String("sample", string(sample.verdict)), zap.Int("http", sample.status), zap.String("answer_class", sample.answerClass),
		zap.String("effective_before", string(before)), zap.String("effective_after", string(after)),
		zap.Int("flips_hour", flipsHour), zap.Int("probes_hour", probesHour),
	}
	switch {
	case transition.flipped:
		votes := make([]string, 0, len(transition.votes))
		for _, vote := range transition.votes {
			votes = append(votes, string(vote))
		}
		logger.L().Info("openai_tibo verdict flip", append(fields,
			zap.String("from", string(transition.from)), zap.String("to", string(transition.to)), zap.Strings("votes", votes))...)
	case before != after:
		logger.L().Info("openai_tibo effective verdict", fields...)
	}
}

// openAITiboShouldPersist writes confirmed flips, and refreshes an unchanged
// verdict once it has aged half of max_stale, so a restart keeps it usable.
func openAITiboShouldPersist(r *openAITiboRouteState, transition openAITiboTransition, now time.Time, cfg openAITiboSettings) bool {
	for _, sample := range r.recent {
		if sample.Verdict.definitive() && sample.At.After(r.persistedSampleAt) {
			return true
		}
	}
	if !r.verdict.definitive() {
		return false
	}
	if transition.flipped {
		return true
	}
	return r.checkedAt.After(r.persistedAt) && now.Sub(r.persistedAt) >= cfg.maxStale/2
}

type openAITiboVerdictRecord struct {
	ProbeIdentity string            `json:"probe_identity,omitempty"`
	Verdict       openAITiboVerdict `json:"verdict"`
	Model         string            `json:"model"`
	CheckedAt     time.Time         `json:"checked_at"`
	FlippedAt     time.Time         `json:"flipped_at"`
}

func openAITiboVerdictRecordFor(r *openAITiboRouteState) openAITiboVerdictRecord {
	return openAITiboVerdictRecord{Verdict: r.verdict, Model: openAITiboProbeModel, CheckedAt: r.checkedAt, FlippedAt: r.flippedAt}
}

func openAITiboVerdictExtraKey(route openAITiboRoute) string {
	return openAITiboVerdictExtraKeyPrefix + string(route)
}

func parseOpenAITiboVerdictRecord(raw any) (openAITiboVerdictRecord, bool) {
	var record openAITiboVerdictRecord
	if raw == nil {
		return record, false
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &record) != nil || !record.Verdict.definitive() || record.CheckedAt.IsZero() {
		return openAITiboVerdictRecord{}, false
	}
	return record, true
}

func (s *OpenAIGatewayService) persistOpenAITiboVerdict(accountID int64, route openAITiboRoute, record openAITiboVerdictRecord, samples []openAITiboRecentSample, fresh ...TiboAutoGroupSample) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if record.ProbeIdentity == "" {
		current, err := s.latestOpenAITiboAccount(ctx, accountID)
		if err != nil {
			return err
		}
		record.ProbeIdentity = OpenAITiboProbeIdentity(current)
	}
	value := map[string]any{"model": record.Model, "probe_identity": record.ProbeIdentity, "recent_samples": encodeOpenAITiboRecentSamples(samples)}
	if record.Verdict.definitive() && !record.CheckedAt.IsZero() {
		value["verdict"] = string(record.Verdict)
		value["checked_at"] = record.CheckedAt.UTC().Format(time.RFC3339Nano)
		value["flipped_at"] = record.FlippedAt.UTC().Format(time.RFC3339Nano)
	}
	updates := map[string]any{openAITiboVerdictExtraKey(route): value}
	var err error
	if repo, ok := s.accountRepo.(TiboAutoGroupRepository); ok && len(fresh) == 1 {
		err = repo.PersistTiboProbeResult(ctx, accountID, updates, fresh[0])
	} else if repo, ok := s.accountRepo.(TiboObservationRepository); ok {
		err = repo.PersistTiboObservation(ctx, accountID, updates, record.ProbeIdentity)
	} else {
		current, readErr := s.latestOpenAITiboAccount(ctx, accountID)
		if readErr != nil {
			return readErr
		}
		if record.ProbeIdentity != OpenAITiboProbeIdentity(current) {
			return ErrOpenAITiboProbeIdentityChanged
		}
		err = s.accountRepo.UpdateExtra(ctx, accountID, updates)
	}
	if err != nil {
		logger.L().Warn("openai_tibo verdict persist failed", zap.Int64("account_id", accountID), zap.String("route", string(route)), zap.Error(err))
		return err
	}
	return nil
}

func encodeOpenAITiboRecentSamples(samples []openAITiboRecentSample) []map[string]any {
	out := make([]map[string]any, 0, len(samples))
	for _, sample := range samples {
		item := map[string]any{"verdict": string(sample.Verdict)}
		if !sample.At.IsZero() {
			item["at"] = sample.At.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, item)
	}
	return out
}

func parseOpenAITiboRecentSamples(raw any) []openAITiboRecentSample {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var rows []struct {
		Verdict string    `json:"verdict"`
		At      time.Time `json:"at"`
	}
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	out := make([]openAITiboRecentSample, 0, len(rows))
	for _, row := range rows {
		verdict := openAITiboVerdict(row.Verdict)
		if verdict != openAITiboHealthy && verdict != openAITiboDegraded && verdict != openAITiboUnknown {
			continue
		}
		out = append(out, openAITiboRecentSample{Verdict: verdict, At: row.At})
	}
	if extra := len(out) - openAITiboRecentSampleLimit; extra > 0 {
		out = append([]openAITiboRecentSample(nil), out[extra:]...)
	}
	return out
}

// seedOpenAITiboStateLocked adopts persisted verdicts for routes this process
// has not sampled yet, subject to max_stale. Caller holds state.mu.
func (s *OpenAIGatewayService) seedOpenAITiboStateLocked(state *openAITiboAccountState, extra map[string]any, now time.Time, cfg openAITiboSettings) {
	if len(extra) == 0 {
		return
	}
	route := openAITiboRouteHTTP
	r := state.route(route)
	if !r.cold() || r.inflight {
		return
	}
	raw := extra[openAITiboVerdictExtraKey(route)]
	identity, _ := openAITiboExtraField(raw, "probe_identity").(string)
	if state.identity != "" && identity != state.identity {
		// Legacy/unbound history cannot be attributed to freshly edited
		// credentials, including after a process restart.
		return
	}
	if samples := validOpenAIAstraSamples(raw, now); len(samples) > 0 && len(r.recent) == 0 {
		r.recent = samples
		for _, sample := range samples {
			if sample.At.After(r.persistedSampleAt) {
				r.persistedSampleAt = sample.At
			}
		}
		last := samples[len(samples)-1]
		if r.lastSampleAt.IsZero() {
			r.lastSampleAt = last.At
			r.lastSample = openAITiboProbeSample{verdict: last.Verdict}
		}
	}
	record, ok := parseOpenAITiboVerdictRecord(raw)
	if !ok || record.CheckedAt.After(now) || now.Sub(record.CheckedAt) > cfg.maxStale {
		return
	}
	r.verdict, r.checkedAt, r.flippedAt, r.persistedAt = record.Verdict, record.CheckedAt, record.FlippedAt, record.CheckedAt
	r.nextProbeAt = record.CheckedAt.Add(openAITiboCadenceDelay(cfg))
}

func openAITiboExtraField(raw any, key string) any {
	if typed, ok := raw.(map[string]any); ok {
		return typed[key]
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var typed map[string]any
	if json.Unmarshal(b, &typed) != nil {
		return nil
	}
	return typed[key]
}

const openAITiboEnrollInterval = time.Minute

// enrollOpenAITiboAccounts runs at most once a minute. From the active
// OpenAI account list it:
//   - enrolls eligible Tibo accounts, seeds persisted verdicts and staggers
//     the first nextProbeAt so a restart does not fire every account at once;
//   - re-syncs every tracked account's hold (rate limit, overload, pause)
//     from the database, so a lifted limit resumes probing within a minute;
//   - drops state for accounts no longer listed (deleted, disabled, error,
//     moved off OpenAI) so they stop costing a database read every minute.
//
// Tests that set openaiTiboLoaded skip the listing path and inject state
// themselves.
func (s *OpenAIGatewayService) enrollOpenAITiboAccounts(ctx context.Context) {
	if s == nil || s.accountRepo == nil || s.openaiTiboLoaded.Load() {
		return
	}
	now := time.Now()
	last := s.openaiTiboEnrolledAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < openAITiboEnrollInterval {
		return
	}
	if !s.openaiTiboEnrolledAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	defer func() { _ = recover() }()
	var accounts []Account
	for _, platform := range []string{PlatformOpenAI, PlatformOpenAIBPS} {
		listed, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			// Never prune the other platform on a partial enrollment read.
			s.openaiTiboEnrolledAt.Store(last)
			return
		}
		accounts = append(accounts, listed...)
	}
	cfg := s.openAITiboRouteConfig()
	enrolled, held := 0, 0
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	listed := make(map[int64]struct{}, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if !s.openAITiboRouteApplies(account) {
			continue
		}
		listed[account.ID] = struct{}{}
		hold := s.openAITiboHoldFor(ctx, account, now)
		if hold.parked || !hold.until.IsZero() {
			// Not probed now. Only existing state is updated; an account
			// first seen while held is enrolled once it is eligible.
			if state := s.loadOpenAITiboAccountState(account.ID); state != nil {
				state.mu.Lock()
				adoptOpenAITiboIdentityLocked(state, OpenAITiboProbeIdentity(account))
				state.http.applyHold(hold)
				state.mu.Unlock()
				held++
			}
			continue
		}
		state := s.openAITiboAccountState(account.ID)
		state.mu.Lock()
		adoptOpenAITiboIdentityLocked(state, OpenAITiboProbeIdentity(account))
		state.http.applyHold(hold)
		firstSeen := state.http.nextProbeAt.IsZero() && state.http.lastSampleAt.IsZero() && state.http.verdict == ""
		s.seedOpenAITiboStateLocked(state, account.Extra, now, cfg)
		if firstSeen && (state.http.nextProbeAt.IsZero() || !state.http.nextProbeAt.After(now)) {
			state.http.nextProbeAt = now.Add(openAITiboStartupDelay(cfg))
		}
		enrolled++
		state.mu.Unlock()
	}
	dropped := s.pruneOpenAITiboAccounts(listed, now)
	if enrolled > 0 || dropped > 0 {
		logger.L().Info("openai_tibo accounts enrolled", zap.Int("accounts", enrolled), zap.Int("held", held), zap.Int("dropped", dropped))
	}
}

// pruneOpenAITiboAccounts drops probe state for accounts missing from the
// latest listing. A route with a probe in flight or a request after the
// listing started is kept for the next pass. The admin view then shows the
// persisted sparkline for those accounts.
func (s *OpenAIGatewayService) pruneOpenAITiboAccounts(listed map[int64]struct{}, listedAt time.Time) int {
	dropped := 0
	s.openaiTiboRoutes.Range(func(key, value any) bool {
		id, ok := key.(int64)
		if !ok {
			return true
		}
		if _, keep := listed[id]; keep {
			return true
		}
		state, ok := value.(*openAITiboAccountState)
		if !ok || state == nil {
			s.openaiTiboRoutes.Delete(key)
			return true
		}
		state.mu.Lock()
		busy := state.http.inflight || state.lastUsedAt.After(listedAt)
		state.mu.Unlock()
		if !busy {
			s.openaiTiboRoutes.CompareAndDelete(key, value)
			dropped++
		}
		return true
	})
	return dropped
}

// latestOpenAITiboAccount reloads the authoritative account row for a
// background probe. Eligibility is the caller's openAITiboHoldFor.
func (s *OpenAIGatewayService) latestOpenAITiboAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, errOpenAITiboAccountUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	account, err := s.accountRepo.GetByID(readCtx, accountID)
	if err != nil || account == nil || account.ID != accountID {
		return nil, errOpenAITiboAccountUnavailable
	}
	current := *account
	current.Extra = maps.Clone(account.Extra)
	current.Credentials = maps.Clone(account.Credentials)
	return &current, nil
}

// probeOpenAITiboRoutes is the harvester-tick hook: probe due HTTP routes of
// enrolled Tibo-routed accounts in the background, including idle ones.
func (s *OpenAIGatewayService) probeOpenAITiboRoutes(ctx context.Context) {
	if s == nil || ctx.Err() != nil || s.httpUpstream == nil || !s.openAITiboRouteEnabled() {
		return
	}
	s.enrollOpenAITiboAccounts(ctx)
	cfg := s.openAITiboRouteConfig()
	now := time.Now()
	s.pruneOpenAITiboPins(now)
	s.openaiTiboStats.rollover(now)
	s.openaiTiboRoutes.Range(func(_, value any) bool {
		state, ok := value.(*openAITiboAccountState)
		if !ok || state == nil {
			return true
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.http.due(now) {
			s.startOpenAITiboProbeCtxLocked(ctx, state, openAITiboRouteHTTP, nil, false, cfg, now)
		}
		return true
	})
}

// openAITiboStats counts process-wide Tibo routing activity per hour.
type openAITiboStats struct {
	mu          sync.Mutex
	hour        time.Time
	probes      int
	flips       int
	served      int
	degraded    int
	lastAlertAt time.Time
}

func (st *openAITiboStats) rolloverLocked(now time.Time) {
	hour := now.Truncate(time.Hour)
	if st.hour.Equal(hour) {
		return
	}
	if !st.hour.IsZero() && (st.probes > 0 || st.served > 0) {
		logger.L().Info("openai_tibo hourly summary", zap.Time("hour", st.hour), zap.Int("probes", st.probes),
			zap.Int("flips", st.flips), zap.Int("served", st.served), zap.Int("degraded_served", st.degraded))
	}
	st.hour, st.probes, st.flips, st.served, st.degraded = hour, 0, 0, 0, 0
}

func (st *openAITiboStats) rollover(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(now)
}

func (st *openAITiboStats) noteProbe(flipped bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(time.Now())
	st.probes++
	if flipped {
		st.flips++
	}
}

func (st *openAITiboStats) noteFlip() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rolloverLocked(time.Now())
	st.flips++
}

// noteServed counts one Tibo-routed request and warns (at most every ten
// minutes) when the degraded share of this hour exceeds the configured ratio.
func (st *openAITiboStats) noteServed(degraded bool, cfg openAITiboSettings) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	st.rolloverLocked(now)
	st.served++
	if degraded {
		st.degraded++
	}
	if cfg.alertRatio <= 0 || st.served < cfg.alertMinRequests {
		return
	}
	ratio := float64(st.degraded) / float64(st.served)
	if ratio <= cfg.alertRatio || now.Sub(st.lastAlertAt) < 10*time.Minute {
		return
	}
	st.lastAlertAt = now
	logger.L().Warn("openai_tibo degraded ratio high", zap.Float64("ratio", ratio), zap.Float64("threshold", cfg.alertRatio),
		zap.Int("served", st.served), zap.Int("degraded_served", st.degraded), zap.Time("hour", st.hour))
}

// OpenAITiboRouteStatus is the admin view of one route's Tibo state.
type OpenAITiboRouteStatus struct {
	Route     string     `json:"route"`
	Verdict   string     `json:"verdict"`
	Confirmed string     `json:"confirmed,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	FlippedAt *time.Time `json:"flipped_at,omitempty"`
	// NextProbeAt is when the route next probes, including any account hold;
	// absent while parked.
	NextProbeAt *time.Time `json:"next_probe_at,omitempty"`
	HeldUntil   *time.Time `json:"held_until,omitempty"`
	Parked      bool       `json:"parked,omitempty"`
	// ClientErrorStreak counts consecutive 4xx probe rejections (backoff).
	ClientErrorStreak int      `json:"client_error_streak,omitempty"`
	PendingVotes      []string `json:"pending_votes,omitempty"`
	LastSample        string   `json:"last_sample,omitempty"`
	LastHTTP          int      `json:"last_http,omitempty"`
	LastAnswer        string   `json:"last_answer,omitempty"`
	// Oldest-first Tibo probe outcomes for the admin sparkline.
	RecentSamples []OpenAITiboSampleStatus `json:"recent_samples,omitempty"`
	ProbesHour    int                      `json:"probes_hour"`
	FlipsHour     int                      `json:"flips_hour"`
	// Calibration counters since process start. A vote window opens on a
	// sample disagreeing with the confirmed verdict; confirmed means it
	// flipped, rejected means later samples outvoted it (sample noise).
	VoteWindows   int `json:"vote_windows"`
	VoteConfirmed int `json:"vote_confirmed"`
	VoteRejected  int `json:"vote_rejected"`
}

type OpenAITiboSampleStatus struct {
	Verdict string     `json:"verdict"`
	At      *time.Time `json:"at,omitempty"`
}

func openAITiboTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// openAITiboRouteStatuses returns HTTP verdict state for the admin view.
// It never starts probes.
func (s *OpenAIGatewayService) openAITiboRouteStatuses(account *Account, now time.Time) []OpenAITiboRouteStatus {
	if s == nil || account == nil || !s.openAITiboRouteApplies(account) {
		return nil
	}
	cfg := s.openAITiboRouteConfig()
	routes := []openAITiboRoute{openAITiboRouteHTTP}
	out := make([]OpenAITiboRouteStatus, 0, len(routes))
	state := s.loadOpenAITiboAccountState(account.ID)
	for _, route := range routes {
		item := OpenAITiboRouteStatus{Route: string(route), Verdict: string(openAITiboUnknown)}
		if state != nil {
			state.mu.Lock()
			r := state.route(route)
			if state.identity != "" && state.identity != OpenAITiboProbeIdentity(account) {
				state.mu.Unlock()
				out = append(out, item)
				continue
			}
			item.Verdict = string(r.effective(now, cfg))
			item.Confirmed = string(r.verdict)
			item.CheckedAt, item.FlippedAt, item.NextProbeAt = openAITiboTimePtr(r.checkedAt), openAITiboTimePtr(r.flippedAt), openAITiboTimePtr(r.nextDue())
			if r.holdUntil.After(now) {
				item.HeldUntil = openAITiboTimePtr(r.holdUntil)
			}
			item.Parked, item.ClientErrorStreak = r.parked, r.clientErrorStreak
			for _, vote := range r.pending {
				item.PendingVotes = append(item.PendingVotes, string(vote))
			}
			if !r.lastSampleAt.IsZero() {
				item.LastSample, item.LastHTTP, item.LastAnswer = string(r.lastSample.verdict), r.lastSample.status, r.lastSample.answerClass
			}
			for _, sample := range r.recent {
				if !sample.Verdict.definitive() {
					continue
				}
				item.RecentSamples = append(item.RecentSamples, OpenAITiboSampleStatus{Verdict: string(sample.Verdict), At: openAITiboTimePtr(sample.At)})
			}
			item.ProbesHour, item.FlipsHour = countOpenAITiboWindow(r.probes, now), countOpenAITiboWindow(r.flips, now)
			item.VoteWindows, item.VoteConfirmed, item.VoteRejected = r.voteWindows, r.voteConfirmed, r.voteRejected
			state.mu.Unlock()
		}
		if account.IsOpenAIOAuth() || account.IsOpenAIBPS() || len(item.RecentSamples) == 0 {
			// OAuth permissions and dots use the same committed observations.
			// A failed write must not advertise a policy that routing cannot see.
			item.RecentSamples = nil
			// Not yet seeded in this process (restart, or skipped while
			// rate-limited/paused): show the persisted sparkline instead of
			// empty dots. Read-only; never seeds state or starts probes.
			raw := account.Extra[openAITiboVerdictExtraKey(route)]
			if !openAITiboHistoryMatchesAccount(raw, account) {
				out = append(out, item)
				continue
			}
			samples := parseOpenAITiboRecentSamples(openAITiboExtraField(raw, "recent_samples"))
			if account.IsOpenAIOAuth() {
				samples = validOpenAIAstraSamples(raw, now)
			}
			for _, sample := range samples {
				if sample.Verdict.definitive() {
					item.RecentSamples = append(item.RecentSamples, OpenAITiboSampleStatus{Verdict: string(sample.Verdict), At: openAITiboTimePtr(sample.At)})
				}
			}
		}
		out = append(out, item)
	}
	return out
}

// observeOpenAITiboResponseModel is the passive model check at the end of a
// business response. An astra request answered by another model is hard
// evidence: the route turns degraded without a vote and a confirmation probe
// is queued. Other models are only logged for now. It returns true for an
// astra mismatch so Cookie WS callers can retire the socket.
func (s *OpenAIGatewayService) observeOpenAITiboResponseModel(account *Account, route openAITiboRoute, sentModel, responseModel string) bool {
	sentModel, responseModel = strings.TrimSpace(sentModel), strings.TrimSpace(responseModel)
	if s == nil || account == nil || sentModel == "" || responseModel == "" || upstreamModelsMatchForAudit(sentModel, responseModel) {
		return false
	}
	astra := sentModel == openAITiboProbeModel
	s.logOpenAITiboModelMismatch(account.ID, route, sentModel, responseModel, astra)
	if !astra {
		return false
	}
	if route != openAITiboRouteCookieWS && s.openAITiboRouteApplies(account) {
		s.forceOpenAITiboDegraded(account, route)
	}
	return true
}

func (s *OpenAIGatewayService) forceOpenAITiboDegraded(account *Account, route openAITiboRoute) {
	state := s.openAITiboAccountState(account.ID)
	state.observationMu.Lock()
	defer state.observationMu.Unlock()
	identity := OpenAITiboProbeIdentity(account)
	if s.accountRepo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		current, err := s.latestOpenAITiboAccount(ctx, account.ID)
		cancel()
		if err != nil {
			return
		}
		if identity != OpenAITiboProbeIdentity(current) {
			state.mu.Lock()
			if state.identity == "" || state.identity == identity {
				adoptOpenAITiboIdentityLocked(state, OpenAITiboProbeIdentity(current))
			}
			state.mu.Unlock()
			return
		}
	}
	cfg := s.openAITiboRouteConfig()
	now := time.Now()
	state.mu.Lock()
	adoptOpenAITiboIdentityLocked(state, identity)
	r := state.route(route)
	next := cloneOpenAITiboRouteState(r)
	before := next.effective(now, cfg)
	changed := next.forceDegraded(now)
	if changed {
		next.flips = append(pruneOpenAITiboWindow(next.flips, now), now)
	}
	record := openAITiboVerdictRecordFor(&next)
	record.ProbeIdentity = identity
	samples := append([]openAITiboRecentSample(nil), next.recent...)
	state.mu.Unlock()
	if err := s.persistOpenAITiboVerdict(account.ID, route, record, samples); err != nil {
		return
	}
	for _, sample := range samples {
		if sample.At.After(next.persistedSampleAt) {
			next.persistedSampleAt = sample.At
		}
	}
	if record.CheckedAt.After(next.persistedAt) {
		next.persistedAt = record.CheckedAt
	}
	state.mu.Lock()
	if state.identity != identity {
		state.mu.Unlock()
		return
	}
	r = state.route(route)
	next.inflight, next.done, next.holdUntil, next.parked = r.inflight, r.done, r.holdUntil, r.parked
	// A tick can reserve a probe while passive persistence is outside state.mu.
	// Preserve its live budget independently of the earlier observation clone.
	next.probes = append([]time.Time(nil), r.probes...)
	*r = next
	if route == openAITiboRouteHTTP {
		r.nextProbeAt = now
		s.startOpenAITiboProbeLocked(state, route, account, false, cfg, now)
	}
	state.mu.Unlock()
	if changed {
		s.openaiTiboStats.noteFlip()
		logger.L().Info("openai_tibo verdict flip", zap.Int64("account_id", account.ID), zap.String("route", string(route)),
			zap.String("from", string(before)), zap.String("to", string(openAITiboDegraded)), zap.String("cause", "response_model_mismatch"))
	}
}

func (s *OpenAIGatewayService) logOpenAITiboModelMismatch(accountID int64, route openAITiboRoute, sentModel, responseModel string, astra bool) {
	every := 10 * time.Minute
	if astra {
		every = time.Minute
	}
	key := strings.Join([]string{strconv.FormatInt(accountID, 10), string(route), sentModel, responseModel}, "\x00")
	now := time.Now()
	if raw, ok := s.openaiTiboMismatchLog.Load(key); ok {
		if last, ok := raw.(time.Time); ok && now.Sub(last) < every {
			return
		}
	}
	s.openaiTiboMismatchLog.Store(key, now)
	logger.L().Info("openai_tibo response model mismatch", zap.Int64("account_id", accountID), zap.String("route", string(route)),
		zap.String("sent_model", sentModel), zap.String("response_model", responseModel), zap.Bool("hard_evidence", astra))
}
