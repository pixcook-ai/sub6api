-- Freeze current operation explicitly; historical and pending copies are evidence.
ALTER TABLE gateway_file_bindings ADD COLUMN current_operation_id VARCHAR(64);

DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM gateway_file_bindings b
  LEFT JOIN gateway_file_remote_operations o ON o.operation_id=b.active_operation_id
  WHERE b.active_operation_id IS NOT NULL AND
   (o.operation_id IS NULL OR o.state<>'succeeded' OR
    (o.file_id,o.main_account_id,o.backend_id,o.subject_fingerprint,o.credential_generation)
    IS DISTINCT FROM
    (b.file_id,b.main_account_id,b.backend_id,b.subject_fingerprint,b.credential_generation))
 ) THEN RAISE EXCEPTION 'invalid legacy active operation' USING ERRCODE='23514'; END IF;
END $$;

-- Existing recovery always cleared active before allocating its new operation.
-- Keep that selected identity and prioritize actual active evidence. Pending
-- migrations never choose the active mapping, even with matching identity.
UPDATE gateway_file_bindings b SET current_operation_id=COALESCE(b.active_operation_id,(
 SELECT o.operation_id FROM gateway_file_remote_operations o
 WHERE (o.file_id,o.main_account_id,o.backend_id,o.subject_fingerprint,o.credential_generation)
   = (b.file_id,b.main_account_id,b.backend_id,b.subject_fingerprint,b.credential_generation)
 AND o.kind IN ('upload','recovery') AND o.state IN ('reserved','started','uncertain','succeeded')
 ORDER BY (o.state IN ('reserved','started','uncertain')) DESC,
          o.created_at DESC,o.operation_id DESC LIMIT 1
));

-- Historical ties without an active operation are ambiguous. Do not choose
-- by randomized operation IDs when two confirmed candidates share a clock.
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM gateway_file_bindings b
  JOIN gateway_file_remote_operations selected ON selected.operation_id=b.current_operation_id
  JOIN gateway_file_remote_operations candidate ON candidate.file_id=b.file_id
   AND candidate.main_account_id=b.main_account_id AND candidate.backend_id=b.backend_id
   AND candidate.subject_fingerprint=b.subject_fingerprint
   AND candidate.credential_generation=b.credential_generation
   AND candidate.kind IN ('upload','recovery') AND candidate.state='succeeded'
   AND candidate.created_at=selected.created_at AND candidate.operation_id<>selected.operation_id
  WHERE b.active_operation_id IS NULL AND selected.state='succeeded'
 ) THEN RAISE EXCEPTION 'ambiguous legacy current operation' USING ERRCODE='23514'; END IF;
END $$;

CREATE FUNCTION gateway_file_binding_assert_current(target_file BIGINT)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE b gateway_file_bindings%ROWTYPE; o gateway_file_remote_operations%ROWTYPE;
BEGIN
 SELECT * INTO b FROM gateway_file_bindings WHERE file_id=target_file;
 IF NOT FOUND THEN RETURN; END IF;
 SELECT * INTO o FROM gateway_file_remote_operations WHERE operation_id=b.current_operation_id;
 IF NOT FOUND OR (o.file_id,o.main_account_id,o.backend_id,o.subject_fingerprint,o.credential_generation)
  IS DISTINCT FROM (b.file_id,b.main_account_id,b.backend_id,b.subject_fingerprint,b.credential_generation) THEN
  RAISE EXCEPTION 'managed current operation identity mismatch' USING ERRCODE='23514';
 END IF;
 IF b.active_operation_id IS NOT NULL AND (b.active_operation_id<>b.current_operation_id OR o.state<>'succeeded') THEN
  RAISE EXCEPTION 'managed active operation is not current confirmed copy' USING ERRCODE='23514';
 END IF;
 IF EXISTS (
  SELECT 1 FROM gateway_file_remote_operations pending
  WHERE (pending.file_id,pending.main_account_id,pending.backend_id,pending.subject_fingerprint,pending.credential_generation)
   = (b.file_id,b.main_account_id,b.backend_id,b.subject_fingerprint,b.credential_generation)
  AND pending.kind IN ('upload','recovery') AND pending.state IN ('reserved','started','uncertain')
  AND pending.operation_id<>b.current_operation_id
 ) THEN RAISE EXCEPTION 'managed upload or recovery must be current' USING ERRCODE='23514'; END IF;
END $$;

DO $$ DECLARE binding_file BIGINT;
BEGIN
 FOR binding_file IN SELECT file_id FROM gateway_file_bindings LOOP
  PERFORM gateway_file_binding_assert_current(binding_file);
 END LOOP;
END $$;

ALTER TABLE gateway_file_bindings ADD CONSTRAINT gateway_file_binding_current_operation_fk
 FOREIGN KEY(current_operation_id) REFERENCES gateway_file_remote_operations(operation_id)
 DEFERRABLE INITIALLY DEFERRED;

CREATE FUNCTION gateway_file_binding_validate_current() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  PERFORM gateway_file_binding_assert_current(OLD.file_id);
 ELSE
  PERFORM gateway_file_binding_assert_current(NEW.file_id);
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER gateway_file_binding_current_valid
 AFTER INSERT OR UPDATE ON gateway_file_bindings DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION gateway_file_binding_validate_current();
CREATE CONSTRAINT TRIGGER gateway_file_operation_current_valid
 AFTER INSERT OR UPDATE OR DELETE ON gateway_file_remote_operations DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION gateway_file_binding_validate_current();
