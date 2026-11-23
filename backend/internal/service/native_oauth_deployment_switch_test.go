//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func switchDeploymentFixture() (*OpenAIGatewayService, *deploymentTestRepo) {
	s, r := deploymentFixture()
	r.deployment.Codex.Subject = r.deployment.BPS.Subject
	r.deployment.Codex.WorkspaceID = r.deployment.BPS.WorkspaceID
	c := r.accounts[12]
	c.Status, c.Schedulable, c.GroupIDs = StatusActive, true, []int64{1}
	marker := c.Extra[ManagedOAuthExtraKey].(map[string]any)
	marker["subject"], marker["workspace_id"] = r.deployment.Codex.Subject, r.deployment.Codex.WorkspaceID
	marker["token_revision"] = int64(1)
	marker["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	marker["last_operation_id"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	marker["last_operation_hash"] = strings.Repeat("a", 64)
	return s, r
}

func TestManagedDeploymentSwitchOnlyChangesNewSessions(t *testing.T) {
	s, r := switchDeploymentFixture()
	ctx := context.Background()
	old, err := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "before", nil)
	require.NoError(t, err)
	require.NoError(t, s.RecordManagedDeploymentResource(ctx, old, "file", "file_before"))
	in := ManagedDeploymentSwitchRequest{ExpectedRouteRevision: 1, SelectedProvider: "codex", RequestedModel: "gpt-5", RequiredCapabilities: []OpenAIEndpointCapability{OpenAIEndpointCapabilityChatCompletions}, ScheduleNewSessions: true}
	state, err := s.SwitchManagedDeployment(ctx, r.deployment.DeploymentID, in)
	require.NoError(t, err)
	require.Equal(t, "codex", state.SelectedProvider)
	require.Equal(t, int64(2), state.RouteRevision)
	fresh, err := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "after", nil)
	require.NoError(t, err)
	require.Equal(t, "codex", fresh.Binding.Provider)
	continued, err := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "", []ManagedDeploymentResourceRef{{Kind: "file", ID: "file_before"}})
	require.NoError(t, err)
	require.Equal(t, old.Binding, continued.Binding)
	in.ExpectedRouteRevision, in.SelectedProvider = 2, "bps_excel"
	state, err = s.SwitchManagedDeployment(ctx, r.deployment.DeploymentID, in)
	require.NoError(t, err)
	require.Equal(t, int64(3), state.RouteRevision)
	again, err := s.BindManagedDeploymentRequest(ctx, 1, 1, 1, "after", nil)
	require.NoError(t, err)
	require.Equal(t, fresh.Binding, again.Binding)
}

func TestManagedDeploymentSwitchRejectsInvalidOrUnavailableTargetWithoutChangingRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ManagedDeploymentSwitchRequest, *deploymentTestRepo)
		want   error
	}{
		{"stale revision", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) { in.ExpectedRouteRevision = 2 }, ErrManagedDeploymentConflict},
		{"missing scheduling intent", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) { in.ScheduleNewSessions = false }, ErrManagedDeploymentInvalid},
		{"missing model", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) { in.RequestedModel = "" }, ErrManagedDeploymentInvalid},
		{"unknown capability", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) {
			in.RequiredCapabilities = []OpenAIEndpointCapability{"invented"}
		}, ErrManagedDeploymentInvalid},
		{"different subject", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) {
			r.deployment.Codex.Subject = "another"
			r.accounts[12].Extra[ManagedOAuthExtraKey].(map[string]any)["subject"] = "another"
		}, ErrManagedDeploymentUnavailable},
		{"different workspace", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) {
			r.deployment.Codex.WorkspaceID = "another"
			r.accounts[12].Extra[ManagedOAuthExtraKey].(map[string]any)["workspace_id"] = "another"
		}, ErrManagedDeploymentUnavailable},
		{"manually paused", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) { r.accounts[12].Schedulable = false }, ErrManagedDeploymentUnavailable},
		{"other group", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) { r.accounts[12].GroupIDs = []int64{2} }, ErrManagedDeploymentUnavailable},
		{"unsupported model", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) {
			r.accounts[12].Credentials = map[string]any{"model_mapping": map[string]any{"allowed": "allowed"}}
		}, ErrManagedDeploymentUnavailable},
		{"unsupported capability", func(in *ManagedDeploymentSwitchRequest, r *deploymentTestRepo) {
			in.RequiredCapabilities = []OpenAIEndpointCapability{OpenAIEndpointCapabilityEmbeddings}
		}, ErrManagedDeploymentUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, r := switchDeploymentFixture()
			in := ManagedDeploymentSwitchRequest{ExpectedRouteRevision: 1, SelectedProvider: "codex", RequestedModel: "gpt-5", RequiredCapabilities: []OpenAIEndpointCapability{OpenAIEndpointCapabilityChatCompletions}, ScheduleNewSessions: true}
			tc.mutate(&in, r)
			_, err := s.SwitchManagedDeployment(context.Background(), r.deployment.DeploymentID, in)
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, int64(1), r.deployment.RouteRevision)
			require.Equal(t, "bps_excel", r.deployment.SelectedProvider)
		})
	}
}

func TestManagedDeploymentSwitchOptionsRequireExplicitContext(t *testing.T) {
	s, r := switchDeploymentFixture()
	state, err := s.GetManagedDeploymentSwitchOptions(context.Background(), r.deployment.DeploymentID, "", nil)
	require.NoError(t, err)
	require.True(t, state.ContextRequired)
	require.Len(t, state.Options, 2)
	for _, option := range state.Options {
		require.False(t, option.Available)
		require.Equal(t, "request_context_required", option.Reason)
	}
}
