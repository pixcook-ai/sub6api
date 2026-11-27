package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const bpsUsageSnapshotKey = "bps_usage_snapshot"

type bpsUsageSnapshot struct {
	CredentialIdentity string         `json:"credential_identity"`
	FetchedAt          time.Time      `json:"fetched_at"`
	FiveHour           *UsageProgress `json:"five_hour,omitempty"`
	SevenDay           *UsageProgress `json:"seven_day,omitempty"`
	ThirtyDay          *UsageProgress `json:"thirty_day,omitempty"`
}

// BPS uses its own grant. A read failure must not change its credentials or scheduling.
func (s *AccountUsageService) getOpenAIBPSUsage(ctx context.Context, account *Account, force ...bool) (*UsageInfo, error) {
	now := time.Now()
	usage := &UsageInfo{UpdatedAt: &now}
	if account == nil {
		return usage, nil
	}
	var snapshot bpsUsageSnapshot
	if raw, ok := account.Extra[bpsUsageSnapshotKey]; ok {
		if data, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(data, &snapshot)
		}
	}
	credentialIdentity := OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(account))
	if snapshot.CredentialIdentity != credentialIdentity {
		snapshot = bpsUsageSnapshot{}
	}
	forceProbe := len(force) > 0 && force[0]
	if (forceProbe || snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) >= openAIProbeCacheTTL) && s.shouldProbeOpenAICodexSnapshot(account.ID, now, forceProbe) {
		fresh, err := s.fetchOpenAIBPSUsage(ctx, account)
		if err != nil {
			usage.ErrorCode = "quota_unavailable"
			usage.Error = "BPS upstream quota is unavailable; local usage is still shown."
		} else {
			snapshot = *fresh
			updates := map[string]any{bpsUsageSnapshotKey: snapshot}
			if s.accountRepo != nil {
				_ = s.accountRepo.UpdateExtra(ctx, account.ID, updates)
			}
			mergeAccountExtra(account, updates)
		}
	}
	if snapshot.FetchedAt.IsZero() || now.Sub(snapshot.FetchedAt) >= openAIProbeCacheTTL {
		usage.FiveHour, usage.SevenDay = &UsageProgress{QuotaUnknown: true}, &UsageProgress{QuotaUnknown: true}
		if snapshot.ThirtyDay != nil {
			usage.ThirtyDay = &UsageProgress{QuotaUnknown: true}
		}
	} else {
		usage.FiveHour = bpsUsageProgress(snapshot.FiveHour, now)
		usage.SevenDay = bpsUsageProgress(snapshot.SevenDay, now)
		if snapshot.ThirtyDay != nil {
			usage.ThirtyDay = bpsUsageProgress(snapshot.ThirtyDay, now)
		}
	}
	hasMonthlyQuota := usage.ThirtyDay != nil && !usage.ThirtyDay.QuotaUnknown
	if usage.ErrorCode != "" || (!hasMonthlyQuota && (usage.FiveHour.QuotaUnknown || usage.SevenDay.QuotaUnknown)) {
		usage.ErrorCode = "quota_unavailable"
		usage.Error = "BPS upstream quota is unavailable; local usage is still shown."
	}
	if !snapshot.FetchedAt.IsZero() {
		usage.UpdatedAt = &snapshot.FetchedAt
	}
	if s.usageLogRepo != nil {
		if stats, err := s.usageLogRepo.GetAccountWindowStats(ctx, account.ID, codexWindowStatsStart(usage.FiveHour, 5*time.Hour, now)); err == nil {
			usage.FiveHour.WindowStats = windowStatsFromAccountStats(stats)
		}
		if stats, err := s.usageLogRepo.GetAccountWindowStats(ctx, account.ID, codexWindowStatsStart(usage.SevenDay, 7*24*time.Hour, now)); err == nil {
			usage.SevenDay.WindowStats = windowStatsFromAccountStats(stats)
		}
		if usage.ThirtyDay != nil {
			if stats, err := s.usageLogRepo.GetAccountWindowStats(ctx, account.ID, codexWindowStatsStart(usage.ThirtyDay, 30*24*time.Hour, now)); err == nil {
				usage.ThirtyDay.WindowStats = windowStatsFromAccountStats(stats)
			}
		}
	}
	return usage, nil
}

