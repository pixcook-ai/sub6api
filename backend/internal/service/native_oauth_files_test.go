package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai_compat"
	"github.com/MACOS-DO/sub4api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

const coreOAuthFilesTestKey = "synthetic-files-service-key-1234567890"

type coreOAuthFilesTestUpstream struct{ client *http.Client }

func (u *coreOAuthFilesTestUpstream) Do(r *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	if !HTTPUpstreamRedirectsDisabled(r.Context()) || r.GetBody != nil {
		return nil, ErrCoreOAuthInvalid
	}
	client := *u.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(r)
}

func (u *coreOAuthFilesTestUpstream) DoWithTLS(r *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, proxy, accountID, concurrency)
}

func coreOAuthFilesDescription(identity managedfiles.Identity) managedfiles.DescribeResult {
	return managedfiles.DescribeResult{
		Contract:    managedfiles.Contract,
		Identity:    identity,
		JSONProfile: managedfiles.JSONProfile,
		Limits: managedfiles.Limits{FileBytes: 1024, MessageBytes: 4096, MessagePositions: 10, StagingBytes: 1 << 20,
			AdmissionBytes: 1 << 16, ConcurrentAdmissions: 2, LiveAdmissions: 4, AdmissionTTLSeconds: 60,
			ManifestBytes: 1 << 16, UniqueFiles: 8, FreeSpaceReserveBytes: 1, AdmitTimeoutSeconds: 30, EnsureTimeoutSeconds: 30},
		Capabilities: managedfiles.Capabilities{BatchAdmit: true, EnsureUpload: true, MetadataRecovery: true},
	}
}

func TestCoreOAuthWorkerFilesDiscoveryBindsIndependentKeyAndIdentity(t *testing.T) {
	want := managedfiles.Identity{BackendID: "synthetic-backend", SubjectFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CredentialGeneration: "22222222-2222-4222-8222-222222222222"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, managedfiles.Prefix+"describe", r.URL.Path)
		require.Equal(t, coreOAuthFilesTestKey, r.Header.Get(managedfiles.ServiceKeyHeader))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, "bps-metadata-recovery/1", r.Header.Get(managedfiles.RecoveryFeatureHeader))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(coreOAuthFilesDescription(want))
	}))
	defer server.Close()
	s, account, binding, snapshot, _ := coreWorkerFixture(server.URL)
	account.Credentials["bps_files_service_key"] = coreOAuthFilesTestKey
	s.httpUpstream = &coreOAuthFilesTestUpstream{client: server.Client()}
	s.cfg = &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}

	got, err := s.discoverCoreOAuthWorkerFiles(context.Background(), account, binding, snapshot, coreOAuthFilesTestKey)
	require.NoError(t, err)
	require.Equal(t, want, got)

	for _, key := range []string{"", workerTestKey, "synthetic-inference-key"} {
		_, err = s.discoverCoreOAuthWorkerFiles(context.Background(), account, binding, snapshot, key)
		require.ErrorIs(t, err, ErrCoreOAuthInvalid)
	}
}

func TestCoreOAuthWorkerFilesDiscoveryRejectsWrongIdentityAndCapabilities(t *testing.T) {
	want := managedfiles.Identity{BackendID: "synthetic-backend", SubjectFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CredentialGeneration: "22222222-2222-4222-8222-222222222222"}
	for _, tc := range []struct {
		name   string
		mutate func(*managedfiles.DescribeResult)
	}{
		{name: "wrong backend", mutate: func(d *managedfiles.DescribeResult) { d.Identity.BackendID = "other-worker" }},
		{name: "wrong generation", mutate: func(d *managedfiles.DescribeResult) {
			d.Identity.CredentialGeneration = "33333333-3333-4333-8333-333333333333"
		}},
		{name: "wrong contract", mutate: func(d *managedfiles.DescribeResult) { d.Contract = "other-contract" }},
		{name: "missing batch capability", mutate: func(d *managedfiles.DescribeResult) { d.Capabilities.BatchAdmit = false }},
		{name: "missing upload capability", mutate: func(d *managedfiles.DescribeResult) { d.Capabilities.EnsureUpload = false }},
		{name: "invalid limit", mutate: func(d *managedfiles.DescribeResult) { d.Limits.FileBytes = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				description := coreOAuthFilesDescription(want)
				tc.mutate(&description)
				_ = json.NewEncoder(w).Encode(description)
			}))
			defer server.Close()
			s, account, binding, snapshot, _ := coreWorkerFixture(server.URL)
			s.httpUpstream = &coreOAuthFilesTestUpstream{client: server.Client()}
			s.cfg = &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
			_, err := s.discoverCoreOAuthWorkerFiles(context.Background(), account, binding, snapshot, coreOAuthFilesTestKey)
			require.Error(t, err)
		})
	}
}

