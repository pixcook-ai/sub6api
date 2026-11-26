//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type tiboSettingRepository struct {
	SettingRepository
	data   map[string]string
	getErr error
	setErr error
}

func (r *tiboSettingRepository) GetValue(_ context.Context, key string) (string, error) {
	if r.getErr != nil {
		return "", r.getErr
	}
	value, ok := r.data[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}
func (r *tiboSettingRepository) Set(_ context.Context, key, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.data[key] = value
	return nil
}

type tiboSettingsGroupReader struct {
	groups map[int64]*Group
	err    error
}

func (r *tiboSettingsGroupReader) GetByID(_ context.Context, id int64) (*Group, error) {
	if r.err != nil {
		return nil, r.err
	}
	if group := r.groups[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func TestOpenAITiboSettingsDefaultIsOptIn(t *testing.T) {
	service := NewSettingService(&tiboSettingRepository{data: map[string]string{}}, nil)
	settings, err := service.GetOpenAITiboSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, TiboAutomationSettings{PauseOnDegraded: true}, settings)
}

func TestOpenAITiboSettingsGroupingIsOptional(t *testing.T) {
	repository := &tiboSettingRepository{data: map[string]string{}}
	service := NewSettingService(repository, nil)
	// No groups or group reader are required for quality isolation alone.
	settings := TiboAutomationSettings{Enabled: true, PauseOnDegraded: true}
	require.NoError(t, service.UpdateOpenAITiboSettings(context.Background(), settings))
	got, err := service.GetOpenAITiboSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, settings, got)
	require.Contains(t, repository.data[SettingKeyOpenAITiboSettings], `"auto_group_enabled":false`)
}

func TestOpenAITiboSettingsGroupIDsNotNames(t *testing.T) {
	repository := &tiboSettingRepository{data: map[string]string{}}
	service := NewSettingService(repository, nil)
	service.SetDefaultSubscriptionGroupReader(&tiboSettingsGroupReader{groups: map[int64]*Group{
		17: {ID: 17, Name: "自选正常账号", Platform: PlatformOpenAI, Status: StatusActive},
		29: {ID: 29, Name: "自选观察账号", Platform: PlatformOpenAI, Status: StatusActive},
	}})
	settings := TiboAutomationSettings{Enabled: true, AutoGroupEnabled: true, HealthyGroupID: 17, DegradedGroupID: 29}
	require.NoError(t, service.UpdateOpenAITiboSettings(context.Background(), settings))
	got, err := service.GetOpenAITiboSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, settings, got)
	require.NotContains(t, repository.data[SettingKeyOpenAITiboSettings], "GPT-")
}

func TestOpenAITiboSettingsRejectInvalidGroups(t *testing.T) {
	for _, tc := range []struct {
		name              string
		healthy, degraded int64
		group             *Group
	}{
		{name: "unselected", healthy: 0, degraded: 29},
		{name: "same group", healthy: 17, degraded: 17},
		{name: "not found", healthy: 17, degraded: 29},
		{name: "wrong platform", healthy: 17, degraded: 29, group: &Group{ID: 29, Platform: PlatformAnthropic, Status: StatusActive}},
		{name: "inactive", healthy: 17, degraded: 29, group: &Group{ID: 29, Platform: PlatformOpenAI, Status: "disabled"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := &tiboSettingRepository{data: map[string]string{}}
			service := NewSettingService(repository, nil)
			groups := map[int64]*Group{17: {ID: 17, Platform: PlatformOpenAI, Status: StatusActive}}
			if tc.group != nil {
				groups[29] = tc.group
			}
			service.SetDefaultSubscriptionGroupReader(&tiboSettingsGroupReader{groups: groups})
			err := service.UpdateOpenAITiboSettings(context.Background(), TiboAutomationSettings{AutoGroupEnabled: true, HealthyGroupID: tc.healthy, DegradedGroupID: tc.degraded})
			require.Error(t, err)
			require.Empty(t, repository.data)
		})
	}
}

func TestOpenAITiboSettingsMalformedOrUnavailableDoesNotEnable(t *testing.T) {
	for _, raw := range []string{"null", "[]", "invalid", `{"enabled":true,"auto_group_enabled":true}`} {
		settings, err := DecodeOpenAITiboSettings(raw)
		require.Error(t, err)
		require.False(t, settings.Enabled)
	}
	failure := errors.New("database unavailable")
	service := NewSettingService(&tiboSettingRepository{getErr: failure}, nil)
	settings, err := service.GetOpenAITiboSettings(context.Background())
	require.ErrorIs(t, err, failure)
	require.False(t, settings.Enabled)
}

func TestOpenAITiboSettingsUpdateFailureKeepsPreviousCache(t *testing.T) {
	repository := &tiboSettingRepository{data: map[string]string{}}
	service := NewSettingService(repository, nil)
	original, err := service.GetOpenAITiboSettings(context.Background())
	require.NoError(t, err)
	repository.setErr = errors.New("database unavailable")
	require.Error(t, service.UpdateOpenAITiboSettings(context.Background(), TiboAutomationSettings{Enabled: true}))
	current, err := service.GetOpenAITiboSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, original, current)
}