func bpsUsageProgress(cached *UsageProgress, now time.Time) *UsageProgress {
	if cached == nil || (cached.ResetsAt != nil && !cached.ResetsAt.After(now)) {
		return &UsageProgress{QuotaUnknown: true}
	}
	progress := *cached
	if progress.ResetsAt != nil {
		progress.RemainingSeconds = max(0, int(progress.ResetsAt.Sub(now).Seconds()))
	}
	return &progress
}

func (s *AccountUsageService) fetchOpenAIBPSUsage(ctx context.Context, account *Account) (*bpsUsageSnapshot, error) {
	if s.openAIQuotaService == nil || s.openAIQuotaService.privacyClientFactory == nil {
		return nil, fmt.Errorf("quota client is unavailable")
	}
	ctx, release, err := beginOpenAIRequestProxyWithRepository(ctx, nil, account, s.openAIUsageProxyRepository())
	if err != nil {
		return nil, err
	}
	defer release()

	token := strings.TrimSpace(account.GetCredential("access_token"))
	accountID := strings.TrimSpace(account.GetCredential("chatgpt_account_id"))
	if token == "" || accountID == "" || account.bpsTokenExpired() {
		return nil, fmt.Errorf("BPS credentials are unavailable")
	}
	proxyURL, err := resolveOpenAIMaintenanceProxy(ctx, account, s.openAIUsageProxyRepository())
	if err != nil {
		return nil, err
	}
	if !account.OpenAIRequestProxyEnabled() && account.ProxyID != nil {
		if account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		} else if s.openAIQuotaService.proxyRepo != nil {
			proxy, err := s.openAIQuotaService.proxyRepo.GetByID(ctx, *account.ProxyID)
			if err != nil || proxy == nil {
				return nil, fmt.Errorf("BPS proxy is unavailable")
			}
			proxyURL = proxy.URL()
		} else {
			return nil, fmt.Errorf("BPS proxy is unavailable")
		}
	}
	client, err := s.openAIQuotaService.privacyClientFactory(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("quota client is unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, openaiQuotaUpstreamTimeout)
	defer cancel()
	var payload struct {
		RateLimit *struct {
			PrimaryWindow   *bpsQuotaWindow `json:"primary_window"`
			SecondaryWindow *bpsQuotaWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	resp, err := client.R().SetContext(callCtx).
		SetHeaders(buildCodexCommonHeaders(token, accountID, account.IsChatGPTAccountFedRAMP())).
		SetSuccessResult(&payload).Get(chatGPTUsageURL)
	if err != nil || !resp.IsSuccessState() || payload.RateLimit == nil {
		return nil, fmt.Errorf("BPS quota read failed")
	}
	now := time.Now()
	snapshot := &bpsUsageSnapshot{FetchedAt: now, CredentialIdentity: OpenAIBPSCredentialIdentity(OpenAIBPSCredentialSnapshotFromAccount(account))}
	for _, window := range []*bpsQuotaWindow{payload.RateLimit.PrimaryWindow, payload.RateLimit.SecondaryWindow} {
		if window == nil || window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || math.IsInf(*window.UsedPercent, 0) || *window.UsedPercent < 0 {
			continue
		}
		progress := &UsageProgress{Utilization: *window.UsedPercent}
		resetAt := window.ResetAt
		if resetAt <= 0 && window.ResetAfterSeconds > 0 {
			resetAt = now.Unix() + window.ResetAfterSeconds
		}
		if resetAt > 0 {
			resetTime := time.Unix(resetAt, 0)
			progress.ResetsAt = &resetTime
			progress.RemainingSeconds = max(0, int(resetAt-now.Unix()))
		}
		switch window.LimitWindowSeconds {
		case int64(5 * time.Hour / time.Second):
			snapshot.FiveHour = progress
		case int64(7 * 24 * time.Hour / time.Second):
			snapshot.SevenDay = progress
		case int64(30 * 24 * time.Hour / time.Second):
			snapshot.ThirtyDay = progress
		}
	}
	if snapshot.FiveHour == nil && snapshot.SevenDay == nil && snapshot.ThirtyDay == nil {
		return nil, fmt.Errorf("BPS quota windows are unavailable")
	}
	return snapshot, nil
}

type bpsQuotaWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAfterSeconds  int64    `json:"reset_after_seconds"`
	ResetAt            int64    `json:"reset_at"`
}
