# Sub6 Core OAuth API v1

Base /api/v1/admin/core-oauth, existing admin authentication/RBAC. Profile owner is Sub6; never automatically adopt external managed accounts. All responses use existing response.Success envelope.

## ROOT routes
- GET /capabilities => {available,reason?,providers:[{provider,available,requires_callback_url:true}]}. No stable configured encryption key => available=false; existing server still starts.
- GET /profiles => {profiles:CoreOAuthProfile[]}.
- POST /profiles body {name,provider:codex|bps_excel,proxy_id?} => CoreOAuthProfile.
- GET /profiles/:ref => CoreOAuthProfile.
- POST /profiles/:ref/authorize body {} => CoreOAuthAuthorization.
- POST /profiles/:ref/complete body {attempt_id,callback_url} => CoreOAuthProfile. Full official callback URL <=16384 bytes, current authenticated admin must be initiating actor.
- POST /profiles/:ref/cancel body {attempt_id} => CoreOAuthProfile, same actor check.
- POST /profiles/:ref/refresh body {expected_revision} => CoreOAuthProfile.
- POST /profiles/:ref/handoff-invitations => CoreOAuthHandoffInvitation.
- POST /profiles/:ref/handoffs/accept => CoreOAuthHandoffReceipt.
- GET /profiles/:ref/handoffs/:operation_id => CoreOAuthHandoffReceipt.
- POST /profiles/:ref/handoffs/:operation_id/abandon body {} => CoreOAuthHandoffReceipt.

## Go service contract
NewCoreOAuthService(repo CoreOAuthRepository, encryptor SecretEncryptor, protocols map[string]CoreOAuthProtocol, proxyRepo ProxyRepository) (*CoreOAuthService,error).
NewCoreOAuthBPSProtocol() CoreOAuthProtocol; NewCoreOAuthCodexProtocol(oauthService *OpenAIOAuthService) CoreOAuthProtocol.
Repository NewCoreOAuthRepository(db *sql.DB) service.CoreOAuthRepository; reuses settings private namespace, encrypted payload and CAS, no migration.

- CreateProfile(ctx, CoreOAuthCreateInput) (*CoreOAuthProfile,error).
- ListProfiles(ctx) ([]CoreOAuthProfile,error); GetProfile(ctx,ref) (*CoreOAuthProfile,error).
- StartAuthorization(ctx,ref string,actorID int64) (*CoreOAuthAuthorization,error).
- CompleteAuthorization(ctx,ref string,CoreOAuthCompleteInput) (*CoreOAuthProfile,error).
- CancelAuthorization(ctx,ref,attemptID string,actorID int64) (*CoreOAuthProfile,error).
- Refresh(ctx,ref string,expectedRevision int64) (*CoreOAuthProfile,error).
- RecoverUnauthorized(ctx,CoreOAuthUnauthorizedInput) (*CoreOAuthProfile,error).
- CredentialSnapshot(ctx,ref string) (*CoreOAuthCredentialSnapshot,error).

CoreOAuthCreateInput: Provider string, Name string, ProxyID *int64.
CoreOAuthCompleteInput: AttemptID string, CallbackURL string, ActorID int64.
CoreOAuthUnauthorizedInput: ProfileRef string, IdentityEpoch string, FailedRevision int64, StatusCode int, Origin string. Only origin upstream_oauth with status401 can refresh; stale revisions never repeat it.

## Safe DTO
CoreOAuthProfile wire keys: profile_ref,provider,name,status,authorized,needs_authorization,subject,workspace_id,email,client_id,identity_epoch,token_revision,expires_at,attempt,last_error_code,created_at,updated_at.
expires_at: RFC3339 string or null; attempt: null or {attempt_id,provider,status,expires_at}. Fields for unknown identity are empty strings; token_revision initially0.
Profile status: empty,authorizing,authorized,refreshing,refresh_uncertain,reauthorization_required,handoff_pending. `handoff_pending` is a sealed, non-authorized receiver transaction; refresh, authorization, 401 recovery and credential snapshots remain blocked, including after expiry, until protected acceptance or explicit abandonment by the original administrator and operation ID. Abandonment erases staged payload/private key, retains a terminal receipt, and permits a fresh native authorization; it never restores source authority. Protected acceptance atomically imports the validated grant and records an accepted receipt; accepted cannot be abandoned. It does not install/select runtime accounts or change deployment/group/scheduling choices. `authorized` is true only when an existing usable grant exists; starting authorization preserves an earlier usable grant.
Attempt status: pending,exchanging,consumed,canceled,expired,failed. No actor/state/verifier/callback/tokens in profile DTO.
CoreOAuthAuthorization: {profile:CoreOAuthProfile,attempt_id,authorization_url,redirect_uri,expires_at}.
ActorID is trusted middleware context, never client JSON. Refresh token/access token/ID token never returned.