func TestCoreOAuthRuntimeFilesUpdateStoresOnlyBoundFileServiceIdentity(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour)
	snapshot := &CoreOAuthCredentialSnapshot{AccessToken: "synthetic-access-token", Profile: CoreOAuthProfile{ProfileRef: "11111111-1111-4111-8111-111111111111", Provider: CoreOAuthProviderExcel, Authorized: true, ClientID: coreOAuthClientID(CoreOAuthProviderExcel),
		Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 1, ExpiresAt: &expiry}}
	binding := ManagedDeploymentBinding{Provider: CoreOAuthProviderExcel, AccountID: 7, BackendID: "worker-1", BindingRef: "bind_33333333-3333-4333-8333-333333333333",
		ProfileRef: "oauth_" + snapshot.Profile.ProfileRef, Subject: snapshot.Profile.Subject, WorkspaceID: snapshot.Profile.WorkspaceID, IdentityEpoch: snapshot.Profile.IdentityEpoch}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3, Schedulable: false, GroupIDs: []int64{1}, Credentials: map[string]any{"api_key": "inference-key", "base_url": "https://worker.example/v1", "core_oauth_control_key": workerTestKey},
		Extra: map[string]any{"core_oauth_runtime_shell": map[string]any{"provider": "bps_excel", "group_id": float64(1)}, openai_compat.ExtraKeyResponsesSupported: true}}
	service := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	identity := managedfiles.Identity{BackendID: binding.BackendID, SubjectFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CredentialGeneration: binding.IdentityEpoch}
	got, err := prepareCoreOAuthFilesRuntimeUpdate(service, account, binding, snapshot, "https://worker.example", coreOAuthFilesTestKey, identity)
	require.NoError(t, err)
	_, err = coreOAuthFilesServiceKey(account, binding, snapshot, nil, "")
	require.ErrorIs(t, err, ErrManagedDeploymentInvalid)
	require.Equal(t, managedfiles.Contract, got.Credentials["bps_files_contract"])
	require.Equal(t, "https://worker.example", got.Credentials["bps_files_control_url"])
	require.Equal(t, coreOAuthFilesTestKey, got.Credentials["bps_files_service_key"])
	require.Equal(t, identity.SubjectFingerprint, got.Credentials["bps_files_subject_fingerprint"])
	require.Equal(t, identity.CredentialGeneration, got.Credentials["bps_files_credential_generation"])
	require.Equal(t, []string{string(OpenAIEndpointCapabilityChatCompletions), string(OpenAIEndpointCapabilityResponses), string(OpenAIEndpointCapabilityBPSFiles)}, got.Credentials[openAIEndpointCapabilitiesCredentialKey])
	updatedAccount := &Account{ID: account.ID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: account.Concurrency,
		Extra: got.Extra, Credentials: got.Credentials}
	require.True(t, updatedAccount.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses))
	require.True(t, updatedAccount.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityBPSFiles))
	_, err = BPSFilesConfigForAccount(updatedAccount)
	require.NoError(t, err)
	require.Equal(t, workerTestKey, got.Credentials["core_oauth_control_key"])
	require.Equal(t, "inference-key", got.Credentials["api_key"])
	require.NotContains(t, got.Credentials, "access_token")
	require.NotContains(t, got.Credentials, "refresh_token")
	require.NotContains(t, got.Credentials, "id_token")
	require.NotContains(t, account.Credentials, "bps_files_service_key")

	runtimeAccount := *account
	runtimeAccount.Credentials = got.Credentials
	runtimeAccount.Extra = got.Extra
	existing := &ManagedDeployment{BPS: binding}
	reused, err := coreOAuthFilesServiceKey(&runtimeAccount, binding, snapshot, existing, "")
	require.NoError(t, err)
	require.Equal(t, coreOAuthFilesTestKey, reused)
	changedBinding := binding
	changedBinding.IdentityEpoch = "33333333-3333-4333-8333-333333333333"
	_, err = coreOAuthFilesServiceKey(&runtimeAccount, changedBinding, snapshot, existing, "")
	require.ErrorIs(t, err, ErrManagedDeploymentInvalid)
	require.NoError(t, validateCoreOAuthFilesIdentity(&runtimeAccount, identity))
	changedIdentity := identity
	changedIdentity.SubjectFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.ErrorIs(t, validateCoreOAuthFilesIdentity(&runtimeAccount, changedIdentity), ErrCoreOAuthConflict)
}

