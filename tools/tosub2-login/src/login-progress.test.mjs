import assert from "node:assert/strict";
import { test } from "node:test";
import { fork } from "node:child_process";
import { mkdtemp, mkdir, copyFile, writeFile, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";

test("wrapper delivers only safe progress before the final login result", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "oauth-progress-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(path.join(root, "src"));
  await copyFile(new URL("../run-login.mjs", import.meta.url), path.join(root, "run-login.mjs"));
  await copyFile(new URL("./login-progress.mjs", import.meta.url), path.join(root, "src/login-progress.mjs"));
  await writeFile(path.join(root, "src/protocol-login.mjs"), `
    const ref = process.argv[process.argv.indexOf("--oauth-profile-ref") + 1];
    process.send({ type: "oauth_progress", profileRef: "wrong-profile", stage: "email" });
    process.send({ type: "oauth_progress", profileRef: ref, stage: "secret-password" });
    process.send({ type: "oauth_progress", profileRef: ref, stage: "password", password: "private-value" });
    setTimeout(() => process.send({ type: "oauth_tokens", profileRef: ref, record: { provider: "codex", email: "fixture@example.invalid" } }), 200);
  `);
  const child = fork(path.join(root, "run-login.mjs"), ["--email", "fixture@example.invalid", "--provider", "codex"], {
    execArgv: [], silent: true,
    env: { ...process.env, CHATGPT_LOGIN_PASSWORD: "synthetic", CHATGPT_TOTP_SECRET: "synthetic" },
  });
  t.after(() => child.kill());
  let stdout = "";
  let stderr = "";
  let progressBeforeExit = false;
  child.stdout.on("data", (value) => { stdout += value; });
  child.stderr.on("data", (value) => {
    stderr += value;
    if (stderr.includes('"stage":"password"')) progressBeforeExit = child.exitCode === null && stdout === "";
  });
  const [code] = await once(child, "close");
  assert.equal(code, 0);
  assert.equal(progressBeforeExit, true);
  assert.deepEqual(stderr.trim().split("\n").map((line) => JSON.parse(line)), [{ type: "progress", stage: "password" }]);
  assert.equal(JSON.parse(stdout).ok, true);
  assert.equal(stderr.includes("private-value"), false);
});


test("wrapper preserves numeric HTTP reasons across stderr chunks", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "oauth-failure-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(path.join(root, "src"));
  await copyFile(new URL("../run-login.mjs", import.meta.url), path.join(root, "run-login.mjs"));
  await copyFile(new URL("./login-progress.mjs", import.meta.url), path.join(root, "src/login-progress.mjs"));
  const scenarios = [
    { chunks: ["[reason] codex_oauth_ht", "tp_429"], code: "codex_oauth_http_429" },
    { chunks: ["[reason] codex_oauth_risk_control_http_403[error] private"], code: "codex_oauth_risk_control_http_403" },
    { chunks: ["[reason] codex_token_exchange_http_503\n"], code: "codex_token_exchange_http_503" },
    { chunks: ["[reason] " + "x".repeat(97)], code: "login_failed" },
    { chunks: ["[reason] codex_oauth_http_429Secret"], code: "login_failed" },
  ];
  for (const scenario of scenarios) {
    await writeFile(path.join(root, "src/protocol-login.mjs"), `
      const chunks = ${JSON.stringify(scenario.chunks)};
      for (const chunk of chunks) {
        await new Promise((resolve) => process.stderr.write(chunk, resolve));
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
      process.exit(1);
    `);
    const child = fork(path.join(root, "run-login.mjs"), ["--email", "fixture@example.invalid", "--provider", "codex"], {
      execArgv: [], silent: true,
      env: { ...process.env, CHATGPT_LOGIN_PASSWORD: "synthetic", CHATGPT_TOTP_SECRET: "synthetic" },
    });
    t.after(() => child.kill());
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (value) => { stdout += value; });
    child.stderr.on("data", (value) => { stderr += value; });
    const [code] = await once(child, "close");
    assert.equal(code, 1);
    assert.deepEqual(JSON.parse(stdout), { ok: false, code: scenario.code });
    assert.equal(stderr, "");
  }
});

test("wrapper exposes proxy timezone detection progress", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "oauth-timezone-progress-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  await mkdir(path.join(root, "src"));
  await copyFile(new URL("../run-login.mjs", import.meta.url), path.join(root, "run-login.mjs"));
  await copyFile(new URL("./login-progress.mjs", import.meta.url), path.join(root, "src/login-progress.mjs"));
  await writeFile(path.join(root, "src/protocol-login.mjs"), `
    const ref = process.argv[process.argv.indexOf("--oauth-profile-ref") + 1];
    process.send({ type: "oauth_progress", profileRef: ref, stage: "timezone_check" });
    setTimeout(() => process.send({ type: "oauth_tokens", profileRef: ref, record: { provider: "codex", email: "fixture@example.invalid" } }), 50);
  `);
  const child = fork(path.join(root, "run-login.mjs"), ["--email", "fixture@example.invalid", "--provider", "codex"], {
    execArgv: [], silent: true,
    env: { ...process.env, CHATGPT_LOGIN_PASSWORD: "synthetic", CHATGPT_TOTP_SECRET: "synthetic" },
  });
  t.after(() => child.kill());
  let stderr = "";
  child.stderr.on("data", (value) => { stderr += value; });
  const [code] = await once(child, "close");

  assert.equal(code, 0);
  assert.deepEqual(stderr.trim().split("\n").map((line) => JSON.parse(line)), [
    { type: "progress", stage: "timezone_check" },
  ]);
});
