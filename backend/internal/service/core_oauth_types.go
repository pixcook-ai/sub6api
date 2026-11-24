package service

import (
	"context"
	"net/http"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
)

const (
	CoreOAuthProviderCodex        = "codex"
	CoreOAuthProviderExcel        = "bps_excel"
	CoreOAuthAccountExtraKey      = "core_oauth"
	CoreOAuthRuntimeShellExtraKey = "core_oauth_runtime_shell"
	CoreOAuthContract             = "sub6-core-oauth/v1"
)

var (
	ErrCoreOAuthUnavailable           = infraerrors.New(http.StatusServiceUnavailable, "CORE_OAUTH_UNAVAILABLE", "Core OAuth is unavailable")
	ErrCoreOAuthInvalid               = infraerrors.New(http.StatusBadRequest, "CORE_OAUTH_INVALID_INPUT", "Invalid core OAuth input")
	ErrCoreOAuthProfileNotFound       = infraerrors.New(http.StatusNotFound, "CORE_OAUTH_PROFILE_NOT_FOUND", "Core OAuth profile not found")
	ErrCoreOAuthConflict              = infraerrors.New(http.StatusConflict, "CORE_OAUTH_CONFLICT", "Core OAuth identity or version changed")
	ErrCoreOAuthBusy                  = infraerrors.New(http.StatusConflict, "CORE_OAUTH_BUSY", "Core OAuth operation is in progress")
	ErrCoreOAuthAttemptExpired        = infraerrors.New(http.StatusGone, "CORE_OAUTH_ATTEMPT_EXPIRED", "Authorization attempt expired; start a new attempt")
	ErrCoreOAuthAttemptForbidden      = infraerrors.New(http.StatusForbidden, "CORE_OAUTH_ATTEMPT_FORBIDDEN", "Authorization attempt belongs to another administrator")
	ErrCoreOAuthAuthorizationRequired = infraerrors.New(http.StatusConflict, "CORE_OAUTH_AUTHORIZATION_REQUIRED", "Complete authorization for the original provider")
	ErrCoreOAuthRefreshUncertain      = infraerrors.New(http.StatusConflict, "CORE_OAUTH_REFRESH_UNCERTAIN", "Refresh outcome is unknown; authorize the original provider again")
	ErrCoreOAuthProviderRejected      = infraerrors.New(http.StatusBadRequest, "CORE_OAUTH_PROVIDER_REJECTED", "The provider rejected authorization")
	ErrCoreOAuthRegionUnsupported     = infraerrors.New(http.StatusBadRequest, "CORE_OAUTH_REGION_UNSUPPORTED", "The provider rejected this network region; select a supported proxy and start a new authorization")
	ErrCoreOAuthStorage               = infraerrors.New(http.StatusServiceUnavailable, "CORE_OAUTH_STORAGE_FAILED", "Core OAuth secure storage is unavailable")
	ErrCoreOAuthHandoffPending        = infraerrors.New(http.StatusConflict, "CORE_OAUTH_HANDOFF_PENDING", "Core OAuth ownership handoff is pending")
	ErrCoreOAuthHandoffExpired        = infraerrors.New(http.StatusConflict, "CORE_OAUTH_HANDOFF_EXPIRED", "Core OAuth ownership handoff has expired and remains locked")
	// Aliases keep handler terminology aligned with fixed public error codes.
	ErrCoreOAuthInvalidInput  = ErrCoreOAuthInvalid
	ErrCoreOAuthStorageFailed = ErrCoreOAuthStorage
)

// CoreOAuthRepository stores sealed records only. CAS is the cross-process writer fence.
type CoreOAuthRepository interface {
	ReadProfile(context.Context, string) (*CoreOAuthSealedProfile, error)
	ListProfiles(context.Context) ([]CoreOAuthSealedProfile, error)
	CreateProfile(context.Context, CoreOAuthSealedProfile) error
	CompareAndSwapProfile(context.Context, string, int64, CoreOAuthSealedProfile) (bool, error)
}

type CoreOAuthSealedProfile struct {
	ProfileRef string
	Version    int64
	Ciphertext string
}

type CoreOAuthCreateInput struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	ProxyID  *int64 `json:"proxy_id,omitempty"`
}

type CoreOAuthCompleteInput struct {
	AttemptID   string `json:"attempt_id"`
	CallbackURL string `json:"callback_url"`
	ActorID     int64  `json:"-"`
}

type CoreOAuthUnauthorizedInput struct {
	ProfileRef     string
	IdentityEpoch  string
	FailedRevision int64
	StatusCode     int
	Origin         string
}

