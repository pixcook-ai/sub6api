#!/usr/bin/env python3
"""Exercise migrated Files schema and actual SQL in disposable PostgreSQL.

Run from backend: rtk proxy python3 internal/repository/file_core_postgres.py
Go integration tests separately verify repository control flow.
"""

import json
from pathlib import Path
import re
import subprocess
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
CONTAINER = "sub6-files-core-verify-" + uuid.uuid4().hex[:8]
SOURCES = {name: (ROOT / "internal/repository" / name).read_text() for name in ("file_repo.go", "file_lifecycle_repo.go", "file_delete_repo.go", "file_purge_repo.go")}
COLUMNS = re.search(r"const fileColumns = `([^`]+)`", SOURCES["file_repo.go"])[1]


def query(file, prefix, **variables):
    expression = r"`[^`]+`(?:\s*\+\s*[A-Za-z_]\w*(?:\s*\+\s*`[^`]*`)?)*"
    candidates = [m[0] for m in re.finditer(expression, SOURCES[file]) if m[0].startswith("`" + prefix)]
    assert len(candidates) == 1, (file, prefix, candidates)
    parts = re.split(r"(`[^`]*`)", candidates[0])
    result = ""
    for part in parts:
        if part.startswith("`"):
            result += part[1:-1]
        elif part.strip():
            variable = part.strip().strip("+").strip()
            result += {"fileColumns": COLUMNS, **variables}[variable]
    return result


def command(args, **kwargs):
    return subprocess.run(["rtk", "proxy", *args], text=True, capture_output=True, **kwargs)


def quote(value):
    if value is None:
        return "NULL"
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, dict):
        value = json.dumps(value, separators=(",", ":"))
    return "'" + str(value).replace("'", "''") + "'"


def render(statement, params):
    return re.sub(r"\$([0-9]+)", lambda m: quote(params[int(m[1]) - 1]), statement)


def sql(statement, check=True):
    r = command(["docker", "exec", "-i", CONTAINER, "psql", "-U", "postgres", "-d", "files", "-qAt", "-v", "ON_ERROR_STOP=1"], input=statement, timeout=25)
    if check and r.returncode:
        raise AssertionError(r.stderr)
    return r


def rows(result):
    return [line for line in result.stdout.splitlines() if line.strip()]


def ready():
    for _ in range(100):
        logs = command(["docker", "logs", CONTAINER], timeout=5)
        if "PostgreSQL init process complete; ready for start up." in logs.stdout + logs.stderr and sql("SELECT 1;", False).returncode == 0:
            return
        time.sleep(0.1)
    raise AssertionError("isolated PostgreSQL did not initialize")


def create(number, user=7, key=11, group=42, state="ready", expires="NULL"):
    pid = "file-" + format(number, "032x")
    lease = "NULL,NULL" if state == "ready" else "'upload-token',clock_timestamp()+interval '1 hour'"
    sql("INSERT INTO gateway_files(public_id,user_id,api_key_id,group_id,storage_key,filename,purpose,mime,state,bytes,reserved_bytes,sha256,upload_token,upload_lease_until,expires_at) VALUES(" + ",".join(map(quote, [pid, user, key, group, format(number, "032x"), "private.txt", "user_data", "text/plain", state, 6 if state == "ready" else 0, 6, "a" * 64])) + "," + lease + "," + expires + " );")
    return pid


