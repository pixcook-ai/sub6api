#!/usr/bin/env python3
"""Real PostgreSQL contention checks for the production publication-fence SQL.

Uses a disposable local container; never connects to an existing database.
"""
import concurrent.futures
import json
from pathlib import Path
import re
import subprocess
import time
import uuid

import account_repo_oauth_controller_postgres as db

ROOT = Path(__file__).resolve().parents[2]
source = (ROOT / "internal/repository/account_repo_oauth_publication_fence.go").read_text()
for name in ("lockManagedOAuthPublicationSQL", "fenceManagedOAuthPublicationSQL",
             "revokeAllManagedOAuthControllerKeysSQL", "enqueueManagedOAuthPublicationFenceSQL"):
    db.QUERIES[name] = re.search("const " + name + " = `([\\s\\S]*?)`", source).group(1)
install_source = (ROOT / "internal/repository/account_repo_managed_oauth.go").read_text()
db.QUERIES["installManagedOAuthSQL"] = re.search("const installManagedOAuthSQL = `([\\s\\S]*?)`", install_source).group(1)


def fenced_state(operation, state=db.STATE):
    return dict(state, publication_fence_operation_id=operation)


def fence_script(account, operation, state=db.STATE, fail_outbox=False):
    lock = db.render("lockManagedOAuthPublicationSQL", [account, state]).strip().rstrip(";")
    return "BEGIN;\nSELECT EXISTS(" + lock + ") AS eligible \\gset\n\\if :eligible\n" + "\n".join([
        db.render("fenceManagedOAuthPublicationSQL", [account, fenced_state(operation, state)]),
        db.render("revokeAllManagedOAuthControllerKeysSQL", [account]),
        db.render("enqueueManagedOAuthPublicationFenceSQL", [account, "fail" if fail_outbox else "account_changed"]),
    ]) + "\n\\endif\nCOMMIT;\n\\echo :eligible\n"


def install(account, state):
    next_state = dict(state, token_revision=1)
    return db.sql(db.render("installManagedOAuthSQL", ["synthetic-at", "1999999999", "workspace", "client", "managed_oauth", next_state, account, state, "account_changed"]))


