import crypto from "node:crypto";

export const BPS_EXCEL_CLIENT_ID = "app_fnr0pYvVwwFDocDumLG3H2Bp";
export const BPS_EXCEL_CALLBACK = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback";
const PROFILE = /^oauth_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const AUTH = "https://auth.openai.com";
const SCOPE = "openid offline_access email profile organization.read";
const AUDIENCE = "https://api.openai.com/v1";
const fail = (code) => Object.assign(new Error(code), { code });
const nowMs = (now) => typeof now === "function" ? now() : now ?? Date.now();

export function bpsAuthBase(value = AUTH) {
  let url;
  try { url = new URL(value); } catch { throw fail("bps_invalid_auth_origin"); }
  const fixture = process.env.NODE_ENV === "test" && url.protocol === "http:"
    && ["127.0.0.1", "localhost", "[::1]"].includes(url.hostname);
  if ((url.origin !== AUTH && !fixture) || url.username || url.password || url.search || url.hash || url.pathname !== "/") {
    throw fail("bps_invalid_auth_origin");
  }
  return url.origin;
}

export function isBpsOAuthCallback(value) {
  try {
    if (typeof value !== "string" || value.split("?", 1)[0] !== BPS_EXCEL_CALLBACK) return false;
    const url = new URL(value);
    const callback = new URL(BPS_EXCEL_CALLBACK);
    return url.origin === callback.origin && url.pathname === callback.pathname
      && !url.username && !url.password && !url.hash;
  } catch { return false; }
}

function normalizedEmail(value) {
  const email = typeof value === "string" ? value.trim().toLowerCase() : "";
  if (email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email) || /[\x00-\x1f\x7f]/.test(email)) {
    throw fail("bps_invalid_email");
  }
  return email;
}

function tokenText(value, required = true) {
  if (typeof value !== "string" || value.length > 65_536 || (required && !value) || /[\x00-\x20\x7f]/.test(value)) {
    throw fail("bps_invalid_token_response");
  }
  return value;
}

function claims(token) {
  const parts = token.split(".");
  if (parts.length !== 3 || !parts.every(Boolean)) throw fail("bps_invalid_token_claims");
  try {
    const value = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error();
    return value;
  } catch { throw fail("bps_invalid_token_claims"); }
}

function one(values, code) {
  const present = values.filter((value) => value !== undefined && value !== null);
  if (!present.length || present.some((value) => typeof value !== "string" || !value || value !== value.trim()
    || value.length > 1024 || /[\x00-\x1f\x7f]/.test(value)) || new Set(present).size !== 1) throw fail(code);
  return present[0];
}

