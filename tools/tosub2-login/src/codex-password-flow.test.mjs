import assert from "node:assert/strict";
import { test } from "node:test";
import { CODEX_CALLBACK, CODEX_CLIENT_ID, createCodexOAuthTransaction } from "./codex-oauth.mjs";
import { loginCodexPassword } from "./codex-password-flow.mjs";

const authBase = "https://auth.openai.com";

function fixture(overrides = {}) {
  const transaction = createCodexOAuthTransaction({
    email: "fixture@example.invalid",
    profileRef: "oauth_12345678-1234-4234-8234-123456789abc",
  });
  const callbackUrl = `${CODEX_CALLBACK}?code=fixture-code&state=${transaction.state}`;
  const calls = [];
  const client = {
    jar: { value: () => "fixture-device" },
    follow: async (url) => {
      calls.push(["follow", url]);
      return { finalUrl: url === transaction.authorizationUrl ? `${authBase}/log-in` : url };
    },
  };
  const options = {
    authBase,
    email: "fixture@example.invalid",
    password: "synthetic-password",
    totpSecret: "synthetic-totp",
    transaction,
    targetWorkspaceId: "fixture-workspace",
  };
  const helpers = {
    isPasswordLoginPage: (url) => new URL(url).pathname === "/log-in/password",
    isAuthLoginPage: (url) => new URL(url).pathname === "/log-in",
    getContinueUrl: (payload) => payload?.continue_url,
    createSentinelHeaders: async (_client, args) => {
      calls.push(["sentinel", args.flow]);
      assert.equal(args.deviceId, "fixture-device");
      return { fixture: "sentinel" };
    },
    authJsonStep: async (_client, base, method, pathname, body, requestOptions) => {
      calls.push(["email", pathname]);
      assert.equal(base, authBase);
      assert.equal(method, "POST");
      assert.equal(pathname, "/api/accounts/authorize/continue");
      assert.deepEqual(body, { username: { kind: "email", value: options.email } });
      assert.deepEqual(requestOptions.headers, { fixture: "sentinel" });
      return { data: { continue_url: `${authBase}/log-in/password` } };
    },
    verifyPassword: async (_client, args) => {
      calls.push(["password"]);
      assert.equal(args.password, options.password);
      return { mfa: true, continue_url: `${authBase}/mfa-challenge/fixture` };
    },
    isMfaChallengePayload: (payload) => Boolean(payload?.mfa),
    completeTotpMfaIfNeeded: async (_client, args) => {
      calls.push(["totp"]);
      assert.equal(args.totpSecret, options.totpSecret);
      return {
        continue_url: `${authBase}/workspace`,
        "oai-client-auth-session": { workspaces: [{ id: "fixture-workspace" }] },
      };
    },
    pickWorkspaceId: (_payload, targetWorkspaceId) => targetWorkspaceId,
    selectChatgptLoginWorkspaceIfNeeded: async (_client, args) => {
      calls.push(["workspace", args.targetWorkspaceId]);
      return { continue_url: `${authBase}/continue` };
    },
    continueFlow: async () => {
      calls.push(["continue"]);
      return { finalUrl: callbackUrl };
    },
    ...overrides,
  };
  return { client, options, helpers, transaction, callbackUrl, calls };
}

test("Codex uses its PKCE authorization URL for email, password, TOTP, and workspace selection", async () => {
  const f = fixture();
  const result = await loginCodexPassword(f.client, f.options, f.helpers);

  assert.deepEqual(f.calls.map(([name]) => name), [
    "follow", "sentinel", "email", "follow", "password", "totp", "workspace", "continue",
  ]);
  const authorization = new URL(f.calls[0][1]);
  assert.equal(authorization.origin, authBase);
  assert.equal(authorization.pathname, "/oauth/authorize");
  assert.equal(authorization.searchParams.get("client_id"), CODEX_CLIENT_ID);
  assert.equal(authorization.searchParams.get("redirect_uri"), CODEX_CALLBACK);
  assert.equal(result.callbackUrl, f.callbackUrl);
  assert.equal(result.workspaceId, "fixture-workspace");
  assert.equal(result.mfaVerified, true);
});

test("a completed initial authorization returns its validated callback directly", async () => {
  const f = fixture();
  f.client.follow = async () => ({ finalUrl: f.callbackUrl });

  const result = await loginCodexPassword(f.client, f.options, f.helpers);

  assert.equal(result.callbackUrl, f.callbackUrl);
  assert.equal(result.mfaVerified, false);
  assert.deepEqual(f.calls, []);
});

