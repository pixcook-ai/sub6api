# Sub6 Core OAuth Integration Verification

Date: 2026-11-24
Branch: `codex/sub6-core-oauth-integrated-20261124`

## Changes

- `df49d4398` adds a persistence-failure regression for OAuth callback exchange. If the provider exchange succeeds but the final profile CAS fails, the persisted `exchanging` operation remains a fence after restart; no profile is returned and the callback is not replayed.
- `3d9112ec1` maps only `CORE_OAUTH_REGION_UNSUPPORTED` in the existing Excel OAuth account-creation flow to localized proxy guidance. Provider response text is not displayed.

## Verification

- Backend: `go test -tags=unit ./internal/service ./internal/handler/admin ./internal/repository ./internal/server/routes -run "CoreOAuth|CoreRuntime|ManagedDeployment.*Switch" -count=1` passed.
- Frontend: CreateAccountModal and CoreOAuthView suites passed, 77/77.
- `git diff --check` passed; integration worktree is clean.
- The full `internal/service` unit suite was also run. It failed only `TestResponsesSearchBuiltinTranslation` and `TestResponsesSearchAdditionalToolsAndStringNamesAvoidCollisions`; those failures are outside these OAuth changes.

## Acceptance Boundary

The local synthetic Sub6 OAuth SDK E2E record at `/Users/leo/Projects/pixcook-ai/.verification/sub6-core-oauth-20261124-current-e2e-local.json` reports 59 steps passed and owned-resource cleanup passed. It explicitly does not prove real official OAuth, real refresh-token rotation, real BPS model/file understanding, or billing. This change does not close those gates.