Errors: CORE_OAUTH_UNAVAILABLE503; CORE_OAUTH_INVALID_INPUT400; CORE_OAUTH_PROFILE_NOT_FOUND404; CORE_OAUTH_CONFLICT409; CORE_OAUTH_BUSY409; CORE_OAUTH_ATTEMPT_EXPIRED410; CORE_OAUTH_ATTEMPT_FORBIDDEN403; CORE_OAUTH_AUTHORIZATION_REQUIRED409; CORE_OAUTH_REFRESH_UNCERTAIN409; CORE_OAUTH_PROVIDER_REJECTED400; CORE_OAUTH_REGION_UNSUPPORTED400; CORE_OAUTH_STORAGE_FAILED503; CORE_OAUTH_HANDOFF_EXPIRED409 (receiver remains locked). Only fixed safe error text. CORE_OAUTH_REGION_UNSUPPORTED is emitted only for the fixed provider classification `unsupported_country_region_territory`; provider response bodies and arbitrary provider error codes are never persisted or returned.

## D runtime seam
CoreOAuthCredentialSnapshot: Profile CoreOAuthProfile, AccessToken string; AccessToken has json omit tag. No RT/ID token. CredentialSnapshot refreshes an expired grant through this core authority. RT is committed before snapshot publication; unknown refresh fails closed.
Distinct account Extra marker core_oauth, contract sub6-core-oauth/v1. CoreOAuthAccountBinding fields Contract/ProfileRef/Provider/Subject/WorkspaceID/IdentityEpoch/TokenRevision/ExpiresAt, wire snake_case. CoreOAuthAccountBindingForSnapshot(snapshot) and CoreOAuthAccountBindingFromAccount(account) return (*CoreOAuthAccountBinding,error). Reject external managed marker, RT-bearing runtime account, provider/platform mismatch or malformed identity/version/expiry. D owns runtime account CAS/install and must consume current profile identity/revision and AT as one snapshot. No external-owner fence bypass.

## Restart and refresh ownership
Private encrypted profile includes grant, actor-bound attempt metadata and durable refresh operation marker. PKCE objects remain process-local; unfinished attempts after process restart explicitly expire and require a new authorization. Profile/RT remains durable. Cross-instance CAS claim precedes token request; sent/unknown refresh is never blindly retried with old RT. No dependency on toSub2 process/Keychain.

## Protected handoff

The authenticated invitation/accept/read/abandon routes above implement `sub6-core-oauth-handoff/v1`. Exact wire fields, raw base64url RSA-OAEP-SHA256 + AES-256-GCM encoding, source quiescence, publication-fence proof verification, operation order and recovery are defined once in [PROTECTED-HANDOFF.md](PROTECTED-HANDOFF.md).

`CreateHandoffInvitation(ctx,ref,CoreOAuthHandoffInvitationInput)`, `AcceptProtectedHandoff(ctx,ref,actorID,envelope,verifier)`, `GetHandoff(ctx,ref,operationID,actorID)` and `AbandonHandoff(ctx,ref,operationID,actorID)` return only public invitation metadata or safe receipts. `BeginHandoff` remains an internal opaque staging seam, not an alternate HTTP import path. The production handler requires live proof verification against saved accounts/config; nil verifier fails closed.

Receipt keys: operation_id,provider,profile_ref,source_profile_ref,subject,workspace_id,identity_epoch,token_revision,status,expires_at,transfer_supported. Only status accepted has transfer_supported=true; pending/abandoned do not. Source references use toSub2 `oauth_<uuid>`, destinations use Sub6 UUIDs. Same-operation retries retain the original invitation expiry and receiver receipt. Accepted retries require the same envelope digest.

Codex publisher revocation uses authenticated POST/GET `/api/v1/admin/accounts/:id/oauth-publication-fence`; BPS proof is a read-only GET `/internal/bps-files/v1/publication-fence` through the independently configured Files service key. Source stopping and submitted inventory entries are verified within the documented scope; unknown external-system inventory completeness is not proven. Only bound-account, single-target transfer is supported.

Source cancellation is separate from receiver abandonment: toSub2 abort without a transfer journal retains the operation_id-only request; an existing transfer also requires target:{baseUrl,adminApiKey}. Before remote revocation starts, it durably records abort_pending, confirms the same receiver operation is abandoned and persists that receipt under the coordinator operation lock, then recovers the source and records aborted. It first GETs the exact receiver operation: an existing pending/abandoned receipt, including an expired invitation, is matched and abandoned/read back without reposting the invitation. Only when no local invitation exists and GET returns 404/409 does it POST a same-operation cancellation reservation and read back regardless of POST outcome. Neither 404/409 nor an empty profile is proof of non-activation. Only an absent cancellation reservation can use a new short TTL; an existing invitation remains immutable. Once remote revocation starts, source recovery is forbidden, including unknown outcomes. See the protected handoff recovery table for the complete ordering.

The protected code path has local synthetic validation; the cancellation recovery increment still requires affected-range regression evidence. Real OAuth, credential migration and RT rotation remain unverified. See [verification record](HANDOFF-RECOVERY-VERIFICATION.md).