test("an initial password page proceeds directly to password verification", async () => {
  const f = fixture();
  f.client.follow = async (url) => {
    f.calls.push(["follow", url]);
    return { finalUrl: `${authBase}/log-in/password` };
  };

  const result = await loginCodexPassword(f.client, f.options, f.helpers);

  assert.equal(result.callbackUrl, f.callbackUrl);
  assert.equal(f.calls.some(([name]) => name === "email" || name === "sentinel"), false);
  assert.equal(f.calls.some(([name]) => name === "password"), true);
});

test("a password authorization callback without workspaces skips workspace selection and navigation", async () => {
  const f = fixture();
  f.helpers.completeTotpMfaIfNeeded = async () => ({ continue_url: f.callbackUrl });

  const result = await loginCodexPassword(f.client, f.options, f.helpers);

  assert.equal(result.callbackUrl, f.callbackUrl);
  assert.equal(f.calls.some(([name]) => name === "workspace" || name === "continue"), false);
});

test("email verification remains required before password submission", async () => {
  const f = fixture({
    authJsonStep: async () => ({ data: { continue_url: `${authBase}/email-verification` } }),
  });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /EMAIL_OTP_REQUIRED/);
  assert.equal(f.calls.some(([name]) => name === "password"), false);
});

test("phone verification stops the flow before workspace selection", async () => {
  const f = fixture({
    completeTotpMfaIfNeeded: async () => ({ page: { type: "add_phone" }, continue_url: `${authBase}/add-phone` }),
  });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /PHONE_REQUIRED/);
  assert.equal(f.calls.some(([name]) => name === "workspace"), false);
});

test("an untrusted authorization origin is rejected before navigation", async () => {
  const f = fixture();
  f.options.transaction = { ...f.transaction, authorizationUrl: "https://untrusted.invalid/oauth/authorize" };

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /ORIGIN_INVALID/);
  assert.deepEqual(f.calls, []);
});

test("an untrusted configured authentication origin is rejected before navigation", async () => {
  const f = fixture();
  f.options.authBase = "https://untrusted.invalid";

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /ORIGIN_INVALID/);
  assert.deepEqual(f.calls, []);
});

test("a cross-origin password URL is rejected before following or submitting the password", async () => {
  const f = fixture({
    authJsonStep: async () => ({ data: { continue_url: "https://untrusted.invalid/log-in/password" } }),
  });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /ORIGIN_INVALID/);
  assert.equal(f.calls.filter(([name]) => name === "follow").length, 1);
  assert.equal(f.calls.some(([name]) => name === "password"), false);
});

test("an external final login page is rejected before password submission", async () => {
  const f = fixture();
  f.client.follow = async () => ({ finalUrl: "https://untrusted.invalid/log-in/password" });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /ORIGIN_INVALID/);
  assert.equal(f.calls.some(([name]) => name === "password"), false);
});

test("an external password response continuation is rejected before MFA", async () => {
  const f = fixture({ verifyPassword: async () => ({ mfa: true, continue_url: "https://untrusted.invalid/mfa-challenge/x" }) });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /ORIGIN_INVALID/);
  assert.equal(f.calls.some(([name]) => name === "totp"), false);
});

test("a mismatched initial callback is rejected before invoking authentication helpers", async () => {
  const f = fixture();
  f.client.follow = async () => ({ finalUrl: `${CODEX_CALLBACK}?code=fixture-code&state=wrong` });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /CALLBACK_STATE_MISMATCH/);
  assert.deepEqual(f.calls, []);
});

test("a mismatched callback returned after continuation is rejected", async () => {
  const f = fixture({
    continueFlow: async () => ({ finalUrl: `${CODEX_CALLBACK}?code=fixture-code&state=wrong` }),
  });

  await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /CALLBACK_STATE_MISMATCH/);
});

test("callback state, code, and duplicate parameters are rejected before continuation", async () => {
  for (const query of [
    "code=fixture-code&state=wrong",
    "code=fixture-code&state=STATE&state=STATE",
    "code=fixture-code&code=duplicate&state=STATE",
    "state=STATE",
    "code=&state=STATE",
    "code=fixture-code&state=STATE&error=access_denied",
    "code=fixture%20code&state=STATE",
  ]) {
    const f = fixture();
    const callback = `${CODEX_CALLBACK}?${query.replaceAll("STATE", f.transaction.state)}`;
    f.helpers.completeTotpMfaIfNeeded = async () => ({ continue_url: callback });

    await assert.rejects(loginCodexPassword(f.client, f.options, f.helpers), /CALLBACK_STATE_MISMATCH/);
    assert.equal(f.calls.some(([name]) => name === "workspace" || name === "continue"), false);
  }
});