// Claims are cross-checked for identity consistency, not cryptographically verified.
// Production tokens must arrive directly from the fixed HTTPS token endpoint.
export function validateBpsTokenResponse(payload, options = {}) {
  const { profileRef, previous = null, expectedIdentity = {} } = options;
  if (!PROFILE.test(profileRef || "")) throw fail("bps_invalid_profile_ref");
  const email = normalizedEmail(options.email ?? previous?.email);
  if (!payload || typeof payload !== "object" || Array.isArray(payload)
    || (payload.token_type && String(payload.token_type).toLowerCase() !== "bearer")) throw fail("bps_invalid_token_response");
  if (payload.client_id !== undefined && payload.client_id !== BPS_EXCEL_CLIENT_ID) throw fail("bps_client_mismatch");
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
    if (!audience.includes(AUDIENCE)) throw fail("bps_audience_mismatch");
  }
  const instant = nowMs(options.now);
  const issuer = bpsAuthBase(options.authBase);
  for (const c of [access, ...(idToken ? [id] : [])]) {
    if (c.iss !== undefined && c.iss !== issuer && c.iss !== `${issuer}/`) throw fail("bps_issuer_mismatch");
    if (c.exp !== undefined && (!Number.isFinite(c.exp) || c.exp * 1000 <= instant)) throw fail("bps_token_expired");
    for (const client of [c.client_id, c.azp]) if (client !== undefined && client !== BPS_EXCEL_CLIENT_ID) throw fail("bps_client_mismatch");
  }
  if (idToken) {
    const audience = Array.isArray(id.aud) ? id.aud : [id.aud];
    if (audience.length !== 1 || audience[0] !== BPS_EXCEL_CLIENT_ID) throw fail("bps_client_mismatch");
    if (id.email_verified === false) throw fail("bps_email_unverified");
  }
  const returnedClients = [payload.client_id, access.client_id, access.azp, id.client_id, id.azp, ...(idToken ? [BPS_EXCEL_CLIENT_ID] : [])].filter((value) => value !== undefined);
  if (!returnedClients.length || returnedClients.some((value) => value !== BPS_EXCEL_CLIENT_ID)) throw fail("bps_client_mismatch");
  one([access.sub, id.sub], "bps_subject_mismatch");
  one([auth.chatgpt_account_id, idAuth.chatgpt_account_id, payload.chatgpt_account_id], "bps_workspace_mismatch");
  const subject = one([access.sub, id.sub, previous?.subject, expectedIdentity.subject], "bps_subject_mismatch");
  const workspaceId = one([auth.chatgpt_account_id, idAuth.chatgpt_account_id, payload.chatgpt_account_id, previous?.workspaceId, expectedIdentity.workspaceId], "bps_workspace_mismatch");
  const tokenEmails = [profile.email, access.email, id.email, payload.email].filter((value) => value !== undefined).map(normalizedEmail);
  if (!tokenEmails.length || tokenEmails.some((value) => value !== email) || (previous && previous.email !== email)
    || (expectedIdentity.email && normalizedEmail(expectedIdentity.email) !== email)) throw fail("bps_email_mismatch");
  if (previous && (previous.provider !== "bps_excel" || previous.profileRef !== profileRef || previous.clientId !== BPS_EXCEL_CLIENT_ID)) throw fail("bps_profile_mismatch");
  const expires = [];
  if (payload.expires_in !== undefined) {
    if (!Number.isFinite(payload.expires_in) || payload.expires_in <= 0 || payload.expires_in > 366 * 86400) throw fail("bps_invalid_token_expiry");
    expires.push(instant + payload.expires_in * 1000);
  }
  if (Number.isFinite(access.exp)) expires.push(access.exp * 1000);
  if (!expires.length) throw fail("bps_invalid_token_expiry");
  return {
    provider: "bps_excel", profileRef, email, subject, workspaceId, clientId: BPS_EXCEL_CLIENT_ID,
    accessToken, refreshToken, idToken, expiresAt: new Date(Math.min(...expires)).toISOString(),
    tokenRevision: previous ? previous.tokenRevision + 1 : 1, identityEpoch: previous?.identityEpoch ?? crypto.randomUUID(),
  };
}

async function requestTokens(form, { authBase, fetchImpl = fetch, refresh = false }) {
  const endpoint = `${bpsAuthBase(authBase)}/oauth/token?unified=true`;
  let response;
  try {
    response = await fetchImpl(endpoint, {
      method: "POST", headers: { "content-type": "application/x-www-form-urlencoded", accept: "application/json" },
      body: new URLSearchParams(form).toString(), redirect: "error", signal: AbortSignal.timeout(30_000),
    });
  } catch { throw fail(refresh ? "bps_refresh_outcome_unknown" : "bps_token_exchange_outcome_unknown"); }
  let payload;
  try { payload = await response.json(); } catch { throw fail(refresh ? "bps_refresh_outcome_unknown" : "bps_invalid_token_response"); }
  if (!response.ok) {
    if (payload?.error === "invalid_grant") throw fail("bps_invalid_grant");
    throw fail(refresh && response.status >= 500 ? "bps_refresh_outcome_unknown" : "bps_token_endpoint_rejected");
  }
  return payload;
}

