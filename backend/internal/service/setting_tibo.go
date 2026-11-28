package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

const SettingKeyOpenAITiboSettings = "openai_tibo_settings"
const OpenAITiboQualityIsolatedExtraKey = "openai_tibo_quality_isolated"
const OpenAITiboQualityIsolationAtExtraKey = "openai_tibo_quality_isolation_at"
const openAITiboSettingsCacheTTL = 5 * time.Second

// TiboAutomationSettings keeps probing, quality isolation and optional group
// movement independent. Group IDs identify administrator-selected groups;
// their names have no behavioral meaning.
type TiboAutomationSettings struct {
	Enabled             bool  `json:"enabled"`
	AutoGroupEnabled    bool  `json:"auto_group_enabled"`
	HealthyGroupID      int64 `json:"healthy_group_id"`
	DegradedGroupID     int64 `json:"degraded_group_id"`
	BPSHealthyGroupID   int64 `json:"bps_healthy_group_id"`
	BPSDegradedGroupID  int64 `json:"bps_degraded_group_id"`
	PauseOnDegraded     bool  `json:"pause_on_degraded"`
	AutoResumeOnHealthy bool  `json:"auto_resume_on_healthy"`
}

// GroupIDsForPlatform keeps automatic membership changes within the account's
// own platform. The original group fields remain the OpenAI configuration.
func (settings TiboAutomationSettings) GroupIDsForPlatform(platform string) (healthy, degraded int64) {
	switch platform {
	case PlatformOpenAI:
		return settings.HealthyGroupID, settings.DegradedGroupID
	case PlatformOpenAIBPS:
		return settings.BPSHealthyGroupID, settings.BPSDegradedGroupID
	default:
		return 0, 0
	}
}

type cachedOpenAITiboSettings struct {
	settings  TiboAutomationSettings
	expiresAt time.Time
}

func DefaultOpenAITiboSettings() TiboAutomationSettings {
	return TiboAutomationSettings{PauseOnDegraded: true}
}

// DecodeOpenAITiboSettings is shared with the transactional probe writer so
// missing fields have the same safe defaults in both paths.
func DecodeOpenAITiboSettings(value string) (TiboAutomationSettings, error) {
	settings := DefaultOpenAITiboSettings()
	if strings.TrimSpace(value) == "" {
		return settings, nil
	}
	if strings.TrimSpace(value) == "null" {
		return settings, fmt.Errorf("tibo settings must be a JSON object")
	}
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		return DefaultOpenAITiboSettings(), fmt.Errorf("decode Tibo settings: %w", err)
	}
	if err := validateOpenAITiboSettings(settings); err != nil {
		return DefaultOpenAITiboSettings(), err
	}
	return settings, nil
}

func validateOpenAITiboSettings(settings TiboAutomationSettings) error {
	configured := false
	for _, platform := range []string{PlatformOpenAI, PlatformOpenAIBPS} {
		healthy, degraded := settings.GroupIDsForPlatform(platform)
		if healthy < 0 || degraded < 0 {
			return infraerrors.BadRequest("TIBO_GROUP_INVALID", "Tibo group IDs must not be negative")
		}
		if settings.AutoGroupEnabled && (healthy != 0 || degraded != 0) {
			if healthy == 0 || degraded == 0 || healthy == degraded {
				return infraerrors.BadRequest("TIBO_GROUP_INVALID", "choose two different groups for each configured Tibo platform")
			}
			configured = true
		}
	}
	if settings.AutoGroupEnabled && !configured {
		return infraerrors.BadRequest("TIBO_GROUP_INVALID", "choose an OpenAI or OpenAI BPS group pair before enabling Tibo automatic grouping")
	}
	return nil
}

func (s *SettingService) GetOpenAITiboSettings(ctx context.Context) (TiboAutomationSettings, error) {
	if s == nil || s.settingRepo == nil {
		return DefaultOpenAITiboSettings(), nil
	}
	if cached := s.openAITiboSettingsCache.Load(); cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.settings, nil
	}
	s.openAITiboSettingsMu.Lock()
	defer s.openAITiboSettingsMu.Unlock()
	if cached := s.openAITiboSettingsCache.Load(); cached != nil && time.Now().Before(cached.expiresAt) {
		return cached.settings, nil
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAITiboSettings)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return DefaultOpenAITiboSettings(), fmt.Errorf("get Tibo settings: %w", err)
	}
	settings, err := DecodeOpenAITiboSettings(value)
	if err != nil {
		return DefaultOpenAITiboSettings(), err
	}
	s.openAITiboSettingsCache.Store(&cachedOpenAITiboSettings{settings: settings, expiresAt: time.Now().Add(openAITiboSettingsCacheTTL)})
	return settings, nil
}

func (s *SettingService) UpdateOpenAITiboSettings(ctx context.Context, settings TiboAutomationSettings) error {
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("tibo settings repository is unavailable")
	}
	if err := validateOpenAITiboSettings(settings); err != nil {
		return err
	}
	if settings.AutoGroupEnabled {
		if s.defaultSubGroupReader == nil {
			return fmt.Errorf("tibo group validation is unavailable")
		}
		for _, platform := range []string{PlatformOpenAI, PlatformOpenAIBPS} {
			healthy, degraded := settings.GroupIDsForPlatform(platform)
			if healthy == 0 && degraded == 0 {
				continue
			}
			for _, id := range []int64{healthy, degraded} {
				group, err := s.defaultSubGroupReader.GetByID(ctx, id)
				if err != nil || group == nil || group.Platform != platform || group.Status != StatusActive {
					if err != nil && !errors.Is(err, ErrGroupNotFound) {
						return fmt.Errorf("validate Tibo group %d: %w", id, err)
					}
					return infraerrors.BadRequest("TIBO_GROUP_INVALID", "Tibo groups must exist, be active and belong to their configured platform")
				}
			}
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	s.openAITiboSettingsMu.Lock()
	if err := s.settingRepo.Set(ctx, SettingKeyOpenAITiboSettings, string(raw)); err != nil {
		s.openAITiboSettingsMu.Unlock()
		return fmt.Errorf("save Tibo settings: %w", err)
	}
	s.openAITiboSettingsCache.Store(&cachedOpenAITiboSettings{settings: settings, expiresAt: time.Now().Add(openAITiboSettingsCacheTTL)})
	s.openAITiboSettingsMu.Unlock()
	if s.onUpdate != nil {
		s.onUpdate()
	}
	return nil
}
