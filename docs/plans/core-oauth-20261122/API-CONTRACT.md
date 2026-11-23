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
Profile status: empty,authorizing,authorized,refreshing,refresh_uncertain,reauthorization_required. authorized is true only when an existing usable grant exists; starting authorization preserves an earlier usable grant.
Attempt status: pending,exchanging,consumed,canceled,expired,failed. No actor/state/verifier/callback/tokens in profile DTO.
CoreOAuthAuthorization: {profile:CoreOAuthProfile,attempt_id,authorization_url,redirect_uri,expires_at}.
ActorID is trusted middleware context, never client JSON. Refresh token/access token/ID token never returned.

Errors: CORE_OAUTH_UNAVAILABLE503; CORE_OAUTH_INVALID_INPUT400; CORE_OAUTH_PROFILE_NOT_FOUND404; CORE_OAUTH_CONFLICT409; CORE_OAUTH_BUSY409; CORE_OAUTH_ATTEMPT_EXPIRED410; CORE_OAUTH_ATTEMPT_FORBIDDEN403; CORE_OAUTH_AUTHORIZATION_REQUIRED409; CORE_OAUTH_REFRESH_UNCERTAIN409; CORE_OAUTH_PROVIDER_REJECTED400; CORE_OAUTH_REGION_UNSUPPORTED400; CORE_OAUTH_STORAGE_FAILED503. Only fixed safe error text. CORE_OAUTH_REGION_UNSUPPORTED is emitted only for the fixed provider classification `unsupported_country_region_territory`; provider response bodies and arbitrary provider error codes are never persisted or returned.

## D runtime seam
CoreOAuthCredentialSnapshot: Profile CoreOAuthProfile, AccessToken string; AccessToken has json omit tag. No RT/ID token. CredentialSnapshot refreshes an expired grant through this core authority. RT is committed before snapshot publication; unknown refresh fails closed.
Distinct account Extra marker core_oauth, contract sub6-core-oauth/v1. CoreOAuthAccountBinding fields Contract/ProfileRef/Provider/Subject/WorkspaceID/IdentityEpoch/TokenRevision/ExpiresAt, wire snake_case. CoreOAuthAccountBindingForSnapshot(snapshot) and CoreOAuthAccountBindingFromAccount(account) return (*CoreOAuthAccountBinding,error). Reject external managed marker, RT-bearing runtime account, provider/platform mismatch or malformed identity/version/expiry. D owns runtime account CAS/install and must consume current profile identity/revision and AT as one snapshot. No external-owner fence bypass.

## Restart and refresh ownership
Private encrypted profile includes grant, actor-bound attempt metadata and durable refresh operation marker. PKCE objects remain process-local; unfinished attempts after process restart explicitly expire and require a new authorization. Profile/RT remains durable. Cross-instance CAS claim precedes token request; sent/unknown refresh is never blindly retried with old RT. No dependency on toSub2 process/Keychain.
