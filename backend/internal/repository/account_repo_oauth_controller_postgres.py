#!/usr/bin/env python3
"""Verify exact controller-key SQL in disposable PostgreSQL; no Go build or existing DB.

Run: rtk proxy python3 internal/repository/account_repo_oauth_controller_postgres.py
"""

import concurrent.futures
import copy
import json
from pathlib import Path
import re
import subprocess
import time
import uuid


ROOT = Path(__file__).resolve().parents[2]
SOURCE = (ROOT / "internal/repository/account_repo_oauth_controller.go").read_text()
QUERIES = {
    name: re.search("const " + name + " = " + chr(96) + r"([\s\S]*?)" + chr(96), SOURCE).group(1)
    for name in (
        "putManagedOAuthControllerKeySQL",
        "revokeManagedOAuthControllerKeySQL",
        "findManagedOAuthControllerKeySQL",
    )
}
CONTAINER = "sub6-controller-keys-verify-" + uuid.uuid4().hex[:8]
IDENTITY = {
    "contract": "tosub2-managed-oauth/v1",
    "backend_id": "backend",
    "binding_ref": "binding",
    "profile_ref": "profile",
    "provider": "codex",
    "subject": "subject",
    "workspace_id": "workspace",
    "identity_epoch": "epoch",
}
STATE = dict(IDENTITY, token_revision=0, auth_blocked=False, event_cursor=0, events=[])
OPERATION = "36dc40ec-a9f6-4e46-a4e0-ab899ce1b85e"
HASH = "a" * 64


def command(args, **kwargs):
    return subprocess.run(["rtk", "proxy", *args], text=True, capture_output=True, **kwargs)


def quote(value):
    if isinstance(value, int):
        return str(value)
    if isinstance(value, dict):
        value = json.dumps(value, separators=(",", ":"))
    return "'" + value.replace("'", "''") + "'"


def render(name, params):
    return re.sub(r"\$(\d+)", lambda match: quote(params[int(match[1]) - 1]), QUERIES[name]) + ";"


def psql_args(application=None):
    args = ["docker", "exec", "-i"]
    if application:
        args += ["-e", "PGAPPNAME=" + application]
    return args + [CONTAINER, "psql", "-U", "postgres", "-d", "controllerkeys", "-qAt", "-v", "ON_ERROR_STOP=1"]


def sql(query, check=True, application=None):
    result = command(psql_args(application), input=query, timeout=20)
    if check and result.returncode:
        raise AssertionError(result.stderr)
    return result


def rows(result):
    return [line for line in result.stdout.splitlines() if line.strip()]


def put(account=17, operation=OPERATION, key_hash=HASH, expected=STATE):
    return sql(render("putManagedOAuthControllerKeySQL", [account, expected, operation, key_hash, IDENTITY]))


def revoke(account=17, operation=OPERATION):
    return sql(render("revokeManagedOAuthControllerKeySQL", [account, operation]))


def lookup(key_hash=HASH):
    return sql(render("findManagedOAuthControllerKeySQL", [key_hash]))


def wait_for_lock(application):
    for _ in range(30):
        count = sql("SELECT COUNT(*) FROM pg_stat_activity WHERE application_name=" + quote(application) + " AND wait_event_type='Lock';")
        if rows(count) == ["1"]:
            return
        time.sleep(0.04)
    raise AssertionError("expected controller SQL to wait on account row lock")


