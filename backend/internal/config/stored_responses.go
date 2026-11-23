package config

import "fmt"

// StoredResponsesConfig bounds local display storage independently of upstream
// provider storage. It remains opt-in even for explicitly capable BPS accounts.
type StoredResponsesConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// DefaultStore applies only to explicitly local BPS response pools. Nil
	// preserves the standard default (true) for existing configurations.
	DefaultStore                   *bool `mapstructure:"default_store"`
	NativePreviousResponsesEnabled bool  `mapstructure:"native_previous_responses_enabled"`
	RetentionDays                  int   `mapstructure:"retention_days"`
	LeaseSeconds                   int   `mapstructure:"lease_seconds"`
	MaxResponseBytes               int64 `mapstructure:"max_response_bytes"`
	UserMaxRows                    int64 `mapstructure:"user_max_rows"`
	UserMaxBytes                   int64 `mapstructure:"user_max_bytes"`
	GlobalMaxRows                  int64 `mapstructure:"global_max_rows"`
	GlobalMaxBytes                 int64 `mapstructure:"global_max_bytes"`
}

func (c StoredResponsesConfig) LocalDefaultStore() bool {
	return c.DefaultStore == nil || *c.DefaultStore
}

func DefaultStoredResponsesConfig() StoredResponsesConfig {
	return StoredResponsesConfig{RetentionDays: 30, LeaseSeconds: 120, MaxResponseBytes: 1 << 20, UserMaxRows: 1000, UserMaxBytes: 64 << 20, GlobalMaxRows: 100000, GlobalMaxBytes: 1 << 30}
}

func (c StoredResponsesConfig) Validate() error {
	if c.RetentionDays < 1 || c.RetentionDays > 30 {
		return fmt.Errorf("gateway.stored_responses.retention_days must be between 1 and 30")
	}
	if c.LeaseSeconds < 6 || c.LeaseSeconds > 3600 {
		return fmt.Errorf("gateway.stored_responses.lease_seconds must be between 6 and 3600")
	}
	if c.MaxResponseBytes < 1 || c.MaxResponseBytes > 64<<20 {
		return fmt.Errorf("gateway.stored_responses.max_response_bytes must be between 1 and 64 MiB")
	}
	if c.UserMaxRows < 1 {
		return fmt.Errorf("gateway.stored_responses.user_max_rows must be positive")
	}
	if c.GlobalMaxRows < c.UserMaxRows {
		return fmt.Errorf("gateway.stored_responses.global_max_rows must be no less than user_max_rows")
	}
	if c.UserMaxBytes < c.MaxResponseBytes {
		return fmt.Errorf("gateway.stored_responses.user_max_bytes must be no less than max_response_bytes")
	}
	if c.GlobalMaxBytes < c.UserMaxBytes {
		return fmt.Errorf("gateway.stored_responses.global_max_bytes must be no less than user_max_bytes")
	}
	return nil
}
