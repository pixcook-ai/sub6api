import assert from "node:assert/strict";
import test from "node:test";
import { classifyLoginFailure, stageLoginFailure } from "./login-failure.mjs";

test("reports a Codex preflight security challenge as risk control", () => {
  const category = classifyLoginFailure(
    "PROXY_RISK_CONTROL: GET https://chatgpt.com/ returned HTTP 403 with a security-check page",
  );

  assert.equal(stageLoginFailure("codex_chatgpt_preflight", category), "codex_chatgpt_preflight_risk_control");
});

test("keeps proxy connection failures tied to their login stage", () => {
  const category = classifyLoginFailure("PROXY_CONNECTION_RETRY: GET https://chatgpt.com/ failed");

  assert.equal(stageLoginFailure("codex_chatgpt_preflight", category), "codex_chatgpt_preflight_proxy_error");
});

test("does not add a stage to authentication failures", () => {
  assert.equal(stageLoginFailure("codex_oauth", "password_rejected"), "password_rejected");
});
