-- Request-private upload evidence deliberately has no gateway_files row or FK.
CREATE TABLE gateway_inline_requests (
    request_id VARCHAR(256) PRIMARY KEY,
    user_id BIGINT NOT NULL CHECK (user_id > 0),
    api_key_id BIGINT NOT NULL CHECK (api_key_id > 0),
    group_id BIGINT NOT NULL CHECK (group_id > 0),
    main_account_id BIGINT NOT NULL CHECK (main_account_id > 0),
    backend_id VARCHAR(256) NOT NULL,
    subject_fingerprint VARCHAR(256) NOT NULL,
    credential_generation VARCHAR(256) NOT NULL,
    lease_token VARCHAR(128) NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    released BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE gateway_inline_remote_operations (
    operation_id VARCHAR(64) PRIMARY KEY,
    request_id VARCHAR(256) NOT NULL REFERENCES gateway_inline_requests(request_id) ON DELETE RESTRICT,
    file_ref VARCHAR(256) NOT NULL,
    sha256 VARCHAR(64) NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
    bytes BIGINT NOT NULL CHECK (bytes > 0),
    filename VARCHAR(1024) NOT NULL,
    mime VARCHAR(1024) NOT NULL,
    state VARCHAR(16) NOT NULL CHECK (state IN ('reserved', 'started', 'uncertain', 'succeeded')),
    remote_id VARCHAR(256),
    input_tokens BIGINT,
    safe_error_code VARCHAR(64),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    UNIQUE (request_id, file_ref),
    CHECK ((state = 'succeeded' AND remote_id IS NOT NULL AND input_tokens IS NOT NULL AND input_tokens >= 0 AND completed_at IS NOT NULL)
        OR (state <> 'succeeded' AND remote_id IS NULL AND input_tokens IS NULL AND completed_at IS NULL)),
    CHECK ((state = 'reserved') = (started_at IS NULL))
);

CREATE FUNCTION gateway_inline_keep_facts() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'gateway_inline_requests' THEN
        IF (to_jsonb(NEW)-'released') IS DISTINCT FROM (to_jsonb(OLD)-'released') OR (OLD.released AND NOT NEW.released) THEN
            RAISE EXCEPTION 'inline request identity and lease are immutable' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF (NEW.operation_id,NEW.request_id,NEW.file_ref,NEW.sha256,NEW.bytes,NEW.filename,NEW.mime)
            IS DISTINCT FROM (OLD.operation_id,OLD.request_id,OLD.file_ref,OLD.sha256,OLD.bytes,OLD.filename,OLD.mime) THEN
            RAISE EXCEPTION 'inline operation identity is immutable' USING ERRCODE = '23514';
        END IF;
        IF OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at THEN
            RAISE EXCEPTION 'inline upload cannot restart' USING ERRCODE = '23514';
        END IF;
        IF OLD.state = 'succeeded' AND NEW IS DISTINCT FROM OLD THEN
            RAISE EXCEPTION 'confirmed inline evidence is immutable' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER gateway_inline_request_facts BEFORE UPDATE ON gateway_inline_requests
    FOR EACH ROW EXECUTE FUNCTION gateway_inline_keep_facts();
CREATE TRIGGER gateway_inline_operation_facts BEFORE UPDATE ON gateway_inline_remote_operations
    FOR EACH ROW EXECUTE FUNCTION gateway_inline_keep_facts();
