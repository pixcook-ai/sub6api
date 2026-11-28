import crypto from "node:crypto";

export async function loginBpsExcelPassword(client, options, helpers) {
  const { authBase, email, rl, password, totpSecret, transaction, targetWorkspaceId } = options;
  if (authBase !== "https://auth.openai.com") {
    throw new Error("EXCEL_AUTH_ORIGIN_INVALID");
  }

  client.onProgress?.("authorize");
  let page = await client.follow(transaction.authorizationUrl);
  const deviceId = client.jar.value("oai-did", authBase) || crypto.randomUUID();
  if (!helpers.isPasswordLoginPage(page.finalUrl)) {
    if (!helpers.isAuthLoginPage(page.finalUrl)) {
      throw new Error("EXCEL_UNEXPECTED_INITIAL_PAGE");
    }
    client.onProgress?.("email");
    const headers = await helpers.createSentinelHeaders(client, {
      authBase,
      deviceId,
      flow: "authorize_continue",
    });
    const { data } = await helpers.authJsonStep(
      client,
      authBase,
      "POST",
      "/api/accounts/authorize/continue",
      { username: { kind: "email", value: email } },
      { referer: page.finalUrl, headers },
    );
    const next = helpers.getContinueUrl(data);
    if (!next || !helpers.isPasswordLoginPage(next)) {
      throw new Error("EXCEL_ADDITIONAL_VERIFICATION_REQUIRED");
    }
    page = await client.follow(next, { referer: page.finalUrl });
  }

  if (!helpers.isPasswordLoginPage(page.finalUrl)) {
    throw new Error("EXCEL_PASSWORD_PAGE_REQUIRED");
  }

  let payload = await helpers.verifyPassword(client, {
    authBase,
    rl,
    deviceId,
    password,
    referer: page.finalUrl,
  });
  const mfaRequired = helpers.isMfaChallengePayload(payload);
  payload = await helpers.completeTotpMfaIfNeeded(client, {
    authBase,
    rl,
    deviceId,
    payload,
    totpSecret,
    referer: page.finalUrl,
  });

  const directCallback = transaction.isCallback(helpers.getContinueUrl(payload))
    && !Array.isArray(payload?.["oai-client-auth-session"]?.workspaces);
  const selectedWorkspaceId = directCallback
    ? null
    : helpers.pickWorkspaceId(payload, targetWorkspaceId);
  payload = await helpers.selectChatgptLoginWorkspaceIfNeeded(client, {
    authBase,
    payload,
    targetWorkspaceId,
  });

  const continued = await helpers.continueFlow(client, payload);
  const callbackUrl = continued?.finalUrl || helpers.getContinueUrl(payload);
  let callback;
  try {
    callback = new URL(callbackUrl);
  } catch {
    throw new Error("EXCEL_CALLBACK_STATE_MISMATCH");
  }
  const states = callback.searchParams.getAll("state");
  const codes = callback.searchParams.getAll("code");
  if (!transaction.isCallback(callbackUrl) || states.length !== 1
    || states[0] !== transaction.state || codes.length !== 1 || !codes[0]) {
    throw new Error("EXCEL_CALLBACK_STATE_MISMATCH");
  }

  return {
    callbackUrl,
    workspaceId: targetWorkspaceId || selectedWorkspaceId || undefined,
    mfaVerified: mfaRequired,
  };
}
