package config

import (
	"strings"
	"testing"
)

func TestFilesConfigDisabledNeedsNoStorage(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.Enabled {
		t.Fatal("Files must be disabled by default")
	}
	if cfg.Root != "" {
		t.Fatal("Files storage requires an explicit persistent directory")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled Files must not require storage: %v", err)
	}
}

func TestFilesConfigEnabledRequiresBoundedPersistentStorage(t *testing.T) {
	valid := DefaultFilesConfig()
	valid.Enabled = true
	valid.Root = t.TempDir()
	if err := valid.Validate(); err != nil {
		t.Fatalf("explicit storage with finite quotas must be valid: %v", err)
	}

	for _, tc := range []struct {
		name string
		edit func(*FilesConfig)
		key  string
	}{
		{"missing root", func(c *FilesConfig) { c.Root = "" }, "root"},
		{"relative root", func(c *FilesConfig) { c.Root = "data/files" }, "root"},
		{"zero file limit", func(c *FilesConfig) { c.MaxFileBytes = 0 }, "max_file_bytes"},
		{"file over 200 MiB", func(c *FilesConfig) { c.MaxFileBytes = 209715201 }, "max_file_bytes"},
		{"zero user quota", func(c *FilesConfig) { c.UserQuotaBytes = 0 }, "user_quota_bytes"},
		{"user quota below file limit", func(c *FilesConfig) { c.UserQuotaBytes = 209715199 }, "user_quota_bytes"},
		{"zero global quota", func(c *FilesConfig) { c.GlobalQuotaBytes = 0 }, "global_quota_bytes"},
		{"global quota below user quota", func(c *FilesConfig) { c.GlobalQuotaBytes = 2147483647 }, "global_quota_bytes"},
		{"zero space reserve", func(c *FilesConfig) { c.FreeSpaceReserveBytes = 0 }, "free_space_reserve_bytes"},
		{"negative space reserve", func(c *FilesConfig) { c.FreeSpaceReserveBytes = -1 }, "free_space_reserve_bytes"},
		{"lease too short", func(c *FilesConfig) { c.LeaseSeconds = 59 }, "lease_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.edit(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "gateway.files."+tc.key) {
				t.Fatalf("want actionable %s validation error, got %v", tc.key, err)
			}
		})
	}
}

func TestLoadFilesStoragePolicy(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	files := cfg.Gateway.Files
	if files.Enabled || files.Root != "" || files.MaxFileBytes != 209715200 ||
		files.UserQuotaBytes != 2147483648 || files.GlobalQuotaBytes != 21474836480 ||
		files.FreeSpaceReserveBytes != 1073741824 || files.LeaseSeconds != 300 {
		t.Fatalf("unsafe or unexpected Files defaults: %+v", files)
	}

	t.Setenv("GATEWAY_FILES_ENABLED", "true")
	t.Setenv("GATEWAY_FILES_ROOT", t.TempDir())
	t.Setenv("GATEWAY_FILES_GLOBAL_QUOTA_BYTES", "0")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "gateway.files.global_quota_bytes") {
		t.Fatalf("enabled Files with explicit unlimited quota must fail startup: %v", err)
	}
}

func TestFilesConfigUploadConcurrencyIsBounded(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.UserConcurrentUploads != 2 || cfg.GlobalConcurrentUploads != 8 {
		t.Fatalf("unexpected upload concurrency defaults: %+v", cfg)
	}
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	for _, tc := range []struct {
		name string
		user int
		all  int
		key  string
	}{
		{"no user bound", 0, 8, "user_concurrent_uploads"},
		{"negative user bound", -1, 8, "user_concurrent_uploads"},
		{"no global bound", 2, 0, "global_concurrent_uploads"},
		{"global below user", 3, 2, "global_concurrent_uploads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			invalid.UserConcurrentUploads = tc.user
			invalid.GlobalConcurrentUploads = tc.all
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "gateway.files."+tc.key) {
				t.Fatalf("want %s validation error, got %v", tc.key, err)
			}
		})
	}
	cfg.GlobalConcurrentUploads = cfg.UserConcurrentUploads
	if err := cfg.Validate(); err != nil {
		t.Fatalf("equal positive user/global upload limits must be valid: %v", err)
	}
}

func TestFilesConfigUploadAndReadDeadlinesAreBounded(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.UploadTimeoutSeconds != 900 || cfg.ReadDrainSeconds != 900 {
		t.Fatalf("unexpected transfer deadlines: %+v", cfg)
	}
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	for _, tc := range []struct {
		name string
		edit func(*FilesConfig)
		key  string
	}{
		{"upload timeout too short", func(c *FilesConfig) { c.UploadTimeoutSeconds = 59 }, "upload_timeout_seconds"},
		{"upload timeout too long", func(c *FilesConfig) { c.UploadTimeoutSeconds = 86401 }, "upload_timeout_seconds"},
		{"read lease too long", func(c *FilesConfig) { c.LeaseSeconds = 3601 }, "lease_seconds"},
		{"drain shorter than lease", func(c *FilesConfig) { c.ReadDrainSeconds = 299 }, "read_drain_seconds"},
		{"drain too long", func(c *FilesConfig) { c.ReadDrainSeconds = 86401 }, "read_drain_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			tc.edit(&invalid)
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "gateway.files."+tc.key) {
				t.Fatalf("want %s validation error, got %v", tc.key, err)
			}
		})
	}
	for _, seconds := range []int{60, 86400} {
		valid := cfg
		valid.UploadTimeoutSeconds = seconds
		valid.LeaseSeconds = 3600
		valid.ReadDrainSeconds = 3600
		if err := valid.Validate(); err != nil {
			t.Fatalf("valid transfer deadline boundary rejected: %v", err)
		}
	}
}

