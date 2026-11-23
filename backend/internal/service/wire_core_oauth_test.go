package service

import (
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthStableKeyCapabilityFailsClosedWithoutStoppingSub6(t *testing.T) {
	for _, cfg := range []*config.Config{nil, {}} {
		core, err := ProvideCoreOAuthService(cfg, nil, nil, nil, nil)
		require.NoError(t, err)
		require.Nil(t, core)
	}
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = "startup-generated-key-is-not-durable-configuration"
	core, err := ProvideCoreOAuthService(cfg, nil, nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, core)
	cfg.Totp.EncryptionKeyConfigured = true
	core, err = ProvideCoreOAuthService(cfg, nil, nil, nil, nil)
	require.ErrorIs(t, err, ErrCoreOAuthUnavailable)
	require.Nil(t, core)
}
