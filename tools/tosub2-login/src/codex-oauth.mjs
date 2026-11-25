import crypto from "node:crypto";

export const CODEX_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann";
export const CODEX_CALLBACK = "http://localhost:1455/auth/callback";
const PROFILE = /^oauth_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const AUTH = "https://auth.openai.com";
const SCOPE = "openid profile email offline_access";
const AUDIENCE = "https://api.openai.com/v1";
const fail = (code) => Object.assign(new Error(code), { code });
const nowMs = (now) => typeof now === "function" ? now() : now ?? Date.now();

export function codexAuthBase(value = AUTH) {
  let url;
  try { url = new URL(value); } catch { throw fail("codex_invalid_auth_origin"); }
  const fixture = process.env.NODE_ENV === "test" && url.protocol === "http:"
    && ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
  if ((url.origin !== AUTH && !fixture) || url.username || url.password || url.search || url.hash || url.pathname !== "/") {
    throw fail("codex_invalid_auth_origin");
  }
  return url.origin;
}

export function isCodexOAuthCallback(value) {
  try {
    if (typeof value !== "string" || value.split("?", 1)[0] !== CODEX_CALLBACK) return false;
    const url = new URL(value);
    const callback = new URL(CODEX_CALLBACK);
    return url.origin === callback.origin && url.pathname === callback.pathname
      && !url.username && !url.password && !url.hash;
  } catch { return false; }
}

function normalizedEmail(value) {
  const email = typeof value === "string" ? value.trim().toLowerCase() : "";
  if (email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email) || /[\x00-\x1f\x7f]/.test(email)) {
    throw fail("codex_invalid_email");
  }
  return email;
}

function tokenText(value, required = true) {
  if (typeof value !== "string" || value.length > 65_536 || (required && !value) || /[\x00-\x20\x7f]/.test(value)) {
    throw fail("codex_invalid_token_response");
  }
  return value;
}

function claims(token) {
  const parts = token.split(".");
  if (parts.length !== 3 || !parts.every(Boolean)) throw fail("codex_invalid_token_claims");
  try {
    const value = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error();
    return value;
  } catch { throw fail("codex_invalid_token_claims"); }
}

function one(values, code) {
  const present = values.filter((value) => value !== undefined && value !== null);
  if (!present.length || present.some((value) => typeof value !== "string" || !value || value !== value.trim()
    || value.length > 1024 || /[\x00-\x1f\x7f]/.test(value)) || new Set(present).size !== 1) throw fail(code);
  return present[0];
}

