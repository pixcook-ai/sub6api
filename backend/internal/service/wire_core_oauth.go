package service

import "github.com/MACOS-DO/sub4api/internal/config"

// A startup-generated TOTP key cannot protect durable rotating refresh tokens.
// Keep existing Sub6 features available while the new capability fails closed.
func ProvideCoreOAuthService(cfg *config.Config, repo CoreOAuthRepository, encryptor SecretEncryptor, codex *OpenAIOAuthService, proxies ProxyRepository) (*CoreOAuthService, error) {
	if cfg == nil || !cfg.Totp.EncryptionKeyConfigured {
		return nil, nil
	}
	return newCoreOAuthRuntimeService(repo, encryptor, codex, proxies)
}
