import crypto from "node:crypto";

export async function loginCodexPassword(client, options, helpers) {
  const { authBase, email, rl, password, totpSecret, transaction, targetWorkspaceId } = options;
  if (authBase !== "https://auth.openai.com") throw new Error("CODEX_AUTH_ORIGIN_INVALID");

  function checkUrl(value) {
    let url;
    try { url = new URL(value); } catch { throw new Error("CODEX_AUTH_ORIGIN_INVALID"); }
    if (transaction.isCallback(value)) {
      const states = url.searchParams.getAll("state");
      const codes = url.searchParams.getAll("code");
      const duplicateParameter = [...url.searchParams.keys()].some((key) => url.searchParams.getAll(key).length !== 1);
      if (duplicateParameter || states.length !== 1 || states[0] !== transaction.state
        || codes.length !== 1 || !codes[0] || codes[0].length > 8192
        || /[\x00-\x20\x7f]/.test(codes[0]) || url.searchParams.has("error")) {
        throw new Error("CODEX_CALLBACK_STATE_MISMATCH");
      }
      return true;
    }
    if (url.origin !== authBase || url.username || url.password) throw new Error("CODEX_AUTH_ORIGIN_INVALID");
    if (url.pathname === "/email-verification") throw new Error("EMAIL_OTP_REQUIRED");
    if (url.pathname === "/add-phone") throw new Error("PHONE_REQUIRED");
    return false;
  }

  function checkPayload(payload) {
    const type = payload?.page?.type || "";
    if (/^email[_-](?:verification|otp)/.test(type)) throw new Error("EMAIL_OTP_REQUIRED");
    if (type === "add_phone") throw new Error("PHONE_REQUIRED");
    const next = helpers.getContinueUrl(payload);
    if (next) checkUrl(next);
  }

  const completed = (callbackUrl, mfaVerified = false, workspaceId = targetWorkspaceId) => ({
    callbackUrl,
    workspaceId: workspaceId || undefined,
    mfaVerified,
  });

  if (checkUrl(transaction.authorizationUrl)) throw new Error("CODEX_AUTH_ORIGIN_INVALID");
  client.onProgress?.("authorize");
  let page = await client.follow(transaction.authorizationUrl);
  if (checkUrl(page.finalUrl)) return completed(page.finalUrl);
  const deviceId = client.jar.value("oai-did", authBase) || crypto.randomUUID();
  if (!helpers.isPasswordLoginPage(page.finalUrl)) {
    if (!helpers.isAuthLoginPage(page.finalUrl)) throw new Error("CODEX_ADDITIONAL_VERIFICATION_REQUIRED");
    client.onProgress?.("email");
    const headers = await helpers.createSentinelHeaders(client, { authBase, deviceId, flow: "authorize_continue" });
    const { data } = await helpers.authJsonStep(
      client, authBase, "POST", "/api/accounts/authorize/continue",
      { username: { kind: "email", value: email } },
      { referer: page.finalUrl, headers },
    );
    checkPayload(data);
    const next = helpers.getContinueUrl(data);
    if (!next) throw new Error("CODEX_ADDITIONAL_VERIFICATION_REQUIRED");
    if (checkUrl(next)) return completed(next);
    if (!helpers.isPasswordLoginPage(next)) throw new Error("CODEX_ADDITIONAL_VERIFICATION_REQUIRED");
    page = await client.follow(next, { referer: page.finalUrl });
    if (checkUrl(page.finalUrl)) return completed(page.finalUrl);
  }
  if (!helpers.isPasswordLoginPage(page.finalUrl)) throw new Error("CODEX_PASSWORD_PAGE_REQUIRED");

  let payload = await helpers.verifyPassword(client, {
    authBase, rl, deviceId, password, referer: page.finalUrl,
  });
  checkPayload(payload);
  const mfaRequired = helpers.isMfaChallengePayload(payload);
  payload = await helpers.completeTotpMfaIfNeeded(client, {
    authBase, rl, deviceId, payload, totpSecret, referer: page.finalUrl,
  });
  checkPayload(payload);
  const next = helpers.getContinueUrl(payload);
  if (next && transaction.isCallback(next) && !Array.isArray(payload?.["oai-client-auth-session"]?.workspaces)) {
    return completed(next, mfaRequired);
  }

  const workspaceId = helpers.pickWorkspaceId(payload, targetWorkspaceId);
  payload = await helpers.selectChatgptLoginWorkspaceIfNeeded(client, {
    authBase, payload, targetWorkspaceId: targetWorkspaceId || workspaceId || undefined,
  });
  checkPayload(payload);
  const selectedNext = helpers.getContinueUrl(payload);
  if (selectedNext && transaction.isCallback(selectedNext)) {
    return completed(selectedNext, mfaRequired, targetWorkspaceId || workspaceId);
  }
  const continued = await helpers.continueFlow(client, payload);
  const callbackUrl = continued?.finalUrl || selectedNext;
  if (!callbackUrl || !checkUrl(callbackUrl)) throw new Error("CODEX_CALLBACK_STATE_MISMATCH");
  return completed(callbackUrl, mfaRequired, targetWorkspaceId || workspaceId);
}