def main():
    checks = []
    try:
        result = command(["docker", "run", "--rm", "-d", "--name", CONTAINER, "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=files", "postgres:18-alpine"], timeout=20)
        assert result.returncode == 0, result.stderr
        ready()
        # Parent identity rows are fixtures; deployment ownership itself uses
        # the unchanged migration and immutable full binding records.
        sql("CREATE TABLE groups(id bigint PRIMARY KEY); CREATE TABLE users(id bigint PRIMARY KEY); CREATE TABLE api_keys(id bigint PRIMARY KEY);" + (ROOT / "migrations/251_managed_deployments.sql").read_text())
        sql("INSERT INTO groups VALUES(42),(43); INSERT INTO users VALUES(7),(8); INSERT INTO api_keys VALUES(11),(12);")
        for name in ("253_gateway_files.sql", "258_gateway_file_managed_bindings.sql", "260_managed_file_current_operation.sql"):
            sql((ROOT / "migrations" / name).read_text())
        checks.append("ordered_files_and_managed_ledger_schema")
        own = create(1)
        other_key = create(2, key=12)
        other_user = create(3, user=8)
        other_group = create(4, group=43)
        expired = create(5, expires="clock_timestamp()-interval '1 second'")
        own_new = create(6)
        for comparison, direction in (("<", "DESC"), (">", "ASC")):
            listing = query("file_lifecycle_repo.go", "WITH bounds AS MATERIALIZED", comparison=comparison, direction=direction)
            listing = "SELECT public_id FROM (" + render(listing, [7, 42, "", "", "2000-01-01", 0, 100, 11, "", {}]) + ") AS listed;"
            assert set(rows(sql(listing))) == {own, own_new}
        checks.append("page_filters_full_user_key_group_and_expiry_before_limit")
        cursor = query("file_lifecycle_repo.go", "SELECT created_at,id FROM gateway_files")
        for pid in (other_key, other_user, other_group):
            assert not rows(sql(render(cursor, [pid, 7, 42, "", 11, "", {}]) + ";"))
        assert rows(sql(render(cursor, [own, 7, 42, "", 11, "", {}]) + ";"))
        checks.append("cursor_cannot_probe_other_user_key_or_group")
        deployment = "deployment_" + str(uuid.uuid4())
        binding = {"provider": "bps_excel", "account_id": 17, "backend_id": "worker-a", "binding_ref": "bind_" + str(uuid.uuid4()), "profile_ref": "oauth_" + str(uuid.uuid4()), "identity_epoch": str(uuid.uuid4()), "workspace_id": "workspace-a", "subject": "subject-a"}
        codex = {**binding, "provider": "codex", "account_id": 18, "backend_id": "worker-b"}
        epoch = {**binding, "identity_epoch": str(uuid.uuid4())}
        sql("INSERT INTO managed_deployments(deployment_id,group_id,route_revision,selected_provider,bps_binding,codex_binding) VALUES(" + quote(deployment) + ",42,1,'bps_excel'," + quote(binding) + "," + quote(codex) + " );")
        changed_epoch, unbound = create(8), create(9)
        for session, identity, pid in (("old", binding, own), ("new", codex, own_new), ("epoch", epoch, changed_epoch)):
            sql("INSERT INTO managed_deployment_sessions(deployment_id,user_id,api_key_id,session_key,route_revision,binding) VALUES(" + quote(deployment) + ",7,11," + quote(session) + ",1," + quote(identity) + " );INSERT INTO managed_deployment_resources(deployment_id,user_id,api_key_id,resource_kind,resource_hash,session_key) VALUES(" + quote(deployment) + ",7,11,'file',encode(sha256(convert_to(" + quote(pid) + ",'UTF8')),'hex')," + quote(session) + " );")
        listing = query("file_lifecycle_repo.go", "WITH bounds AS MATERIALIZED", comparison="<", direction="DESC")
        for identity, pid in ((binding, own), (codex, own_new), (epoch, changed_epoch)):
            assert rows(sql("SELECT public_id FROM (" + render(listing, [7, 42, "", "", "2000-01-01", 0, 1, 11, deployment, identity]) + ") AS listed;")) == [pid]
        for field in binding:
            changed = {**binding, field: binding[field] + 1 if field == "account_id" else str(uuid.uuid4())}
            assert not rows(sql(render(listing, [7, 42, "", "", "2000-01-01", 0, 100, 11, deployment, changed]) + ";"))
        assert not rows(sql(render(cursor, [own_new, 7, 42, "", 11, deployment, binding]) + ";"))
        assert not rows(sql(render(cursor, [unbound, 7, 42, "", 11, deployment, binding]) + ";"))
        assert rows(sql(render(cursor, [own, 7, 42, "", 11, deployment, binding]) + ";"))
        checks.append("managed_page_and_cursor_fence_every_binding_field_before_limit")
        locked = query("file_delete_repo.go", "SELECT `")
        for pid in (other_key, other_user, other_group):
            assert not rows(sql(render(locked, [pid, 7, 42, 11]) + ";"))
        deletion = query("file_delete_repo.go", "UPDATE gateway_files SET state='retired'")
        file_id = int(rows(sql("SELECT id FROM gateway_files WHERE public_id=" + quote(own) + ";"))[0])
        now = rows(sql("SELECT clock_timestamp();"))[0]
        sql(render(deletion, [file_id, now, "2099-01-01"]) + ";")
        assert rows(sql("SELECT state,retire_reason FROM gateway_files WHERE id=" + str(file_id) + ";")) == ["retired|deleted"]
        checks.append("delete_row_lock_is_exact_owner_and_preserves_tombstone")
        purge = query("file_purge_repo.go", "UPDATE gateway_files SET local_purged_at")
        sql("UPDATE gateway_files SET cleanup_token='cleanup',cleanup_lease_until=clock_timestamp()+interval '1 hour' WHERE id=" + str(file_id) + ";INSERT INTO gateway_file_read_leases VALUES(" + str(file_id) + ",'reader',clock_timestamp()+interval '1 hour');")
        assert not rows(sql(render(purge, [file_id, "cleanup"]) + " RETURNING id;"))
        sql("DELETE FROM gateway_file_read_leases WHERE file_id=" + str(file_id) + ";")
        assert rows(sql(render(purge, [file_id, "cleanup"]) + " RETURNING id;")) == [str(file_id)]
        assert rows(sql("SELECT reserved_bytes,local_purged_at IS NOT NULL FROM gateway_files WHERE id=" + str(file_id) + ";")) == ["0|t"]
        checks.append("purge_requires_read_lease_drain_before_budget_release")
        staged = create(7, state="staging")
        staged_id = int(rows(sql("SELECT id FROM gateway_files WHERE public_id=" + quote(staged) + ";"))[0])
        commit = query("file_repo.go", "UPDATE gateway_files SET state='ready'")
        params = [staged_id, "wrong-token", 6, "b" * 64, "user_data", None, "safe.txt", "text/plain"]
        assert not rows(sql(render(commit, params) + ";"))
        params[1] = "upload-token"
        assert rows(sql(render(commit, params) + ";"))
        checks.append("upload_publish_requires_live_token_and_lease")
        sql("UPDATE gateway_files SET remote_activity_possible=true WHERE id=" + str(staged_id) + ";")
        blocked = sql("UPDATE gateway_files SET remote_activity_possible=false WHERE id=" + str(staged_id) + ";", False)
        assert blocked.returncode != 0 and "remote activity evidence cannot be cleared" in blocked.stderr
        checks.append("remote_activity_evidence_is_monotonic")
        command(["docker", "restart", CONTAINER], timeout=20)
        ready()
        assert rows(sql("SELECT reserved_bytes,local_purged_at IS NOT NULL FROM gateway_files WHERE id=" + str(file_id) + ";")) == ["0|t"]
        checks.append("restart_preserves_owner_and_cleanup_tombstone")
        print(json.dumps({"postgres": "18-alpine", "passed": len(checks), "checks": checks, "scope": "current schema and exact SQL predicates; Go control-flow integration separate"}))
    finally:
        removed = command(["docker", "rm", "-f", CONTAINER], timeout=20)
        print(json.dumps({"isolated_container_removed": removed.returncode == 0}))


if __name__ == "__main__":
    main()