func TestCoreOAuthRuntimeFilesUpdateRejectsMalformedAndMismatchedIdentity(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour)
	snapshot := &CoreOAuthCredentialSnapshot{AccessToken: "synthetic-access-token", Profile: CoreOAuthProfile{ProfileRef: "11111111-1111-4111-8111-111111111111", Provider: CoreOAuthProviderExcel, Authorized: true, ClientID: coreOAuthClientID(CoreOAuthProviderExcel),
		Subject: "subject", WorkspaceID: "workspace", IdentityEpoch: "22222222-2222-4222-8222-222222222222", TokenRevision: 1, ExpiresAt: &expiry}}
	binding := ManagedDeploymentBinding{Provider: CoreOAuthProviderExcel, AccountID: 7, BackendID: "worker-1", BindingRef: "bind_33333333-3333-4333-8333-333333333333",
		ProfileRef: "oauth_" + snapshot.Profile.ProfileRef, Subject: snapshot.Profile.Subject, WorkspaceID: snapshot.Profile.WorkspaceID, IdentityEpoch: snapshot.Profile.IdentityEpoch}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 3, GroupIDs: []int64{1}, Credentials: map[string]any{"api_key": "inference-key", "base_url": "https://worker.example/v1", "core_oauth_control_key": workerTestKey},
		Extra: map[string]any{"core_oauth_runtime_shell": map[string]any{"provider": "bps_excel", "group_id": float64(1)}, openai_compat.ExtraKeyResponsesSupported: true}}
	good := managedfiles.Identity{BackendID: binding.BackendID, SubjectFingerprint: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CredentialGeneration: binding.IdentityEpoch}
	for _, tc := range []struct {
		name     string
		key      string
		identity managedfiles.Identity
	}{
		{name: "short key", key: "short", identity: good},
		{name: "same as inference key", key: "inference-key", identity: good},
		{name: "same as control key", key: workerTestKey, identity: good},
		{name: "wrong backend", key: coreOAuthFilesTestKey, identity: managedfiles.Identity{BackendID: "other", SubjectFingerprint: good.SubjectFingerprint, CredentialGeneration: good.CredentialGeneration}},
		{name: "wrong generation", key: coreOAuthFilesTestKey, identity: managedfiles.Identity{BackendID: good.BackendID, SubjectFingerprint: good.SubjectFingerprint, CredentialGeneration: "33333333-3333-4333-8333-333333333333"}},
		{name: "malformed fingerprint", key: coreOAuthFilesTestKey, identity: managedfiles.Identity{BackendID: good.BackendID, SubjectFingerprint: "sha256:invalid", CredentialGeneration: good.CredentialGeneration}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
			_, err := prepareCoreOAuthFilesRuntimeUpdate(service, account, binding, snapshot, "https://worker.example", tc.key, tc.identity)
			require.Error(t, err)
		})
	}
	service := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}}
	_, err := prepareCoreOAuthFilesRuntimeUpdate(service, account, binding, snapshot, "https://worker.example", strings.Repeat("x", 31)+"\n", good)
	require.Error(t, err)
}
