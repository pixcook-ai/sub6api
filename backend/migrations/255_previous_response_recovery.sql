-- Existing rows intentionally remain display-only. Never infer replay material
-- from a previously persisted display object during an upgrade.
ALTER TABLE gateway_stored_responses
    ADD COLUMN recovery_version INTEGER NOT NULL DEFAULT 0 CHECK (recovery_version IN (0,1)),
    ADD COLUMN private_recovery JSONB,
    ADD COLUMN previous_response_id VARCHAR(64) CHECK (previous_response_id IS NULL OR previous_response_id ~ '^resp_local_[0-9a-f]{32}$');
ALTER TABLE gateway_stored_responses ADD CONSTRAINT gateway_response_recovery_state CHECK (
    (private_recovery IS NULL OR (recovery_version=1 AND state='completed'
        AND COALESCE((jsonb_typeof(private_recovery)='object'
        AND private_recovery->'version'='1'::jsonb
        AND private_recovery->>'kind'='text'),false)))
    AND (state<>'completed' OR recovery_version=0 OR private_recovery IS NOT NULL)
    AND (state<>'deleted' OR (private_recovery IS NULL AND previous_response_id IS NULL))
);
