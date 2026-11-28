import assert from "node:assert/strict";
import test from "node:test";
import { CODEX_CALLBACK, createCodexOAuthTransaction } from "./codex-oauth.mjs";
import { BPS_EXCEL_CALLBACK, createBpsOAuthTransaction } from "./bps-oauth.mjs";
import { classifyLoginFailure, stageLoginFailure } from "./login-failure.mjs";

for (const [provider, createTransaction, callback] of [
  ["codex", createCodexOAuthTransaction, CODEX_CALLBACK],
  ["bps", createBpsOAuthTransaction, BPS_EXCEL_CALLBACK],
]) {
  test(`${provider} token failures preserve HTTP status and fixed error codes`, async () => {
    for (const status of [403, 429, 503]) {
      for (const body of [JSON.stringify({ error: "upstream_error", message: "private-response-secret" }), "private-response-secret"]) {
        const transaction = createTransaction({
          email: "fixture@example.invalid",
          profileRef: "oauth_12345678-1234-4234-8234-123456789abc",
        });
        await assert.rejects(
          transaction.exchangeCallback(`${callback}?code=fixture-code&state=${transaction.state}`, {
            fetchImpl: async () => new Response(body, { status }),
          }),
          (error) => {
            assert.equal(error.code, `${provider}_${body.startsWith("{") ? "token_endpoint_rejected" : "invalid_token_response"}`);
            assert.equal(error.message.includes("private-response-secret"), false);
            const category = classifyLoginFailure(error.message);
            const expected = `${provider}_token_exchange_${status === 403 ? "risk_control_" : ""}http_${status}`;
            assert.equal(stageLoginFailure(`${provider}_token_exchange`, category), expected);
            return true;
          },
        );
      }
    }
  });

  test(`${provider} preserves invalid-grant and transport error behavior`, async () => {
    for (const [fetchImpl, expectedCode, expectedStatus] of [
      [async () => new Response(JSON.stringify({ error: "invalid_grant" }), { status: 400 }), `${provider}_invalid_grant`, 400],
      [async () => { throw new Error("private-transport-secret"); }, `${provider}_token_exchange_outcome_unknown`, null],
    ]) {
      const transaction = createTransaction({
        email: "fixture@example.invalid",
        profileRef: "oauth_12345678-1234-4234-8234-123456789abc",
      });
      await assert.rejects(
        transaction.exchangeCallback(`${callback}?code=fixture-code&state=${transaction.state}`, { fetchImpl }),
        (error) => {
          assert.equal(error.code, expectedCode);
          assert.equal(error.message, expectedCode + (expectedStatus ? ` HTTP ${expectedStatus}` : ""));
          assert.equal(error.message.includes("private-transport-secret"), false);
          return true;
        },
      );
    }
  });
}