// Claims are cross-checked for identity consistency, not cryptographically verified.
// Production tokens must arrive directly from the fixed HTTPS token endpoint.
export function validateCodexTokenResponse(payload, options = {}) {
  const { profileRef, previous = null, expectedIdentity = {} } = options;
  if (!PROFILE.test(profileRef || "")) throw fail("codex_invalid_profile_ref");
  const email = normalizedEmail(options.email ?? previous?.email);
  if (!payload || typeof payload !== "object" || Array.isArray(payload)
    || (payload.token_type && String(payload.token_type).toLowerCase() !== "bearer")) throw fail("codex_invalid_token_response");
  if (payload.client_id !== undefined && payload.client_id !== CODEX_CLIENT_ID) throw fail("codex_client_mismatch");
  const accessToken = tokenText(payload.access_token);
  const refreshToken = tokenText(payload.refresh_token ?? previous?.refreshToken);
  const idToken = tokenText(payload.id_token ?? "", false);
  const access = claims(accessToken);
  const id = idToken ? claims(idToken) : {};
  const auth = access["https://api.openai.com/auth"] ?? {};
  const idAuth = id["https://api.openai.com/auth"] ?? {};
  const profile = access["https://api.openai.com/profile"] ?? {};
  if (access.aud !== undefined) {
    const audience = Array.isArray(access.aud) ? access.aud : [access.aud];
    if (!audience.includes(AUDIENCE)) throw fail("codex_audience_mismatch");
  }
  const instant = nowMs(options.now);
  const issuer = codexAuthBase(options.authBase);
  for (const c of [access, ...(idToken ? [id] : [])]) {
    if (c.iss !== undefined && c.iss !== issuer && c.iss !== `${issuer}/`) throw fail("codex_issuer_mismatch");
    if (c.exp !== undefined && (!Number.isFinite(c.exp) || c.exp * 1000 <= instant)) throw fail("codex_token_expired");
    for (const client of [c.client_id, c.azp]) if (client !== undefined && client !== CODEX_CLIENT_ID) throw fail("codex_client_mismatch");
  }
  if (idToken) {
    const audience = Array.isArray(id.aud) ? id.aud : [id.aud];
    if (audience.length !== 1 || audience[0] !== CODEX_CLIENT_ID) throw fail("codex_client_mismatch");
    if (id.email_verified === false) throw fail("codex_email_unverified");
  }
  const returnedClients = [payload.client_id, access.client_id, access.azp, id.client_id, id.azp, ...(idToken ? [CODEX_CLIENT_ID] : [])].filter((value) => value !== undefined);
  if (!returnedClients.length || returnedClients.some((value) => value !== CODEX_CLIENT_ID)) throw fail("codex_client_mismatch");
  one([access.sub, id.sub], "codex_subject_mismatch");
  one([auth.chatgpt_account_id, idAuth.chatgpt_account_id, payload.chatgpt_account_id], "codex_workspace_mismatch");
  const subject = one([access.sub, id.sub, previous?.subject, expectedIdentity.subject], "codex_subject_mismatch");
  const workspaceId = one([auth.chatgpt_account_id, idAuth.chatgpt_account_id, payload.chatgpt_account_id, previous?.workspaceId, expectedIdentity.workspaceId], "codex_workspace_mismatch");
  const tokenEmails = [profile.email, access.email, id.email, payload.email].filter((value) => value !== undefined).map(normalizedEmail);
  if (!tokenEmails.length || tokenEmails.some((value) => value !== email) || (previous && previous.email !== email)
    || (expectedIdentity.email && normalizedEmail(expectedIdentity.email) !== email)) throw fail("codex_email_mismatch");
  if (previous && (previous.provider !== "codex" || previous.profileRef !== profileRef || previous.clientId !== CODEX_CLIENT_ID)) throw fail("codex_profile_mismatch");
  const expires = [];
  if (payload.expires_in !== undefined) {
    if (!Number.isFinite(payload.expires_in) || payload.expires_in <= 0 || payload.expires_in > 366 * 86400) throw fail("codex_invalid_token_expiry");
    expires.push(instant + payload.expires_in * 1000);
  }
  if (Number.isFinite(access.exp)) expires.push(access.exp * 1000);
  if (!expires.length) throw fail("codex_invalid_token_expiry");
  return {
    provider: "codex", profileRef, email, subject, workspaceId, clientId: CODEX_CLIENT_ID,
    accessToken, refreshToken, idToken, expiresAt: new Date(Math.min(...expires)).toISOString(),
    tokenRevision: previous ? previous.tokenRevision + 1 : 1, identityEpoch: previous?.identityEpoch ?? crypto.randomUUID(),
  };
}

async function requestTokens(form, { authBase, fetchImpl = fetch, refresh = false }) {
  const endpoint = `${codexAuthBase(authBase)}/oauth/token`;
  let response;
  try {
    response = await fetchImpl(endpoint, {
      method: "POST", headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" },
      body: new URLSearchParams(form).toString(), redirect: "error", signal: AbortSignal.timeout(30_000),
    });
  } catch { throw fail(refresh ? "codex_refresh_outcome_unknown" : "codex_token_exchange_outcome_unknown"); }
  let payload;
  try { payload = await response.json(); } catch { throw fail(refresh ? "codex_refresh_outcome_unknown" : "codex_invalid_token_response"); }
  if (!response.ok) {
    if (payload?.error === "invalid_grant") throw fail("codex_invalid_grant");
    throw fail(refresh && response.status >= 500 ? "codex_refresh_outcome_unknown" : "codex_token_endpoint_rejected");
  }
  return payload;
}