// CoreOAuthHandoffInput is an internal, actor-bound receiver request. The
// opaque payload is staged without authenticating its source or importing it. It is retained only
// inside the encrypted profile record and never appears in a public response.
type CoreOAuthHandoffInput struct {
	OperationID      string    `json:"operation_id"`
	Provider         string    `json:"provider"`
	ProfileRef       string    `json:"profile_ref"` // Destination Sub6 profile.
	SourceProfileRef string    `json:"source_profile_ref"`
	Subject          string    `json:"subject"`
	WorkspaceID      string    `json:"workspace_id"`
	IdentityEpoch    string    `json:"identity_epoch"`
	TokenRevision    int64     `json:"token_revision"`
	ExpiresAt        time.Time `json:"expires_at"`
	ProtectedPayload string    `json:"protected_payload"`
	ActorID          int64     `json:"-"`
}

type CoreOAuthHandoffReceipt struct {
	OperationID       string    `json:"operation_id"`
	Provider          string    `json:"provider"`
	ProfileRef        string    `json:"profile_ref"`
	SourceProfileRef  string    `json:"source_profile_ref"`
	Subject           string    `json:"subject"`
	WorkspaceID       string    `json:"workspace_id"`
	IdentityEpoch     string    `json:"identity_epoch"`
	TokenRevision     int64     `json:"token_revision"`
	Status            string    `json:"status"`
	ExpiresAt         time.Time `json:"expires_at"`
	TransferSupported bool      `json:"transfer_supported"`
}

type CoreOAuthAttemptView struct {
	AttemptID string    `json:"attempt_id"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CoreOAuthProfile is safe for admin JSON responses. It never embeds a grant.
type CoreOAuthProfile struct {
	ProfileRef         string                `json:"profile_ref"`
	Provider           string                `json:"provider"`
	Name               string                `json:"name"`
	Status             string                `json:"status"`
	Authorized         bool                  `json:"authorized"`
	NeedsAuthorization bool                  `json:"needs_authorization"`
	Subject            string                `json:"subject"`
	WorkspaceID        string                `json:"workspace_id"`
	Email              string                `json:"email"`
	ClientID           string                `json:"client_id"`
	IdentityEpoch      string                `json:"identity_epoch"`
	TokenRevision      int64                 `json:"token_revision"`
	ExpiresAt          *time.Time            `json:"expires_at"`
	Attempt            *CoreOAuthAttemptView `json:"attempt"`
	LastErrorCode      string                `json:"last_error_code"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
}

type CoreOAuthAuthorization struct {
	Profile          CoreOAuthProfile `json:"profile"`
	AttemptID        string           `json:"attempt_id"`
	AuthorizationURL string           `json:"authorization_url"`
	RedirectURI      string           `json:"redirect_uri"`
	ExpiresAt        time.Time        `json:"expires_at"`
}

type CoreOAuthIdentity struct{ Subject, WorkspaceID, Email string }
type CoreOAuthTransport struct {
	ProxyID  *int64
	ProxyURL string
}

// Protocol attempts and grants are internal seams, never HTTP DTOs.
type CoreOAuthProtocolAttempt struct {
	AuthorizationURL string    `json:"-"`
	RedirectURI      string    `json:"-"`
	ExpiresAt        time.Time `json:"-"`
	State            any       `json:"-"`
}
type CoreOAuthGrant struct {
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	IDToken      string    `json:"-"`
	ClientID     string    `json:"-"`
	Subject      string    `json:"-"`
	WorkspaceID  string    `json:"-"`
	Email        string    `json:"-"`
	ExpiresAt    time.Time `json:"-"`
}
type CoreOAuthProtocol interface {
	Begin(context.Context, CoreOAuthTransport) (*CoreOAuthProtocolAttempt, error)
	ExchangeCallback(context.Context, *CoreOAuthProtocolAttempt, string, CoreOAuthIdentity, CoreOAuthTransport) (*CoreOAuthGrant, error)
	Refresh(context.Context, CoreOAuthGrant, CoreOAuthTransport) (*CoreOAuthGrant, error)
}
type CoreOAuthProtocolError struct {
	Permanent      bool
	OutcomeUnknown bool
	// SafeCode is a fixed local diagnostic suitable for profile state.
	// It never contains provider text or response bodies.
	SafeCode string
}

func (*CoreOAuthProtocolError) Error() string { return "OAuth provider operation failed" }

type CoreOAuthCredentialSnapshot struct {
	Profile     CoreOAuthProfile `json:"profile"`
	AccessToken string           `json:"-"`
}

type CoreOAuthAccountBinding struct {
	Contract      string `json:"contract"`
	ProfileRef    string `json:"profile_ref"`
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	WorkspaceID   string `json:"workspace_id"`
	IdentityEpoch string `json:"identity_epoch"`
	TokenRevision int64  `json:"token_revision"`
	ExpiresAt     string `json:"expires_at"`
}
