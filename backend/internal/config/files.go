package config

import (
	"fmt"
	"path/filepath"
)

type FilesBackendMigrationConfig struct {
	Enabled bool `mapstructure:"enabled"`
}

// FilesConfig bounds the private, persistent storage used by the Files API.
type FilesConfig struct {
	BackendMigration               FilesBackendMigrationConfig `mapstructure:"backend_migration"`
	Enabled                        bool                        `mapstructure:"enabled"`
	Root                           string                      `mapstructure:"root"`
	MaxFileBytes                   int64                       `mapstructure:"max_file_bytes"`
	UserQuotaBytes                 int64                       `mapstructure:"user_quota_bytes"`
	GlobalQuotaBytes               int64                       `mapstructure:"global_quota_bytes"`
	FreeSpaceReserveBytes          int64                       `mapstructure:"free_space_reserve_bytes"`
	LeaseSeconds                   int                         `mapstructure:"lease_seconds"`
	UserConcurrentUploads          int                         `mapstructure:"user_concurrent_uploads"`
	GlobalConcurrentUploads        int                         `mapstructure:"global_concurrent_uploads"`
	UploadTimeoutSeconds           int                         `mapstructure:"upload_timeout_seconds"`
	ReadDrainSeconds               int                         `mapstructure:"read_drain_seconds"`
	LifecycleWorkerIntervalSeconds int                         `mapstructure:"lifecycle_worker_interval_seconds"`
	LifecycleWorkerBatchSize       int                         `mapstructure:"lifecycle_worker_batch_size"`
	CleanupLeaseSeconds            int                         `mapstructure:"cleanup_lease_seconds"`
	DefaultRetentionSeconds        int64                       `mapstructure:"default_retention_seconds"`
}

func DefaultFilesConfig() FilesConfig {
	return FilesConfig{
		MaxFileBytes:                   200 << 20,
		UserQuotaBytes:                 2 << 30,
		GlobalQuotaBytes:               20 << 30,
		FreeSpaceReserveBytes:          1 << 30,
		LeaseSeconds:                   300,
		UserConcurrentUploads:          2,
		GlobalConcurrentUploads:        8,
		UploadTimeoutSeconds:           900,
		ReadDrainSeconds:               900,
		LifecycleWorkerIntervalSeconds: 60,
		LifecycleWorkerBatchSize:       100,
		CleanupLeaseSeconds:            300,
	}
}

func (c FilesConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Root == "" || !filepath.IsAbs(c.Root) {
		return fmt.Errorf("gateway.files.root must be an explicit absolute persistent directory")
	}
	if c.MaxFileBytes <= 0 || c.MaxFileBytes > 200<<20 {
		return fmt.Errorf("gateway.files.max_file_bytes must be positive and no greater than 200 MiB")
	}
	if c.UserQuotaBytes < c.MaxFileBytes {
		return fmt.Errorf("gateway.files.user_quota_bytes must be finite and no less than max_file_bytes")
	}
	if c.GlobalQuotaBytes < c.UserQuotaBytes {
		return fmt.Errorf("gateway.files.global_quota_bytes must be finite and no less than user_quota_bytes")
	}
	if c.FreeSpaceReserveBytes <= 0 {
		return fmt.Errorf("gateway.files.free_space_reserve_bytes must be positive")
	}
	if c.LeaseSeconds < 60 || c.LeaseSeconds > 3600 {
		return fmt.Errorf("gateway.files.lease_seconds must be between 60 and 3600")
	}
	if c.UserConcurrentUploads <= 0 {
		return fmt.Errorf("gateway.files.user_concurrent_uploads must be positive")
	}
	if c.GlobalConcurrentUploads < c.UserConcurrentUploads {
		return fmt.Errorf("gateway.files.global_concurrent_uploads must be no less than user_concurrent_uploads")
	}
	if c.UploadTimeoutSeconds < 60 || c.UploadTimeoutSeconds > 86400 {
		return fmt.Errorf("gateway.files.upload_timeout_seconds must be between 60 and 86400")
	}
	if c.ReadDrainSeconds < c.LeaseSeconds || c.ReadDrainSeconds > 86400 {
		return fmt.Errorf("gateway.files.read_drain_seconds must be no less than lease_seconds and no greater than 86400")
	}
	if c.LifecycleWorkerIntervalSeconds < 1 || c.LifecycleWorkerIntervalSeconds > 3600 {
		return fmt.Errorf("gateway.files.lifecycle_worker_interval_seconds must be between 1 and 3600")
	}
	if c.LifecycleWorkerBatchSize < 1 || c.LifecycleWorkerBatchSize > 1000 {
		return fmt.Errorf("gateway.files.lifecycle_worker_batch_size must be between 1 and 1000")
	}
	if c.CleanupLeaseSeconds < 60 || c.CleanupLeaseSeconds > 3600 {
		return fmt.Errorf("gateway.files.cleanup_lease_seconds must be between 60 and 3600")
	}
	if c.DefaultRetentionSeconds != 0 && (c.DefaultRetentionSeconds < 3600 || c.DefaultRetentionSeconds > 2592000) {
		return fmt.Errorf("gateway.files.default_retention_seconds must be 0 or between 3600 and 2592000")
	}
	return nil
}
