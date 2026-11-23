#!/usr/bin/env python3
"""Run the repository's managed OAuth CTEs against an isolated PostgreSQL 17.

Usage: rtk proxy python3 scripts/test-managed-oauth-postgres.py

Requires a running local Docker daemon and the existing postgres:17-alpine
image. No image is pulled. The random container has no network or published
ports, uses tmpfs for its database, and is forcibly removed in finally.

This verifies SQL compare-and-swap, preservation and transaction semantics on
a minimal schema. It does not verify HTTP/OAuth, ent migrations or scheduling.
"""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from copy import deepcopy
import hashlib
import json
from pathlib import Path
import re
import subprocess
import threading
import time
import uuid


BACKEND = Path(__file__).resolve().parents[1]
IMAGE = "postgres:17-alpine"
DATABASE = "managed_oauth_test"
STATE_KEY = "managed_oauth"
OUTBOX_EVENT = "account_changed"


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def run_docker(*args: str, sql: str | None = None, check: bool = True, timeout: float = 30) -> subprocess.CompletedProcess[str]:
    # argv only: no shell expansion, external downloads, or host DB access.
    result = subprocess.run(
        ["rtk", "proxy", "docker", *args],
        input=sql, text=True, capture_output=True, timeout=timeout, check=False,
    )
    if check and result.returncode:
        raise RuntimeError(f"Docker command failed ({args[0]}): {result.stderr.strip()}")
    return result


def source_sql(name: str) -> str:
    source = (BACKEND / "internal/repository/account_repo_managed_oauth.go").read_text()
    found = re.findall(r"\bconst\s+" + re.escape(name) + r"\s*=\s*`([^`]+)`", source)
    require(len(found) == 1, f"Expected exactly one Go raw-string constant: {name}")
    return found[0].strip()


