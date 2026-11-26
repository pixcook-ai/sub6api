const LOGIN_STAGES = new Set([
  "codex_chatgpt_preflight",
  "bps_auth_preflight",
  "codex_chatgpt_login",
  "codex_oauth",
  "codex_token_exchange",
  "bps_oauth_login",
  "bps_token_exchange",
]);

const TRANSPORT_FAILURES = new Set(["proxy_error", "tls_error", "risk_control"]);

export function classifyLoginFailure(raw) {
  const text = String(raw || "");
  if (/EMAIL_OTP_REQUIRED/.test(text)) return "email_otp_required";
  if (/EXCEL_ADDITIONAL_VERIFICATION_REQUIRED/.test(text)) return "additional_verification_required";
  if (/EXCEL_CALLBACK_STATE_MISMATCH/.test(text)) return "callback_state_mismatch";
  if (/CODEX_ADDITIONAL_VERIFICATION_REQUIRED/.test(text)) return "additional_verification_required";
  if (/CODEX_CALLBACK_STATE_MISMATCH/.test(text)) return "callback_state_mismatch";
  if (/PROXY_(?:RISK_CONTROL|SECURITY_CHECK)|security-check|cloudflare|HTTP 403/i.test(text)) return "risk_control";
  if (/TLS connect error|SSLError|CertificateVerify/i.test(text)) return "tls_error";
  if (/PROXY_|proxy/i.test(text)) return "proxy_error";
  if (/Expected JSON/i.test(text)) return "unexpected_response";
  if (/UNEXPECTED_LOGIN_PAGE/.test(text)) return "unexpected_login_page";
  if (/password/i.test(text) && /(incorrect|invalid|wrong|fail|reject)/i.test(text)) return "password_rejected";
  if (/(mfa|totp|2fa|otp|verification code)/i.test(text)) return "mfa_failed";
  if (/phone/i.test(text)) return "phone_required";
  if (/SESSION_SELECTION|session selection/i.test(text)) return "session_selection";
  if (/WORKSPACE/i.test(text)) return "workspace_unavailable";
  const match = /^([A-Z][A-Z_]{4,}):/.exec(text);
  return match ? match[1].toLowerCase() : "";
}

export function stageLoginFailure(stage, category) {
  if (!LOGIN_STAGES.has(stage)) return category;
  if (!TRANSPORT_FAILURES.has(category)) return category;
  return `${stage}_${category}`;
}
