#!/usr/bin/env python3
"""Verify migrated schema and exact SQL predicates in disposable PostgreSQL.

Run: rtk proxy python3 internal/repository/stored_responses_postgres.py
Go integration tests separately exercise repository control flow and validators.
"""

import json
from pathlib import Path
import re
import subprocess
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
SOURCE = (ROOT / "internal/repository/stored_responses_repo.go").read_text()
CONTAINER = "sub6-stored-responses-verify-" + uuid.uuid4().hex[:8]


def raw_query(prefix):
    found = [value for value in re.findall(r"`([^`]+)`", SOURCE) if value.startswith(prefix)]
    assert len(found) == 1, (prefix, len(found))
    return found[0]


INSERT = raw_query("INSERT INTO gateway_stored_responses(public_id,")
ANCESTRY = raw_query("SELECT COALESCE(previous_response_id,")
FINALIZE = raw_query("UPDATE gateway_stored_responses SET state=$2,")
DELETE = raw_query("UPDATE gateway_stored_responses SET state='deleted',")
RENEW = raw_query("UPDATE gateway_stored_responses SET lease_until=")
KEY_MODEL = raw_query("SELECT model FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND api_key_id=")
KEY_ROW = "SELECT public_id" + raw_query(" FROM gateway_stored_responses WHERE public_id=$1 AND user_id=$2 AND api_key_id=")
GLOBAL_LOCK = raw_query("SELECT owner_id FROM gateway_stored_response_budgets WHERE owner_id=0 FOR UPDATE")
BUDGET_DELTA = raw_query("UPDATE gateway_stored_response_budgets SET rows_count=")


def command(args, **kwargs):
    return subprocess.run(["rtk", "proxy", *args], text=True, capture_output=True, **kwargs)


def quote(value):
    if value is None:
        return "NULL"
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, dict):
        value = json.dumps(value, separators=(",", ":"))
    return "'" + value.replace("'", "''") + "'"


def render(query, params):
    return re.sub(re.escape("$") + r"([0-9]+)", lambda m: quote(params[int(m[1]) - 1]), query)


def sql(query, check=True):
    result = command(["docker", "exec", "-i", CONTAINER, "psql", "-U", "postgres", "-d", "storedresponses", "-qAt", "-v", "ON_ERROR_STOP=1"], input=query, timeout=25)
    if check and result.returncode:
        raise AssertionError(result.stderr)
    return result


def rows(result):
    return [line for line in result.stdout.splitlines() if line.strip()]


def ready():
    for _ in range(100):
        logs = command(["docker", "logs", CONTAINER], timeout=5)
        if "PostgreSQL init process complete; ready for start up." in logs.stdout + logs.stderr and sql("SELECT 1;", False).returncode == 0:
            return
        time.sleep(0.1)
    raise AssertionError("isolated PostgreSQL did not initialize")


def public_id(number):
    return "resp_local_" + format(number, "032x")


def create_record(number, key=11, binding="binding-a", recovery=0, previous=None):
    pid = public_id(number)
    payload = {"id": pid, "object": "response", "status": "in_progress", "store": True, "output": []}
    params = [pid, 7, key, 42, "model", "model", 17, binding, "2026-11-21 00:00:00+00", 30 * 86400, '"hello"', "{}", payload, 512, 4096, "token-" + str(number), 3600, recovery, previous]
    # Use current database time so this remains reproducible after the written date.
    params[8] = rows(sql("SELECT clock_timestamp();"))[0]
    sql("BEGIN;" + GLOBAL_LOCK + ";INSERT INTO gateway_stored_response_budgets(owner_id) VALUES(7) ON CONFLICT DO NOTHING;" + render(INSERT, params) + ";" + render(BUDGET_DELTA, [7, 1, 4096]) + ";COMMIT;")
    return params


def finalize(params):
    payload = {"id": params[0], "object": "response", "status": "completed", "store": True, "output": []}
    recovery = {"version": 2, "kind": "native"} if params[17] == 2 else ({"version": 1, "kind": "text"} if params[17] == 1 else None)
    values = [params[0], "completed", "terminal", payload, 600, "a" * 64, "b" * 64, params[15], 1, recovery]
    return sql(render(FINALIZE, values) + " RETURNING public_id;")