def ordered_race(revoke_first):
    operation = str(uuid.uuid4())
    key_hash = uuid.uuid4().hex * 2
    assert rows(put(account=19, operation=operation, key_hash=key_hash))
    holder = subprocess.Popen(
        ["rtk", "proxy", *psql_args("controller-lock-holder")],
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    holder.stdin.write("BEGIN;SELECT id FROM accounts WHERE id=19 FOR NO KEY UPDATE;\n\\echo LOCKED\nSELECT pg_sleep(5);COMMIT;\n")
    holder.stdin.close()
    while holder.stdout.readline().strip() != "LOCKED":
        assert holder.poll() is None, "account lock holder exited unexpectedly"
    register = render("putManagedOAuthControllerKeySQL", [19, STATE, operation, key_hash, IDENTITY])
    disable = render("revokeManagedOAuthControllerKeySQL", [19, operation])
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(sql, disable if revoke_first else register, True, "controller-race-first")
        wait_for_lock("controller-race-first")
        second = pool.submit(sql, register if revoke_first else disable, True, "controller-race-second")
        wait_for_lock("controller-race-second")
        first_result, second_result = first.result(), second.result()
    assert holder.wait(timeout=10) == 0
    if revoke_first:
        assert rows(first_result)[0].endswith("|f") and not rows(second_result)
    else:
        assert rows(first_result)[0].endswith("|t") and rows(second_result)[0].endswith("|f")
    assert rows(lookup(key_hash))[0].endswith("|f")
    assert not rows(put(account=19, operation=operation, key_hash=key_hash))


def main():
    checks = []
    try:
        started = command([
            "docker", "run", "--rm", "-d", "--name", CONTAINER,
            "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=controllerkeys",
            "postgres:18-alpine",
        ], timeout=20)
        assert started.returncode == 0, started.stderr
        for _ in range(100):
            logs = command(["docker", "logs", CONTAINER], timeout=5)
            initialized = "PostgreSQL init process complete; ready for start up." in logs.stdout + logs.stderr
            if initialized and sql("SELECT 1;", check=False).returncode == 0:
                break
            time.sleep(0.15)
        else:
            raise AssertionError("isolated PostgreSQL did not finish initialization")
        sql("CREATE TABLE accounts(id BIGINT PRIMARY KEY,deleted_at TIMESTAMPTZ,platform TEXT,type TEXT,credentials JSONB,extra JSONB);")
        sql((ROOT / "migrations/252_managed_oauth_controller_keys.sql").read_text())
        for account in (17, 18, 19):
            sql("INSERT INTO accounts VALUES(" + str(account) + ",NULL,'openai','oauth','{}'," + quote({"managed_oauth": STATE}) + "::jsonb);")
        assert len(rows(put())) == 1 and len(rows(put())) == 1
        assert not rows(put(key_hash="b" * 64))
        checks.append("create_lost_response_retry_payload_conflict")
        duplicate = sql(render("putManagedOAuthControllerKeySQL", [18, STATE, str(uuid.uuid4()), HASH, IDENTITY]), False)
        assert duplicate.returncode != 0 and "unique constraint" in duplicate.stderr
        checks.append("hash_unique_across_accounts")
        assert rows(revoke())[0].endswith("|f") and rows(revoke())[0].endswith("|f")
        assert rows(lookup())[0].endswith("|f") and not rows(put())
        assert not rows(lookup("f" * 64))
        assert not rows(revoke(operation=str(uuid.uuid4())))
        checks.append("retained_revoke_distinguished_from_unknown_and_no_recreate")
        changed = copy.deepcopy(STATE)
        changed["token_revision"] = 7
        sql("UPDATE accounts SET extra=" + quote({"managed_oauth": changed}) + "::jsonb WHERE id=18;")
        assert not rows(put(account=18, operation=str(uuid.uuid4()), key_hash="c" * 64))
        sql("UPDATE accounts SET extra=" + quote({"managed_oauth": STATE}) + "::jsonb,credentials='" + json.dumps({"refresh_token": "synthetic-rt"}) + "'::jsonb WHERE id=18;")
        assert not rows(put(account=18, operation=str(uuid.uuid4()), key_hash="c" * 64))
        sql("UPDATE accounts SET credentials='{}',extra='" + json.dumps({"managed_oauth": None}) + "'::jsonb WHERE id=18;")
        assert not rows(put(account=18, operation=str(uuid.uuid4()), key_hash="c" * 64))
        sql("UPDATE accounts SET extra=" + quote({"managed_oauth": STATE}) + "::jsonb,deleted_at=NOW() WHERE id=18;")
        assert not rows(put(account=18, operation=str(uuid.uuid4()), key_hash="c" * 64))
        checks.append("full_state_CAS_RT_null_marker_deleted_fences")
        ordered_race(True)
        ordered_race(False)
        checks.append("observed_account_lock_orders_both_register_revoke_races")
        operation = str(uuid.uuid4())
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(lambda value: put(account=19, operation=operation, key_hash=value), ("d" * 64, "e" * 64)))
        assert sorted(len(rows(result)) for result in results) == [0, 1]
        checks.append("concurrent_same_operation_single_winner")
        print(json.dumps({"postgres": "18-alpine", "passed": len(checks), "checks": checks}))
    finally:
        removed = command(["docker", "rm", "-f", CONTAINER], timeout=20)
        print(json.dumps({"isolated_container_removed": removed.returncode == 0}))


if __name__ == "__main__":
    main()
