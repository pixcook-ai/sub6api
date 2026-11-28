ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS pelican_config JSONB,
    ADD COLUMN IF NOT EXISTS pelican_running_until TIMESTAMPTZ;

ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS pelican_config JSONB;

CREATE INDEX IF NOT EXISTS idx_scheduled_test_results_pelican_created_at
    ON scheduled_test_results (created_at) WHERE pelican_config IS NOT NULL;