export function createBpsOAuthTransaction(options = {}) {
  const { profileRef } = options;
  if (!PROFILE.test(profileRef || "")) throw fail("bps_invalid_profile_ref");
  const email = normalizedEmail(options.email);
  const authBase = bpsAuthBase(options.authBase);
  const ttlMs = options.ttlMs ?? 10 * 60_000;
  if (!Number.isSafeInteger(ttlMs) || ttlMs < 1 || ttlMs > 30 * 60_000) throw fail("bps_invalid_transaction_expiry");
  const expiresAt = nowMs(options.now) + ttlMs;
  const state = `bps.${crypto.randomBytes(24).toString("base64url")}.PC`;
  let verifier = crypto.randomBytes(48).toString("base64url");
  let consumed = false;
  const authorization = new URL(`${authBase}/api/accounts/authorize`);
  authorization.search = new URLSearchParams({
    client_id: BPS_EXCEL_CLIENT_ID, response_type: "code", redirect_uri: BPS_EXCEL_CALLBACK,
    scope: SCOPE, audience: AUDIENCE, code_challenge_method: "S256",
    code_challenge: crypto.createHash("sha256").update(verifier).digest("base64url"), state, platform: "PC",
  }).toString();
  return Object.freeze({
    profileRef, authorizationUrl: authorization.toString(), redirectUri: BPS_EXCEL_CALLBACK, state, expiresAt,
    isCallback: isBpsOAuthCallback,
    async exchangeCallback(value, exchangeOptions = {}) {
      if (!isBpsOAuthCallback(value)) throw fail("bps_callback_mismatch");
      if (consumed) throw fail("bps_oauth_transaction_consumed");
      if (nowMs(options.now) >= expiresAt) { consumed = true; verifier = ""; throw fail("bps_oauth_transaction_expired"); }
      const callback = new URL(value);
      for (const key of callback.searchParams.keys()) {
        if (!["state", "code"].includes(key) && callback.searchParams.getAll(key).length !== 1) throw fail("bps_duplicate_callback_parameter");
      }
      const states = callback.searchParams.getAll("state");
      if (states.length !== 1 || Buffer.byteLength(states[0]) !== Buffer.byteLength(state)
        || !crypto.timingSafeEqual(Buffer.from(states[0]), Buffer.from(state))) throw fail("bps_oauth_state_mismatch");
      if (callback.searchParams.has("error")) { consumed = true; verifier = ""; throw fail("bps_authorization_rejected"); }
      const codes = callback.searchParams.getAll("code");
      if (codes.length !== 1 || !codes[0] || codes[0].length > 8192 || /[\x00-\x20\x7f]/.test(codes[0])) throw fail("bps_invalid_authorization_code");
      consumed = true;
      const codeVerifier = verifier;
      verifier = "";
      const payload = await requestTokens({ grant_type: "authorization_code", code: codes[0], client_id: BPS_EXCEL_CLIENT_ID,
        redirect_uri: BPS_EXCEL_CALLBACK, code_verifier: codeVerifier }, { authBase, fetchImpl: exchangeOptions.fetchImpl });
      return validateBpsTokenResponse(payload, { profileRef, email, authBase, now: options.now, expectedIdentity: exchangeOptions.expectedIdentity });
    },
  });
}

export async function refreshBpsOAuth({ record, fetchImpl, authBase, now } = {}) {
  if (!record || record.provider !== "bps_excel" || record.clientId !== BPS_EXCEL_CLIENT_ID || !PROFILE.test(record.profileRef || "")) throw fail("bps_profile_mismatch");
  const payload = await requestTokens({ grant_type: "refresh_token", refresh_token: tokenText(record.refreshToken), client_id: BPS_EXCEL_CLIENT_ID }, { authBase, fetchImpl, refresh: true });
  try { return validateBpsTokenResponse(payload, { profileRef: record.profileRef, email: record.email, previous: record, authBase, now }); }
  catch { throw fail("bps_refresh_outcome_unknown"); }
}
