import assert from "node:assert/strict";
import { test } from "node:test";
import {
  BPS_EXCEL_CALLBACK,
  BPS_EXCEL_CLIENT_ID,
  createBpsOAuthTransaction,
} from "./bps-oauth.mjs";
import { loginBpsExcelPassword } from "./bps-excel-password-flow.mjs";

const authBase = "https://auth.openai.com";

function fixture(overrides = {}) {
  const transaction = createBpsOAuthTransaction({
    email: "fixture@example.invalid",
    profileRef: "oauth_12345678-1234-4234-8234-123456789abc",
  });
  const calls = [];
  const client = {
    jar: { value: () => "fixture-device" },
    follow: async (url) => {
      calls.push(["follow", url]);
      if (url === transaction.authorizationUrl) {
        return { finalUrl: `${authBase}/log-in` };
      }
      return { finalUrl: url };
    },
  };
  const options = {
    authBase,
    email: "fixture@example.invalid",
    password: "synthetic-password",
    totpSecret: "synthetic-totp",
    transaction,
    targetWorkspaceId: "workspace-fixture",
  };
  const helpers = {
    isPasswordLoginPage: (url) => new URL(url).pathname === "/log-in/password",
    isAuthLoginPage: (url) => new URL(url).pathname === "/log-in",
    getContinueUrl: (payload) => payload?.continue_url,
    createSentinelHeaders: async () => ({ fixture: "sentinel" }),
    authJsonStep: async (_client, base, method, path, body) => {
      calls.push(["continue", method, path]);
      assert.equal(base, authBase);
      assert.equal(body.username.value, options.email);
      return { data: { continue_url: `${base}/log-in/password` } };
    },
    verifyPassword: async (_client, args) => {
      calls.push(["password"]);
      assert.equal(args.password, options.password);
      return { mfa: true };
    },
    isMfaChallengePayload: (payload) => Boolean(payload?.mfa),
    completeTotpMfaIfNeeded: async (_client, args) => {
      calls.push(["totp"]);
      assert.equal(args.totpSecret, options.totpSecret);
      return { continue_url: `${authBase}/continue` };
    },
    pickWorkspaceId: (_payload, targetWorkspaceId) => targetWorkspaceId,
    selectChatgptLoginWorkspaceIfNeeded: async (_client, args) => {
      calls.push(["workspace", args.targetWorkspaceId]);
      return args.payload;
    },
    continueFlow: async () => {
      calls.push(["oauth-continue"]);
      return {
        finalUrl: `${BPS_EXCEL_CALLBACK}?code=fixture-code&state=${encodeURIComponent(transaction.state)}`,
      };
    },
    ...overrides,
  };
  return { client, options, helpers, transaction, calls };
}

test("BPS authorization performs email, password, and TOTP inside the Excel PKCE flow", async () => {
  const value = fixture();
  const result = await loginBpsExcelPassword(value.client, value.options, value.helpers);

  assert.equal(result.mfaVerified, true);
  assert.equal(result.workspaceId, "workspace-fixture");
  assert.deepEqual(value.calls.map(([name]) => name), [
    "follow",
    "continue",
    "follow",
    "password",
    "totp",
    "workspace",
    "oauth-continue",
  ]);

  const authorization = new URL(value.transaction.authorizationUrl);
  assert.equal(authorization.searchParams.get("client_id"), BPS_EXCEL_CLIENT_ID);
  assert.equal(authorization.searchParams.get("redirect_uri"), BPS_EXCEL_CALLBACK);
  assert.equal(authorization.searchParams.get("scope"), "openid offline_access email profile organization.read");
  assert.equal(authorization.searchParams.get("audience"), "https://api.openai.com/v1");
  assert.equal(authorization.searchParams.get("platform"), "PC");
  assert.match(authorization.searchParams.get("state"), /^bps\..+\.PC$/);
});

test("BPS flow stops when the authorization route requires another verification step", async () => {
  const value = fixture({
    authJsonStep: async (_client, base) => ({
      data: { continue_url: `${base}/email-verification` },
    }),
  });

  await assert.rejects(
    loginBpsExcelPassword(value.client, value.options, value.helpers),
    /EXCEL_ADDITIONAL_VERIFICATION_REQUIRED/,
  );
  assert.equal(value.calls.some(([name]) => name === "password"), false);
});

test("BPS flow rejects a callback with a different state", async () => {
  const value = fixture({
    continueFlow: async () => ({
      finalUrl: `${BPS_EXCEL_CALLBACK}?code=fixture-code&state=wrong-state`,
    }),
  });

  await assert.rejects(
    loginBpsExcelPassword(value.client, value.options, value.helpers),
    /EXCEL_CALLBACK_STATE_MISMATCH/,
  );
});
