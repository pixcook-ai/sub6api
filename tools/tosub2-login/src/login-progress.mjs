const STAGES = new Set([
  "proxy_check", "timezone_check", "authorize", "email", "password", "totp", "workspace", "token_exchange",
]);

export function isLoginProgressStage(stage) {
  return STAGES.has(stage);
}

export function reportLoginProgress(profileRef, stage) {
  if (!isLoginProgressStage(stage) || !process.connected) return;
  process.send({ type: "oauth_progress", profileRef, stage }, () => {});
}
