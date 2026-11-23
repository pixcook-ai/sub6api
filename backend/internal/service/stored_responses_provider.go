package service

import "github.com/MACOS-DO/sub4api/internal/config"

func ProvideStoredResponseService(repo StoredResponseRepository, cfg *config.Config) (*StoredResponseService, error) {
	if cfg == nil {
		return nil, ErrStoredResponseInvalid
	}
	c := cfg.Gateway.StoredResponses
	return NewStoredResponseService(repo, StoredResponseConfig{
		Enabled: c.Enabled, RetentionDays: c.RetentionDays, LeaseSeconds: c.LeaseSeconds,
		Limits: StoredResponseLimits{MaxResponseBytes: c.MaxResponseBytes, UserMaxRows: c.UserMaxRows,
			UserMaxBytes: c.UserMaxBytes, GlobalMaxRows: c.GlobalMaxRows, GlobalMaxBytes: c.GlobalMaxBytes},
	})
}