def sql_text(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def sql_json(value: object) -> str:
    return sql_text(json.dumps(value, separators=(",", ":"), sort_keys=True))


def main() -> None:
    install_sql = source_sql("installManagedOAuthSQL")
    event_sql = source_sql("recordManagedOAuthEventSQL")
    # Guard the parameter conventions used below against unrelated source drift.
    service_source = (BACKEND / "internal/service/managed_openai_oauth.go").read_text()
    outbox_source = (BACKEND / "internal/service/scheduler_events.go").read_text()
    require(f'const ManagedOAuthExtraKey = "{STATE_KEY}"' in service_source, "Managed OAuth key changed; update SQL fixture parameters")
    require(re.search(r'SchedulerOutboxEventAccountChanged\s*=\s*"' + OUTBOX_EVENT + r'"', outbox_source) is not None, "Scheduler event type changed")
    name = "sub6-managed-oauth-test-" + uuid.uuid4().hex[:12]
    preparation = (
        "SET statement_timeout='12s'; SET lock_timeout='8s'; SET standard_conforming_strings=on;\n"
        + "PREPARE managed_install(text,text,text,text,text,jsonb,bigint,jsonb,text) AS\n" + install_sql + ";\n"
        + "PREPARE managed_event(text,jsonb,bigint,jsonb,text,text) AS\n" + event_sql + ";\n"
    )

    def psql(sql: str, *, prepare: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
        return run_docker("exec", "-i", name, "psql", "--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
                          "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", DATABASE,
                          sql=(preparation if prepare else "") + sql, check=check)

    def query(sql: str) -> str:
        return psql(sql).stdout.strip()

    def row(account_id: int = 1) -> dict:
        return json.loads(query(f"SELECT row_to_json(a) FROM accounts a WHERE id={account_id};"))

    def outbox_count(account_id: int = 1) -> int:
        return int(query(f"SELECT count(*) FROM scheduler_outbox WHERE account_id={account_id};"))

    def execute(statement: str, *, check: bool = True) -> tuple[int | None, subprocess.CompletedProcess[str]]:
        result = psql(statement + ";\n\\echo affected=:ROW_COUNT\n", prepare=True, check=check)
        matches = re.findall(r"^affected=(\d+)\s*$", result.stdout, re.MULTILINE)
        return (int(matches[-1]) if matches else None), result

    def install(expected: dict, next_state: dict, token: str, account_id: int = 1) -> str:
        params = [sql_text(token), sql_text("1800000000"), sql_text(next_state["workspace_id"]),
                  sql_text("synthetic-codex-client"), sql_text(STATE_KEY), sql_json(next_state),
                  str(account_id), sql_json(expected), sql_text(OUTBOX_EVENT)]
        return "EXECUTE managed_install(" + ",".join(params) + ")"

    def event(expected: dict, next_state: dict, token: str, account_id: int = 1) -> str:
        params = [sql_text(STATE_KEY), sql_json(next_state), str(account_id), sql_json(expected), sql_text(token), sql_text(OUTBOX_EVENT)]
        return "EXECUTE managed_event(" + ",".join(params) + ")"

    def next_install(previous: dict, token_revision: int) -> dict:
        state = deepcopy(previous)
        state.update(token_revision=token_revision, auth_blocked=False, expires_at="2027-01-15T08:00:00Z",
                     last_operation_id=str(uuid.uuid4()), last_operation_hash=hashlib.sha256(str(uuid.uuid4()).encode()).hexdigest())
        return state

    def next_event(previous: dict) -> dict:
        state = deepcopy(previous)
        cursor = previous["event_cursor"] + 1
        identity_fields = ["contract", "backend_id", "binding_ref", "profile_ref", "provider", "subject", "workspace_id", "identity_epoch"]
        notification = {key: state[key] for key in identity_fields}
        notification.update(cursor=cursor, event_id=cursor, failed_token_revision=previous["token_revision"],
                            origin="upstream_oauth", code="401", observed_at="2026-11-21T12:00:00Z")
        state.update(auth_blocked=True, event_cursor=cursor, events=state["events"] + [notification])
        return state

    def no_update(statement: str, label: str, account_id: int = 1) -> None:
        before = row(account_id)
        before_events = outbox_count(account_id)
        affected, _ = execute(statement)
        require(affected == 0, f"{label}: expected zero updated rows")
        require(row(account_id) == before, f"{label}: unexpectedly changed account data")
        require(outbox_count(account_id) == before_events, f"{label}: unexpectedly inserted an outbox event")
        print("PASS " + label)

    cleanup_needed = False
    try:
        cleanup_needed = True
        run_docker("run", "--detach", "--pull", "never", "--name", name, "--network", "none",
                   "--tmpfs", "/var/lib/postgresql/data:rw,nosuid,noexec,size=128m", "--memory", "256m",
                   "--cpus", "1", "--pids-limit", "128", "--shm-size", "64m",
                   "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=" + DATABASE, IMAGE)
        config = json.loads(run_docker("inspect", "--format", "{{json .HostConfig}}", name).stdout)
        require(config["NetworkMode"] == "none" and not config["PortBindings"] and not config["Binds"]
                and "/var/lib/postgresql/data" in config["Tmpfs"], "Database container isolation is not as requested")
        deadline = time.monotonic() + 30
        while True:
            ready = run_docker("exec", name, "pg_isready", "-U", "postgres", "-d", DATABASE, check=False, timeout=5)
            if ready.returncode == 0:
                break
            require(time.monotonic() < deadline, "Isolated PostgreSQL did not become ready")
            time.sleep(0.2)
        print("PostgreSQL ready: isolated network=none, tmpfs, no published ports")
        print("Repository SQL SHA256: install=" + hashlib.sha256(install_sql.encode()).hexdigest()[:16]
              + " event=" + hashlib.sha256(event_sql.encode()).hexdigest()[:16])
        psql("""
CREATE TABLE accounts (
 id bigint PRIMARY KEY, platform text NOT NULL, type text NOT NULL,
 credentials jsonb, extra jsonb, status text NOT NULL, schedulable boolean NOT NULL,
 error_message text NOT NULL, deleted_at timestamptz, updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE scheduler_outbox (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, event_type text NOT NULL,
 account_id bigint, group_id bigint, payload jsonb
);
""")
        initial = {"contract": "tosub2-managed-oauth/v1", "backend_id": "sql-test-worker", "binding_ref": "bind_" + str(uuid.uuid4()),
                   "profile_ref": "oauth_" + str(uuid.uuid4()), "provider": "bps_excel", "subject": "sql-test-subject",
                   "workspace_id": "sql-test-workspace", "identity_epoch": str(uuid.uuid4()), "token_revision": 0,
                   "auth_blocked": False, "event_cursor": 0, "events": []}
        unrelated_extra = {"operator_note": "keep operator note", "nested": {"owner": "admin"}, "unrelated_auth_blocked": True}
        credentials = {"access_token": "", "refresh_token": "", "id_token": "synthetic-legacy-id",
                       "existing_credential": "preserve-credential", "api_key": "preserve-api-key"}
        for account_id in [1, 2]:
            account_credentials = dict(credentials)
            if account_id == 2:
                account_credentials["refresh_token"] = "synthetic-nonempty-rt"
                account_credentials["access_token"] = "synthetic-existing-at"
            params = [str(account_id), "'openai'", "'oauth'", sql_json(account_credentials), sql_json({**unrelated_extra, STATE_KEY: initial}),
                      "'error'", "false", "'admin-disabled-marker'"]
            psql("INSERT INTO accounts(id,platform,type,credentials,extra,status,schedulable,error_message) VALUES (" + ",".join(params) + ");")

        # The winner holds its transaction open after UPDATE. Observe the other
        # psql backend waiting on a real PostgreSQL lock, rather than just running
        # two sequential calls that happen to use the same expected JSON.
        gate = threading.Barrier(3)
        variants = [next_install(initial, 1), next_install(initial, 1)]

        def contender(index: int) -> int:
            gate.wait(timeout=10)
            script = ("SET application_name='managed-oauth-race-" + str(index) + "'; BEGIN;\n"
                      + install(initial, variants[index], "synthetic-race-at-" + str(index))
                      + ";\n\\echo affected=:ROW_COUNT\nSELECT pg_sleep(1.5); COMMIT;\n")
            output = psql(script, prepare=True).stdout
            found = re.findall(r"^affected=(\d+)\s*$", output, re.MULTILINE)
            require(len(found) == 1, "Concurrent EXECUTE did not report ROW_COUNT")
            return int(found[0])

        with ThreadPoolExecutor(max_workers=2) as executor:
            racers = [executor.submit(contender, index) for index in range(2)]
            gate.wait(timeout=10)
            lock_observed = False
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline and not all(future.done() for future in racers):
                waiting = int(query("SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE 'managed-oauth-race-%' AND wait_event_type='Lock';"))
                if waiting:
                    lock_observed = True
                    break
                time.sleep(0.05)
            affected = sorted(future.result(timeout=20) for future in racers)
        require(lock_observed, "No overlapping PostgreSQL row-lock contention was observed")
        require(affected == [0, 1], "Same-expected CAS must have exactly one winner")
        require(outbox_count() == 1, "Concurrent CAS must insert exactly one scheduler outbox event")
        installed = row()
        current = installed["extra"][STATE_KEY]
        require(current in variants and current["token_revision"] == 1, "Winner state was not installed exactly")
        winner = variants.index(current)
        require(installed["credentials"]["access_token"] == "synthetic-race-at-" + str(winner), "AT/state write was not atomic")
        print("PASS concurrent same-expected CAS: observed lock wait, one winner, one outbox")

        stale_epoch = deepcopy(current)
        stale_epoch["identity_epoch"] = str(uuid.uuid4())
        no_update(install(stale_epoch, next_install(stale_epoch, 3), "synthetic-wrong-epoch-at"), "old identity epoch rejects installation")
        no_update(install(initial, next_install(initial, 4), "synthetic-old-expected-at"), "stale expected state rejects installation")
        no_update(event(current, next_event(current), "synthetic-stale-at"), "stale AT event does not block current authorization")
        no_update(event(stale_epoch, next_event(stale_epoch), installed["credentials"]["access_token"]), "old identity epoch rejects event")
        no_update(event(initial, next_event(initial), installed["credentials"]["access_token"]), "stale expected state rejects event")

        blocked = next_event(current)
        current_at = installed["credentials"]["access_token"]
        affected, _ = execute(event(current, blocked, current_at))
        require(affected == 1 and outbox_count() == 2, "Correct 401 event must update once and emit one outbox event")
        blocked_row = row()
        require(blocked_row["extra"][STATE_KEY] == blocked and blocked_row["credentials"] == installed["credentials"], "401 event must only change managed metadata")
        no_update(event(current, blocked, current_at), "duplicate 401 event is claimed once")
        print("PASS current 401 sets auth_blocked and preserves credentials")

        jumped = next_install(blocked, 7)
        affected, _ = execute(install(blocked, jumped, "synthetic-recovered-at-7"))
        require(affected == 1 and outbox_count() == 3, "Revision jump must install exactly once")
        restored = row()
        require(restored["extra"][STATE_KEY] == jumped and not jumped["auth_blocked"], "New AT must clear its own managed auth block")
        require(jumped["events"] == blocked["events"] and jumped["event_cursor"] == 1, "Recovery must retain event history/cursor")
        require({key: value for key, value in restored["extra"].items() if key != STATE_KEY} == unrelated_extra, "Recovery changed unrelated extra values")
        require(restored["status"] == "error" and restored["schedulable"] is False
                and restored["error_message"] == "admin-disabled-marker", "Recovery altered operator status/scheduling/error marker")
        require(restored["credentials"] == {"access_token": "synthetic-recovered-at-7", "expires_at": "1800000000",
                "chatgpt_account_id": initial["workspace_id"], "client_id": "synthetic-codex-client",
                "existing_credential": "preserve-credential", "api_key": "preserve-api-key"}, "Recovery lost unrelated credentials or retained RT/id_token")
        print("PASS revision jump 1->7; recovery preserves operator fences, other extra/credentials, and history")

        no_update(install(initial, next_install(initial, 1), "synthetic-refused-at", 2), "nonempty RT prevents managed installation", 2)
        no_update(event(initial, next_event(initial), "synthetic-existing-at", 2), "nonempty RT prevents managed failure event", 2)

        # Force INSERT failure after the UPDATE CTE has matched. PostgreSQL must
        # roll back the entire statement, including AT and managed metadata.
        psql("ALTER TABLE scheduler_outbox ADD CONSTRAINT reject_test_outbox CHECK(event_type <> 'account_changed') NOT VALID;")
        before_failure = row()
        before_count = outbox_count()
        next_state = next_install(jumped, 9)
        _, failed = execute(install(jumped, next_state, "synthetic-must-rollback-at"), check=False)
        require(failed.returncode != 0 and "reject_test_outbox" in failed.stderr, "Did not induce the intended outbox failure")
        require(row() == before_failure and outbox_count() == before_count, "Outbox failure left a partial AT/state update")
        _, failed_event = execute(event(jumped, next_event(jumped), "synthetic-recovered-at-7"), check=False)
        require(failed_event.returncode != 0 and "reject_test_outbox" in failed_event.stderr, "Did not induce event outbox failure")
        require(row() == before_failure and outbox_count() == before_count, "Outbox failure left a partial auth block/cursor")
        psql("ALTER TABLE scheduler_outbox DROP CONSTRAINT reject_test_outbox;")
        affected, _ = execute(install(jumped, next_state, "synthetic-final-at-9"))
        require(affected == 1 and row()["credentials"]["access_token"] == "synthetic-final-at-9", "Post-rollback retry did not succeed")
        print("PASS outbox failures atomically roll back AT installation and auth-event changes; retry succeeds")
        print("Managed OAuth PostgreSQL SQL integration checks passed (minimal schema; no HTTP/OAuth/ent claim)")
    finally:
        if cleanup_needed:
            removed = run_docker("rm", "--force", name, check=False)
            missing = run_docker("container", "inspect", name, check=False)
            require(missing.returncode != 0 and "no such" in missing.stderr.lower(),
                    "Could not verify removal of the isolated PostgreSQL container: " + removed.stderr.strip())
            print("CLEANUP verified container removed: " + name)


if __name__ == "__main__":
    main()
