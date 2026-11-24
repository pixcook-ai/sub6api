package service

import (
	"context"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func (s *OpenAIGatewayService) discoverCoreOAuthWorkerFiles(ctx context.Context, account *Account, binding ManagedDeploymentBinding, snapshot *CoreOAuthCredentialSnapshot, serviceKey string) (managedfiles.Identity, error) {
	var zero managedfiles.Identity
	if s == nil || account == nil || snapshot == nil || !coreWorkerBindingValid(account, binding) || !coreOAuthBindingMatchesProfile(binding, snapshot.Profile) ||
		len(serviceKey) < 32 || len(serviceKey) > 4096 || !utf8.ValidString(serviceKey) || strings.TrimSpace(serviceKey) != serviceKey || strings.IndexFunc(serviceKey, unicode.IsControl) >= 0 ||
		serviceKey == account.GetCredential("api_key") || serviceKey == account.GetCredential("core_oauth_control_key") || s.httpUpstream == nil || account.ProxyID != nil && account.Proxy == nil {
		return zero, ErrCoreOAuthInvalid
	}
	controlURL, _, err := coreWorkerURL(s, account, "", "")
	if err != nil {
		return zero, ErrCoreOAuthInvalid
	}
	base, err := url.Parse(controlURL)
	if err != nil || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || base.RawPath != "" || (base.Path != "" && path.Clean(base.Path) != base.Path) {
		return zero, ErrCoreOAuthInvalid
	}
	expected := managedfiles.Identity{BackendID: binding.BackendID, CredentialGeneration: binding.IdentityEpoch}
	files, err := NewBPSFilesBackend(BPSFilesBackendConfig{ControlURL: controlURL, ServiceKey: serviceKey, Expected: expected,
		ProxyURL: proxyURLForAccount(account), AccountID: account.ID, Concurrency: account.Concurrency}, s.httpUpstream, s.cfg)
	if err != nil {
		return zero, ErrCoreOAuthInvalid
	}
	description, err := files.DescribeIdentity(ctx)
	if err != nil || description.Contract != managedfiles.Contract || description.JSONProfile != managedfiles.JSONProfile ||
		description.Identity.BackendID != binding.BackendID || description.Identity.CredentialGeneration != binding.IdentityEpoch ||
		description.Identity.SubjectFingerprint == "" || !description.Capabilities.BatchAdmit || !description.Capabilities.EnsureUpload {
		return zero, ErrCoreOAuthUnavailable
	}
	return description.Identity, nil
}

func coreOAuthFilesServiceKey(account *Account, binding ManagedDeploymentBinding, snapshot *CoreOAuthCredentialSnapshot, existing *ManagedDeployment, provided string) (string, error) {
	if account == nil {
		return "", ErrManagedDeploymentInvalid
	}
	key := provided
	if key == "" {
		if snapshot == nil || existing == nil || existing.BPS != binding || !CoreOAuthDeclared(account) || validateCoreOAuthRuntimeOwnership(account, binding, snapshot) != nil {
			return "", ErrManagedDeploymentInvalid
		}
		identity := managedfiles.Identity{
			BackendID:            account.GetCredential("bps_files_backend_id"),
			SubjectFingerprint:   account.GetCredential("bps_files_subject_fingerprint"),
			CredentialGeneration: account.GetCredential("bps_files_credential_generation"),
		}
		key = account.GetCredential("bps_files_service_key")
		if account.GetCredential("bps_files_contract") != managedfiles.Contract || identity.BackendID != binding.BackendID || identity.CredentialGeneration != binding.IdentityEpoch || managedfiles.ValidateIdentity(identity) != nil {
			return "", ErrManagedDeploymentInvalid
		}
	}
	if len(key) < 32 || !bpsFilesOpaque(key, 4096) || key == account.GetCredential("api_key") || key == account.GetCredential("core_oauth_control_key") {
		return "", ErrManagedDeploymentInvalid
	}
	return key, nil
}

func validateCoreOAuthFilesIdentity(account *Account, discovered managedfiles.Identity) error {
	if account == nil {
		return ErrCoreOAuthInvalid
	}
	contract := account.GetCredential("bps_files_contract")
	if contract == "" {
		return nil
	}
	stored := managedfiles.Identity{
		BackendID:            account.GetCredential("bps_files_backend_id"),
		SubjectFingerprint:   account.GetCredential("bps_files_subject_fingerprint"),
		CredentialGeneration: account.GetCredential("bps_files_credential_generation"),
	}
	if contract != managedfiles.Contract || managedfiles.ValidateIdentity(stored) != nil || stored != discovered {
		return ErrCoreOAuthConflict
	}
	return nil
}

func proxyURLForAccount(account *Account) string {
	if account != nil && account.Proxy != nil {
		return account.Proxy.URL()
	}
	return ""
}

func prepareCoreOAuthFilesRuntimeUpdate(service *OpenAIGatewayService, account *Account, binding ManagedDeploymentBinding, snapshot *CoreOAuthCredentialSnapshot, filesControlURL, serviceKey string, identity managedfiles.Identity) (CoreOAuthRuntimeAccountUpdate, error) {
	if service == nil || account == nil || snapshot == nil || !coreWorkerBindingValid(account, binding) || !coreOAuthBindingMatchesProfile(binding, snapshot.Profile) ||
		identity.BackendID != binding.BackendID || identity.CredentialGeneration != binding.IdentityEpoch || managedfiles.ValidateIdentity(identity) != nil ||
		len(serviceKey) < 32 || len(serviceKey) > 4096 || !coreOAuthSecretText(serviceKey) || serviceKey == account.GetCredential("api_key") || serviceKey == account.GetCredential("core_oauth_control_key") {
		return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthInvalid
	}
	filesURL, _, err := coreWorkerURL(service, account, filesControlURL, account.GetCredential("core_oauth_control_key"))
	if err != nil {
		return CoreOAuthRuntimeAccountUpdate{}, ErrCoreOAuthInvalid
	}
	update, err := prepareCoreOAuthRuntimeUpdate(account, binding, snapshot)
	if err != nil {
		return CoreOAuthRuntimeAccountUpdate{}, err
	}
	update.Credentials["bps_files_contract"] = managedfiles.Contract
	update.Credentials["bps_files_control_url"] = filesURL
	update.Credentials["bps_files_service_key"] = serviceKey
	update.Credentials["bps_files_backend_id"] = identity.BackendID
	update.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	update.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	update.Credentials[openAIEndpointCapabilitiesCredentialKey] = []string{string(OpenAIEndpointCapabilityBPSFiles)}
	return update, nil
}
