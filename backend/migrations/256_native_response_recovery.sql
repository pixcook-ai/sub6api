-- Migration 256: permit authenticated native response recovery version 2.
-- Append-only schema change; never modify committed migration 255.
-- Reuses the existing private_recovery column and atomic UPDATE path.
ALTER TABLE gateway_stored_responses
    DROP CONSTRAINT gateway_stored_responses_recovery_version_check,
    ADD CONSTRAINT gateway_stored_responses_recovery_version_check
        CHECK (recovery_version IN (0,1,2));

ALTER TABLE gateway_stored_responses
    DROP CONSTRAINT gateway_response_recovery_state,
    ADD CONSTRAINT gateway_response_recovery_state CHECK (
        (private_recovery IS NULL OR (state='completed'
            AND COALESCE((jsonb_typeof(private_recovery)='object'
                AND ((recovery_version=1 AND private_recovery->'version'='1'::jsonb
                    AND private_recovery->>'kind'='text')
                 OR (recovery_version=2 AND private_recovery->'version'='2'::jsonb
                    AND private_recovery->>'kind'='native'))),false)))
        AND (state<>'completed' OR recovery_version=0 OR private_recovery IS NOT NULL)
        AND (state<>'deleted' OR (private_recovery IS NULL AND previous_response_id IS NULL))
    );