def contention(register_first):
    account = 30 if register_first else 31
    operation = str(uuid.uuid4())
    registered_operation = str(uuid.uuid4())
    key_hash = uuid.uuid4().hex * 2
    holder = subprocess.Popen(["rtk", "proxy", *db.psql_args("publication-holder")], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    holder.stdin.write(f"BEGIN;SELECT id FROM accounts WHERE id={account} FOR NO KEY UPDATE;\n\\echo LOCKED\nSELECT pg_sleep(3);COMMIT;\n")
    holder.stdin.close()
    while holder.stdout.readline().strip() != "LOCKED":
        assert holder.poll() is None
    register = db.render("putManagedOAuthControllerKeySQL", [account, db.STATE, registered_operation, key_hash, db.IDENTITY])
    fence = fence_script(account, operation)
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(db.sql, register if register_first else fence, True, "publication-first")
        db.wait_for_lock("publication-first")
        second = pool.submit(db.sql, fence if register_first else register, True, "publication-second")
        db.wait_for_lock("publication-second")
        one, two = first.result(), second.result()
    assert holder.wait(timeout=10) == 0
    if register_first:
        assert db.rows(one)[0].endswith("|t") and db.rows(two)[-1] == "t"
        assert db.rows(db.lookup(key_hash))[0].endswith("|f"), "fence sees the key committed while waiting"
    else:
        assert db.rows(one)[-1] == "t" and not db.rows(two), "queued registration rechecks the permanent fence"
        assert not db.rows(db.lookup(key_hash))
    assert db.rows(db.sql(f"SELECT schedulable FROM accounts WHERE id={account};")) == ["f"]
    assert not db.rows(db.put(account, str(uuid.uuid4()), uuid.uuid4().hex * 2, fenced_state(operation)))


def main():
    checks = []
    try:
        result = db.command(["docker", "run", "--rm", "-d", "--name", db.CONTAINER, "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=controllerkeys", "postgres:18-alpine"], timeout=20)
        assert result.returncode == 0, result.stderr
        for _ in range(100):
            log = db.command(["docker", "logs", db.CONTAINER], timeout=5)
            if "PostgreSQL init process complete; ready for start up." in log.stdout + log.stderr and db.sql("SELECT 1;", check=False).returncode == 0:
                break
            time.sleep(.15)
        else:
            raise AssertionError("isolated PostgreSQL unavailable")
        db.sql("CREATE TABLE accounts(id BIGINT PRIMARY KEY,deleted_at TIMESTAMPTZ,platform TEXT,type TEXT,credentials JSONB,extra JSONB,schedulable BOOLEAN,updated_at TIMESTAMPTZ);")
        db.sql((ROOT / "migrations/252_managed_oauth_controller_keys.sql").read_text())
        db.sql("CREATE TABLE scheduler_outbox(event_type TEXT CHECK(event_type <> 'fail'),account_id BIGINT,group_id BIGINT,payload JSONB);")
        for account in (17, 18, 19, 30, 31):
            db.sql(f"INSERT INTO accounts VALUES({account},NULL,'openai','oauth','{{}}'," + db.quote({"managed_oauth": db.STATE}) + "::jsonb,true,NOW());")
        keys = [uuid.uuid4().hex * 2 for _ in range(2)]
        for key in keys:
            assert db.rows(db.put(17, str(uuid.uuid4()), key))
        operation = str(uuid.uuid4())
        assert db.rows(db.sql(fence_script(17, operation)))[-1] == "t"
        for key in keys:
            assert db.rows(db.lookup(key))[0].endswith("|f")
        assert not db.rows(db.put(17, str(uuid.uuid4()), uuid.uuid4().hex * 2, fenced_state(operation)))
        install(17, db.STATE)
        install(17, fenced_state(operation))
        assert db.rows(db.sql("SELECT credentials,extra->'managed_oauth'->>'publication_fence_operation_id' FROM accounts WHERE id=17;")) == ["{}|" + operation]
        assert db.rows(db.sql(fence_script(17, str(uuid.uuid4()))))[-1] == "f"
        checks.append("all_keys_revoked_no_registration_or_install_after_fence")
        changed = dict(db.STATE, token_revision=7)
        db.sql("UPDATE accounts SET extra=" + db.quote({"managed_oauth": changed}) + "::jsonb WHERE id=18;")
        assert db.rows(db.sql(fence_script(18, str(uuid.uuid4()))))[-1] == "f"
        assert db.rows(db.sql("SELECT schedulable FROM accounts WHERE id=18;")) == ["t"]
        checks.append("changed_revision_rejects_without_pausing")
        key = uuid.uuid4().hex * 2
        assert db.rows(db.put(19, str(uuid.uuid4()), key))
        failed = db.sql(fence_script(19, str(uuid.uuid4()), fail_outbox=True), check=False)
        assert failed.returncode != 0
        assert db.rows(db.lookup(key))[0].endswith("|t")
        assert db.rows(db.sql("SELECT schedulable,extra->'managed_oauth'->>'publication_fence_operation_id' FROM accounts WHERE id=19;")) == ["t|"]
        checks.append("outbox_failure_rolls_back_fence_pause_and_revocation")
        contention(True)
        contention(False)
        checks.append("observed_lock_contention_both_registration_orders")
        print(json.dumps({"postgres": "18-alpine", "passed": len(checks), "checks": checks}))
    finally:
        removed = db.command(["docker", "rm", "-f", db.CONTAINER], timeout=20)
        print(json.dumps({"isolated_container_removed": removed.returncode == 0}))


if __name__ == "__main__":
    main()
