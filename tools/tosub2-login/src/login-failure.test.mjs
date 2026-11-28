import assert from "node:assert/strict";
import test from "node:test";
import { classifyLoginFailure, stageLoginFailure } from "./login-failure.mjs";

test("reports a Codex preflight security challenge as risk control", () => {
  const category = classifyLoginFailure(
    "PROXY_RISK_CONTROL: GET https://chatgpt.com/ returned HTTP 403 with a security-check page",
  );

  assert.equal(stageLoginFailure("codex_chatgpt_preflight", category), "codex_chatgpt_preflight_risk_control_http_403");
});

test("keeps proxy connection failures tied to their login stage", () => {
  const category = classifyLoginFailure("PROXY_CONNECTION_RETRY: GET https://chatgpt.com/ failed");

  assert.equal(stageLoginFailure("codex_chatgpt_preflight", category), "codex_chatgpt_preflight_proxy_error");
});

test("reports proxy timezone detection failures with a stable code", () => {
  const category = classifyLoginFailure("PROXY_TIMEZONE_DETECTION_FAILED: invalid IANA timezone");

  assert.equal(category, "proxy_timezone_detection_failed");
  assert.equal(stageLoginFailure("codex_oauth", category), "proxy_timezone_detection_failed");
});

test("does not add a stage to authentication failures", () => {
  assert.equal(stageLoginFailure("codex_oauth", "password_rejected"), "password_rejected");
});

test("preserves HTTP failures and their login stage without upstream response text", () => {
  for (const [text, expected] of [
    ["authorize failed with HTTP 429: private upstream message", "codex_oauth_http_429"],
    ["authorize failed with HTTP 503: private upstream message", "codex_oauth_http_503"],
    ["authorize failed with HTTP 403: forbidden", "codex_oauth_risk_control_http_403"],
    ["PROXY_RISK_CONTROL: returned HTTP 409 with a security-check page", "codex_oauth_risk_control_http_409"],
    ["Expected JSON from HTTP 502: private upstream message", "codex_oauth_unexpected_response_http_502"],
  ]) {
    assert.equal(stageLoginFailure("codex_oauth", classifyLoginFailure(text)), expected);
  }
  assert.equal(classifyLoginFailure("HTTP 200 succeeded"), "");
  assert.equal(classifyLoginFailure("HTTP 4290 malformed status"), "");
});
