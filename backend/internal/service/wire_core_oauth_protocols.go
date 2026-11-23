//go:build !coreoauthfixture

package service

// Normal builds always use the fixed official provider contracts.
func newCoreOAuthRuntimeService(repo CoreOAuthRepository, encryptor SecretEncryptor, codex *OpenAIOAuthService, proxies ProxyRepository) (*CoreOAuthService, error) {
    return NewCoreOAuthService(repo, encryptor, map[string]CoreOAuthProtocol{
        CoreOAuthProviderCodex: NewCoreOAuthCodexProtocol(codex),
        CoreOAuthProviderExcel: NewCoreOAuthBPSProtocol(),
    }, proxies)
}