func TestFilesConfigLifecycleWorkerIsBounded(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.LifecycleWorkerIntervalSeconds != 60 || cfg.LifecycleWorkerBatchSize != 100 || cfg.CleanupLeaseSeconds != 300 {
		t.Fatalf("unexpected lifecycle worker defaults: %+v", cfg)
	}
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	for _, tc := range []struct {
		name string
		edit func(*FilesConfig)
		key  string
	}{
		{"worker interval zero", func(c *FilesConfig) { c.LifecycleWorkerIntervalSeconds = 0 }, "lifecycle_worker_interval_seconds"},
		{"worker interval too long", func(c *FilesConfig) { c.LifecycleWorkerIntervalSeconds = 3601 }, "lifecycle_worker_interval_seconds"},
		{"worker batch zero", func(c *FilesConfig) { c.LifecycleWorkerBatchSize = 0 }, "lifecycle_worker_batch_size"},
		{"worker batch too large", func(c *FilesConfig) { c.LifecycleWorkerBatchSize = 1001 }, "lifecycle_worker_batch_size"},
		{"cleanup lease too short", func(c *FilesConfig) { c.CleanupLeaseSeconds = 59 }, "cleanup_lease_seconds"},
		{"cleanup lease too long", func(c *FilesConfig) { c.CleanupLeaseSeconds = 3601 }, "cleanup_lease_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			tc.edit(&invalid)
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "gateway.files."+tc.key) {
				t.Fatalf("want %s validation error, got %v", tc.key, err)
			}
		})
	}
	for _, maximum := range []bool{false, true} {
		valid := cfg
		valid.LifecycleWorkerIntervalSeconds = 1
		valid.LifecycleWorkerBatchSize = 1
		valid.CleanupLeaseSeconds = 60
		if maximum {
			valid.LifecycleWorkerIntervalSeconds = 3600
			valid.LifecycleWorkerBatchSize = 1000
			valid.CleanupLeaseSeconds = 3600
		}
		if err := valid.Validate(); err != nil {
			t.Fatalf("valid lifecycle worker boundary rejected: %v", err)
		}
	}
}

func TestFilesConfigDefaultRetentionPolicy(t *testing.T) {
	cfg := DefaultFilesConfig()
	if cfg.DefaultRetentionSeconds != 0 {
		t.Fatalf("non-batch files must not expire by default: %+v", cfg)
	}
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	for _, seconds := range []int64{-1, 1, 3599, 2592001} {
		invalid := cfg
		invalid.DefaultRetentionSeconds = seconds
		if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "gateway.files.default_retention_seconds") {
			t.Fatalf("retention %d must fail with actionable error, got %v", seconds, err)
		}
	}
	for _, seconds := range []int64{0, 3600, 2592000} {
		valid := cfg
		valid.DefaultRetentionSeconds = seconds
		if err := valid.Validate(); err != nil {
			t.Fatalf("retention %d must be valid: %v", seconds, err)
		}
	}
	if err := (FilesConfig{}).Validate(); err != nil {
		t.Fatalf("disabled Files must not require new lifecycle configuration: %v", err)
	}
}

func TestLoadFilesLifecyclePolicy(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	files := cfg.Gateway.Files
	if files.UserConcurrentUploads != 2 || files.GlobalConcurrentUploads != 8 ||
		files.UploadTimeoutSeconds != 900 || files.ReadDrainSeconds != 900 ||
		files.LifecycleWorkerIntervalSeconds != 60 || files.LifecycleWorkerBatchSize != 100 ||
		files.CleanupLeaseSeconds != 300 || files.DefaultRetentionSeconds != 0 {
		t.Fatalf("missing lifecycle defaults in loaded config: %+v", files)
	}

	for key, value := range map[string]string{
		"GATEWAY_FILES_ENABLED":                           "true",
		"GATEWAY_FILES_ROOT":                              t.TempDir(),
		"GATEWAY_FILES_USER_CONCURRENT_UPLOADS":           "3",
		"GATEWAY_FILES_GLOBAL_CONCURRENT_UPLOADS":         "9",
		"GATEWAY_FILES_UPLOAD_TIMEOUT_SECONDS":            "1200",
		"GATEWAY_FILES_LIFECYCLE_WORKER_INTERVAL_SECONDS": "30",
		"GATEWAY_FILES_LIFECYCLE_WORKER_BATCH_SIZE":       "50",
		"GATEWAY_FILES_CLEANUP_LEASE_SECONDS":             "120",
		"GATEWAY_FILES_READ_DRAIN_SECONDS":                "600",
		"GATEWAY_FILES_DEFAULT_RETENTION_SECONDS":         "7200",
	} {
		t.Setenv(key, value)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatalf("load enabled lifecycle settings: %v", err)
	}
	files = cfg.Gateway.Files
	if !files.Enabled || files.UserConcurrentUploads != 3 || files.GlobalConcurrentUploads != 9 ||
		files.UploadTimeoutSeconds != 1200 || files.ReadDrainSeconds != 600 ||
		files.LifecycleWorkerIntervalSeconds != 30 || files.LifecycleWorkerBatchSize != 50 ||
		files.CleanupLeaseSeconds != 120 || files.DefaultRetentionSeconds != 7200 {
		t.Fatalf("explicit lifecycle settings were not preserved: %+v", files)
	}

	t.Setenv("GATEWAY_FILES_DEFAULT_RETENTION_SECONDS", "-1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "gateway.files.default_retention_seconds") {
		t.Fatalf("invalid explicit lifecycle settings must fail startup: %v", err)
	}
}