def main():
    checks = []
    try:
        result = command(["docker", "run", "--rm", "-d", "--name", CONTAINER, "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=storedresponses", "postgres:18-alpine"], timeout=20)
        assert result.returncode == 0, result.stderr
        ready()
        for name in ("254_stored_responses.sql", "255_previous_response_recovery.sql", "256_native_response_recovery.sql"):
            sql((ROOT / "migrations" / name).read_text())
        checks.append("ordered_migration_schema_accepts_native_v2")
        parent = create_record(1, recovery=2)
        assert rows(finalize(parent)) == [parent[0]]
        assert rows(sql(render(KEY_ROW, [parent[0], 7, 11, 42]) + ";")) == [parent[0]]
        assert rows(sql(render(KEY_MODEL, [parent[0], 7, 11, 42]) + ";")) == ["model"]
        for user, key, group in ((8, 11, 42), (7, 12, 42), (7, 11, 43)):
            assert not rows(sql(render(KEY_ROW, [parent[0], user, key, group]) + ";"))
            assert not rows(sql(render(KEY_MODEL, [parent[0], user, key, group]) + ";"))
        checks.append("exact_key_owner_select_and_management_predicates")
        for key, expected in ((11, True), (12, False)):
            query = render(ANCESTRY, [parent[0], 7, 42, "model", "model", 17, 2, key])
            # A matching root has SQL NULL parent normalized to an empty field.
            found = sql("SELECT count(*) FROM (" + query + ") AS ancestry;")
            assert rows(found) == ["1" if expected else "0"]
        checks.append("exact_ancestry_api_key_fence")
        child = create_record(2, binding="admission-turn-2", recovery=2, previous=parent[0])
        assert rows(finalize(child)) == [child[0]]
        for number, key, binding in ((3, 12, "admission-turn-3"),):
            wrong = create_record(number, key=key, binding=binding, recovery=2, previous=parent[0])
            assert not rows(finalize(wrong))
            assert rows(sql("SELECT state FROM gateway_stored_responses WHERE public_id=" + quote(wrong[0]) + ";")) == ["in_progress"]
        checks.append("exact_finalize_CAS_allows_distinct_turn_admissions_and_rejects_cross_key")
        lease = create_record(5)
        assert rows(sql(render(RENEW, [lease[0], lease[15], 1, 60]) + " RETURNING public_id;")) == [lease[0]]
        assert not rows(sql(render(RENEW, [lease[0], "wrong-token", 1, 60]) + " RETURNING public_id;"))
        assert not rows(sql(render(RENEW, [lease[0], lease[15], 2, 60]) + " RETURNING public_id;"))
        sql("UPDATE gateway_stored_responses SET lease_until=clock_timestamp()-interval '1 second' WHERE public_id=" + quote(lease[0]) + ";")
        assert not rows(finalize(lease))
        checks.append("exact_renew_and_finalize_lease_version_token_CAS")
        sql(render(DELETE, [child[0]]) + ";")
        assert not rows(sql(render(KEY_ROW, [child[0], 7, 11, 42]) + ";"))
        assert rows(sql(render(KEY_MODEL, [child[0], 7, 11, 42]) + ";")) == ["model"]
        assert not rows(sql(render(KEY_MODEL, [child[0], 7, 12, 42]) + ";"))
        sql(render(DELETE, [child[0]]) + ";")
        checks.append("tombstone_content_erasure_and_retained_key_scope")
        invalid = sql("UPDATE gateway_stored_responses SET private_recovery=" + quote({"version": 1, "kind": "text"}) + " WHERE public_id=" + quote(parent[0]) + ";", False)
        assert invalid.returncode != 0 and "gateway_response_recovery_state" in invalid.stderr
        checks.append("schema_rejects_native_recovery_kind_substitution")
        command(["docker", "restart", CONTAINER], timeout=20)
        ready()
        assert rows(sql(render(KEY_ROW, [parent[0], 7, 11, 42]) + ";")) == [parent[0]]
        checks.append("database_restart_retains_private_resource_owner")
        print(json.dumps({"postgres": "18-alpine", "passed": len(checks), "checks": checks, "scope": "migrations and exact SQL predicates; stable backend/subject/credential-generation and admission proofs require Go native validators"}))
    finally:
        removed = command(["docker", "rm", "-f", CONTAINER], timeout=20)
        print(json.dumps({"isolated_container_removed": removed.returncode == 0}))


if __name__ == "__main__":
    main()
