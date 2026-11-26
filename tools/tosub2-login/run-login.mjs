#!/usr/bin/env node
// Thin wrapper around src/protocol-login.mjs for sub6api batch OAuth login.
//
// Contract (stdout is one JSON line, nothing else):
//   success: {"ok":true,"record":{...}}
//   failure: {"ok":false,"code":"<fixed_code>"}
//
// Inputs:
//   argv:  --email <email> --provider bps_excel|codex [--workspace-id <id>]
//   env:   CHATGPT_LOGIN_PASSWORD, CHATGPT_TOTP_SECRET, CHATGPT_PROXY_URL (optional)
//
// The password and TOTP secret are only passed through the child's environment.
// Child stdout/stderr are discarded: they can contain account identifiers.
import { fork } from "node:child_process";
import crypto from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isLoginProgressStage } from "./src/login-progress.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const PROVIDERS = new Set(["bps_excel", "codex"]);
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const TIMEOUT_MS = 9 * 60_000;
// Variables that switch toSub2 into managed/test modes must never be inherited.
const STRIPPED_ENV = [
  "TOSUB2_MANAGED_RESOLVED_TRANSPORT",
  "TOSUB2_TEST_SENTINEL_TOKEN",
  "TOSUB2_TEST_SENTINEL_SO_TOKEN",
  "CHATGPT_NEW_PASSWORD",
  "CHATGPT_BASE",
  "AUTH_BASE",
  "NODE_ENV",
];

function emit(payload) {
  process.stdout.write(JSON.stringify(payload) + "\n");
}

function fail(code, exitCode = 1) {
  emit({ ok: false, code });
  process.exit(exitCode);
}

function parseArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i += 1) {
    const key = argv[i];
    if (key === "--email") args.email = argv[++i];
    else if (key === "--provider") args.provider = argv[++i];
    else if (key === "--workspace-id") args.workspaceId = argv[++i];
    else fail("invalid_arguments", 2);
  }
  return args;
}

const args = parseArgs(process.argv.slice(2));
const email = String(args.email || "").trim().toLowerCase();
if (!EMAIL.test(email) || email.length > 320) fail("invalid_arguments", 2);
if (!PROVIDERS.has(args.provider)) fail("invalid_arguments", 2);
if (args.workspaceId !== undefined && !/^[A-Za-z0-9_-]{1,200}$/.test(args.workspaceId)) fail("invalid_arguments", 2);
if (!process.env.CHATGPT_LOGIN_PASSWORD || !process.env.CHATGPT_TOTP_SECRET) fail("missing_credentials", 2);

const env = { ...process.env };
for (const name of STRIPPED_ENV) delete env[name];

const profileRef = "oauth_" + crypto.randomUUID();
const childArgs = ["--email", email, "--oauth-provider", args.provider, "--oauth-profile-ref", profileRef, "--token-ipc"];
if (args.workspaceId) childArgs.push("--target-workspace-id", args.workspaceId);

const child = fork(path.join(here, "src", "protocol-login.mjs"), childArgs, {
  cwd: here,
  env,
  // stdin is kept as a pipe and closed: the login script falls back to prompts when it needs input.
  stdio: ["pipe", "ignore", "pipe", "ipc"],
});
child.stdin?.on("error", () => {});
child.stdin?.end();

let settled = false;
function finish(payload, exitCode) {
  if (settled) return;
  settled = true;
  clearTimeout(timer);
  emit(payload);
  child.kill("SIGKILL");
  process.exit(exitCode);
}

const timer = setTimeout(() => finish({ ok: false, code: "login_timeout" }, 1), TIMEOUT_MS);

for (const signal of ["SIGTERM", "SIGINT"]) {
  process.on(signal, () => finish({ ok: false, code: "canceled" }, 130));
}

child.on("message", (message) => {
  if (settled) return;
  if (message?.type === "oauth_progress") {
    if (message.profileRef === profileRef && isLoginProgressStage(message.stage)) {
      process.stderr.write(JSON.stringify({ type: "progress", stage: message.stage }) + "\n");
    }
    return;
  }
  if (message?.type !== "oauth_tokens" || message.profileRef !== profileRef || !message.record) {
    finish({ ok: false, code: "invalid_worker_message" }, 1);
    return;
  }
  const r = message.record;
  // Whitelist only the fields the backend maps into credentials.
  finish({
    ok: true,
    record: {
      provider: r.provider,
      email: r.email,
      subject: r.subject,
      workspaceId: r.workspaceId,
      clientId: r.clientId,
      accessToken: r.accessToken,
      refreshToken: r.refreshToken,
      idToken: r.idToken,
      expiresAt: r.expiresAt,
    },
  }, 0);
});

child.on("error", () => finish({ ok: false, code: "worker_spawn_failed" }, 1));
let reason = "";
child.stderr?.on("data", (d) => {
  const m = /\[reason\] ([a-z_]+)/.exec(String(d));
  if (m) reason = m[1];
});
child.on("close", () => {
  finish({ ok: false, code: reason || "login_failed" }, 1);
});
