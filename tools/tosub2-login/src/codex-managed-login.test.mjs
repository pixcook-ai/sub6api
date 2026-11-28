import assert from "node:assert/strict";
import { test } from "node:test";
import { runManagedOAuthLogin } from "./protocol-login.mjs";
import { CODEX_CALLBACK, CODEX_CLIENT_ID } from "./codex-oauth.mjs";
import { BPS_EXCEL_CALLBACK, BPS_EXCEL_CLIENT_ID } from "./bps-oauth.mjs";

for (const [provider, clientId, callbackUrl] of [
  ["codex", CODEX_CLIENT_ID, CODEX_CALLBACK],
  ["bps_excel", BPS_EXCEL_CLIENT_ID, BPS_EXCEL_CALLBACK],
]) {
  test(`managed ${provider} login reports each step before delivering tokens`, async (t) => {
    const previousEnv = { ...process.env };
    Object.assign(process.env, { NODE_ENV: "test", TOSUB2_TEST_SENTINEL_TOKEN: "synthetic-sentinel",
      CHATGPT_LOGIN_PASSWORD: "synthetic-password", CHATGPT_TOTP_SECRET: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" });
    t.after(() => { process.env = previousEnv; });
    t.mock.method(Date, "now", () => 59_000);
    const calls = [];
    let authorization;
    let delivered;
    const progress = [];
    const previousConnected = Object.getOwnPropertyDescriptor(process, "connected");
    Object.defineProperty(process, "connected", { configurable: true, value: true });
    t.after(() => {
      if (previousConnected) Object.defineProperty(process, "connected", previousConnected);
      else delete process.connected;
    });
    const previousSend = process.send;
    process.send = (message, callback) => {
      if (message.type === "oauth_progress") progress.push(message.stage);
      else delivered = message;
      callback();
    };
    t.after(() => { if (previousSend) process.send = previousSend; else delete process.send; });
    const response = (body, status = 200, headers = {}) => new Response(
      typeof body === "string" ? body : JSON.stringify(body), { status, headers },
    );
    const token = (claims) => `e30.${Buffer.from(JSON.stringify(claims)).toString("base64url")}.synthetic`;
    const transport = {
      enabled: true,
      profile: "chrome146",
      cloudflareSolver: true,
      hasConfiguredProxy: () => false,
      fetch: async (value, options) => {
        const url = new URL(value);
        calls.push(`${options.method} ${url.origin}${url.pathname}`);
        assert.equal(url.origin, "https://auth.openai.com", "Codex password login must start in its OAuth transaction");
        if (["/oauth/authorize", "/api/accounts/authorize"].includes(url.pathname)) {
          authorization = url;
          assert.equal(url.searchParams.get("client_id"), clientId);
          assert.equal(url.searchParams.get("redirect_uri"), callbackUrl);
          return response("", 302, { location: "https://auth.openai.com/log-in" });
        }
        if (["/log-in", "/log-in/password"].includes(url.pathname)) return response("<html></html>");
        if (url.pathname === "/api/accounts/authorize/continue") {
          assert.deepEqual(JSON.parse(options.body), { username: { kind: "email", value: "fixture@example.invalid" } });
          return response({ continue_url: "https://auth.openai.com/log-in/password" });
        }
        if (url.pathname === "/api/accounts/password/verify") {
          assert.equal(JSON.parse(options.body).password, "synthetic-password");
          return response({ page: { type: "mfa_challenge" }, "oai-client-auth-session": {
            mfa_factors: [{ factor_type: "totp", id: "factor" }],
          } });
        }
        if (url.pathname === "/api/accounts/mfa/issue_challenge") return response({});
        if (url.pathname === "/api/accounts/mfa/verify") {
          assert.equal(JSON.parse(options.body).code, "287082");
          return response({ continue_url: "https://auth.openai.com/sign-in-with-chatgpt/codex/consent",
            "oai-client-auth-session": { workspaces: [{ id: "workspace" }] } });
        }
        if (url.pathname === "/api/accounts/workspace/select") return response({
          continue_url: `${callbackUrl}?code=synthetic-code&state=${authorization.searchParams.get("state")}`,
        });
        if (url.pathname === "/oauth/token") {
          const form = new URLSearchParams(options.body);
          assert.equal(form.get("client_id"), clientId);
          assert.equal(form.get("redirect_uri"), callbackUrl);
          assert.equal(form.get("code"), "synthetic-code");
          const { createHash } = await import("node:crypto");
          assert.equal(createHash("sha256").update(form.get("code_verifier")).digest("base64url"), authorization.searchParams.get("code_challenge"));
          assert.equal(delivered, undefined, "progress must precede token delivery");
          assert.deepEqual(progress, ["authorize", "email", "password", "totp", "workspace", "token_exchange"]);
          return response({ access_token: token({ client_id: clientId, sub: "subject", email: "fixture@example.invalid",
            exp: 3600, "https://api.openai.com/auth": { chatgpt_account_id: "workspace" } }),
            refresh_token: "synthetic-refresh", expires_in: 3000 });
        }
        assert.fail(`Unexpected path ${url.pathname}`);
      },
    };
    await runManagedOAuthLogin({ args: { email: "fixture@example.invalid", oauthProvider: provider, targetWorkspaceId: "workspace",
      oauthProfileRef: "oauth_12345678-1234-4234-8234-123456789abc" }, transport,
      authBase: "https://auth.openai.com", chatgptBase: "https://chatgpt.com" });
    assert.equal(delivered.type, "oauth_tokens");
    assert.equal(delivered.record.provider, provider);
    assert.equal(delivered.record.workspaceId, "workspace");
    assert.equal(calls.filter((call) => call.endsWith("/oauth/token")).length, 1);
  });
}