export function createCodexOAuthTransaction(options = {}) {
  const { profileRef } = options;
  if (!PROFILE.test(profileRef || "")) throw fail("codex_invalid_profile_ref");
  const email = normalizedEmail(options.email);
  const authBase = codexAuthBase(options.authBase);
  const ttlMs = options.ttlMs ?? 10 * 60_000;
  if (!Number.isSafeInteger(ttlMs) || ttlMs < 1 || ttlMs > 30 * 60_000) throw fail("codex_invalid_transaction_expiry");
  const expiresAt = nowMs(options.now) + ttlMs;
  const state = crypto.randomBytes(32).toString("base64url");
  let verifier = crypto.randomBytes(48).toString("base64url");
  let consumed = false;
  const authorization = new URL(`${authBase}/oauth/authorize`);
  authorization.search = new URLSearchParams({
    client_id: CODEX_CLIENT_ID, response_type: "code", redirect_uri: CODEX_CALLBACK,
    scope: SCOPE, codex_cli_simplified_flow: "true", id_token_add_organizations: "true", code_challenge_method: "S256",
    code_challenge: crypto.createHash("sha256").update(verifier).digest("base64url"), state,
  }).toString();
  return Object.freeze({
    profileRef, authorizationUrl: authorization.toString(), redirectUri: CODEX_CALLBACK, state, expiresAt,
    isCallback: isCodexOAuthCallback,
    async exchangeCallback(value, exchangeOptions = {}) {
      if (!isCodexOAuthCallback(value)) throw fail("codex_callback_mismatch");
      if (consumed) throw fail("codex_oauth_transaction_consumed");
      if (nowMs(options.now) >= expiresAt) { consumed = true; verifier = ""; throw fail("codex_oauth_transaction_expired"); }
      const callback = new URL(value);
      for (const key of callback.searchParams.keys()) {
        if (callback.searchParams.getAll(key).length !== 1) throw fail("codex_duplicate_callback_parameter");
      }
      const states = callback.searchParams.getAll("state");
      if (states.length !== 1 || Buffer.byteLength(states[0]) !== Buffer.byteLength(state)
        || !crypto.timingSafeEqual(Buffer.from(states[0]), Buffer.from(state))) throw fail("codex_oauth_state_mismatch");
      if (callback.searchParams.has("error")) { consumed = true; verifier = ""; throw fail("codex_authorization_rejected"); }
      const codes = callback.searchParams.getAll("code");
      if (codes.length !== 1 || !codes[0] || codes[0].length > 8192 || /[\x00-\x20\x7f]/.test(codes[0])) throw fail("codex_invalid_authorization_code");
      consumed = true;
      const codeVerifier = verifier;
      verifier = "";
      const payload = await requestTokens({ grant_type: "authorization_code", code: codes[0], client_id: CODEX_CLIENT_ID,
        redirect_uri: CODEX_CALLBACK, code_verifier: codeVerifier }, { authBase, fetchImpl: exchangeOptions.fetchImpl });
      return validateCodexTokenResponse(payload, { profileRef, email, authBase, now: options.now, expectedIdentity: exchangeOptions.expectedIdentity });
    },
  });
}

export async function refreshCodexOAuth({ record, fetchImpl, authBase, now } = {}) {
  if (!record || record.provider !== "codex" || record.clientId !== CODEX_CLIENT_ID || !PROFILE.test(record.profileRef || "")) throw fail("codex_profile_mismatch");
  const payload = await requestTokens({ grant_type: "refresh_token", refresh_token: tokenText(record.refreshToken), client_id: CODEX_CLIENT_ID }, { authBase, fetchImpl, refresh: true });
  try { return validateCodexTokenResponse(payload, { profileRef: record.profileRef, email: record.email, previous: record, authBase, now }); }
  catch { throw fail("codex_refresh_outcome_unknown"); }
}
