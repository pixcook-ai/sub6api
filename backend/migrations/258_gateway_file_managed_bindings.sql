-- Remote operations are permanent evidence. No account/user cascade may erase
-- a possible or confirmed BPS copy; lifecycle cleanup retains the file row.
CREATE TABLE gateway_file_bindings (
    file_id BIGINT PRIMARY KEY REFERENCES gateway_files(id) ON DELETE RESTRICT,
    main_account_id BIGINT NOT NULL CHECK (main_account_id > 0),
    backend_id VARCHAR(256) NOT NULL,
    subject_fingerprint VARCHAR(256) NOT NULL,
    credential_generation VARCHAR(256) NOT NULL,
    active_operation_id VARCHAR(64),
    migration_id VARCHAR(64) CHECK (migration_id IS NULL),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    lease_token VARCHAR(128),
    lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK ((lease_token IS NULL) = (lease_until IS NULL))
);

CREATE TABLE gateway_file_remote_operations (
    operation_id VARCHAR(64) PRIMARY KEY,
    file_id BIGINT NOT NULL REFERENCES gateway_files(id) ON DELETE RESTRICT,
    file_version BIGINT NOT NULL CHECK (file_version > 0),
    main_account_id BIGINT NOT NULL CHECK (main_account_id > 0),
    backend_id VARCHAR(256) NOT NULL,
    subject_fingerprint VARCHAR(256) NOT NULL,
    credential_generation VARCHAR(256) NOT NULL,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('upload', 'recovery', 'migrate')),
    state VARCHAR(16) NOT NULL CHECK (state IN ('reserved', 'started', 'succeeded', 'failed', 'uncertain')),
    sha256 VARCHAR(64) NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    bytes BIGINT NOT NULL CHECK (bytes > 0),
    filename VARCHAR(255) NOT NULL,
    mime VARCHAR(255) NOT NULL,
    remote_id VARCHAR(256),
    input_tokens BIGINT,
    safe_error_code VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK ((state = 'succeeded' AND remote_id IS NOT NULL AND input_tokens IS NOT NULL AND input_tokens >= 0 AND completed_at IS NOT NULL)
        OR (state <> 'succeeded' AND remote_id IS NULL AND input_tokens IS NULL)),
    CHECK (state NOT IN ('started', 'uncertain', 'succeeded') OR started_at IS NOT NULL)
);
CREATE INDEX idx_gateway_file_operations_file ON gateway_file_remote_operations(file_id, created_at, operation_id);
CREATE UNIQUE INDEX idx_gateway_file_operation_unfinished ON gateway_file_remote_operations(file_id)
    WHERE state IN ('reserved', 'started', 'uncertain');

CREATE FUNCTION gateway_file_operation_keep_identity() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.operation_id, NEW.file_id, NEW.file_version, NEW.main_account_id, NEW.backend_id,
        NEW.subject_fingerprint, NEW.credential_generation, NEW.kind, NEW.sha256, NEW.bytes, NEW.filename, NEW.mime)
       IS DISTINCT FROM
       (OLD.operation_id, OLD.file_id, OLD.file_version, OLD.main_account_id, OLD.backend_id,
        OLD.subject_fingerprint, OLD.credential_generation, OLD.kind, OLD.sha256, OLD.bytes, OLD.filename, OLD.mime) THEN
        RAISE EXCEPTION 'remote operation identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF OLD.state = 'succeeded' AND (NEW.state, NEW.remote_id, NEW.input_tokens, NEW.completed_at)
       IS DISTINCT FROM (OLD.state, OLD.remote_id, OLD.input_tokens, OLD.completed_at) THEN
        RAISE EXCEPTION 'confirmed remote evidence is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER gateway_file_operation_identity_sticky
    BEFORE UPDATE ON gateway_file_remote_operations
    FOR EACH ROW EXECUTE FUNCTION gateway_file_operation_keep_identity();
