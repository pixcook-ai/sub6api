-- Local durable Responses. Public access is scoped to user, API key and group.
-- Rollback means disabling the feature, not dropping retained user content.
CREATE TABLE gateway_stored_response_budgets (
    owner_id BIGINT PRIMARY KEY CHECK (owner_id >= 0),
    rows_count BIGINT NOT NULL DEFAULT 0 CHECK (rows_count >= 0),
    reserved_bytes BIGINT NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0)
);
-- owner 0 is the global lock and budget; real users always have positive IDs.
INSERT INTO gateway_stored_response_budgets (owner_id) VALUES (0);

CREATE TABLE gateway_stored_responses (
    public_id VARCHAR(64) PRIMARY KEY CHECK (public_id ~ '^resp_local_[0-9a-f]{32}$'),
    user_id BIGINT NOT NULL CHECK (user_id > 0),
    api_key_id BIGINT NOT NULL CHECK (api_key_id > 0),
    group_id BIGINT NOT NULL CHECK (group_id > 0),
    model VARCHAR(255) NOT NULL CHECK (model <> ''),
    resolved_model VARCHAR(255) NOT NULL,
    account_id BIGINT NOT NULL CHECK (account_id >= 0),
    binding VARCHAR(1024) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    state VARCHAR(16) NOT NULL CHECK (state IN ('in_progress','completed','incomplete','failed','deleted')),
    execution_state VARCHAR(16) NOT NULL CHECK (execution_state IN ('not_sent','accepted','unknown','terminal')),
    normalized_input JSONB,
    effective_parameters JSONB,
    public_response JSONB,
    content_bytes BIGINT NOT NULL CHECK (content_bytes >= 0),
    reserved_bytes BIGINT NOT NULL CHECK (reserved_bytes >= content_bytes),
    lease_token VARCHAR(128),
    lease_until TIMESTAMPTZ,
    version BIGINT NOT NULL CHECK (version > 0),
    terminal_hash VARCHAR(64) NOT NULL DEFAULT '',
    finalized_token_hash VARCHAR(64) NOT NULL DEFAULT '',
    deleted_at TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK ((state='in_progress' AND lease_token IS NOT NULL AND lease_until IS NOT NULL
            AND normalized_input IS NOT NULL AND effective_parameters IS NOT NULL AND public_response IS NOT NULL
            AND deleted_at IS NULL AND terminal_hash='' AND finalized_token_hash='')
        OR (state IN ('completed','incomplete','failed') AND lease_token IS NULL AND lease_until IS NULL
            AND normalized_input IS NOT NULL AND effective_parameters IS NOT NULL AND public_response IS NOT NULL
            AND deleted_at IS NULL AND terminal_hash ~ '^[0-9a-f]{64}$')
        OR (state='deleted' AND lease_token IS NULL AND lease_until IS NULL
            AND normalized_input IS NULL AND effective_parameters IS NULL AND public_response IS NULL
            AND content_bytes=0 AND reserved_bytes=0 AND binding='' AND account_id=0
            AND terminal_hash='' AND finalized_token_hash='' AND deleted_at IS NOT NULL)),
    CHECK (public_response IS NULL OR COALESCE((jsonb_typeof(public_response)='object'
        AND public_response->>'id'=public_id AND public_response->>'status'=state
        AND public_response->>'object'='response' AND public_response->'store'='true'::jsonb),false))
);
CREATE INDEX idx_gateway_stored_responses_owner ON gateway_stored_responses (user_id,api_key_id,group_id,public_id) WHERE state <> 'deleted';
CREATE INDEX idx_gateway_stored_responses_expiry ON gateway_stored_responses (expires_at,public_id);
CREATE INDEX idx_gateway_stored_responses_recovery ON gateway_stored_responses (lease_until,public_id) WHERE state='in_progress';
