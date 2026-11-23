-- Controller credentials are independent from administrator/inference keys.
-- Retain revoked hashes and operation IDs so retries cannot restore old keys.
CREATE TABLE managed_oauth_controller_keys (
 account_id BIGINT NOT NULL REFERENCES accounts(id),
 operation_id UUID NOT NULL CHECK (operation_id <> '00000000-0000-0000-0000-000000000000'::uuid),
 key_hash TEXT NOT NULL UNIQUE CHECK (key_hash ~ '^[0-9a-f]{64}$'),
 identity JSONB NOT NULL CHECK (jsonb_typeof(identity) = 'object'),
 revoked_at TIMESTAMPTZ,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
 PRIMARY KEY (account_id,operation_id)
);
