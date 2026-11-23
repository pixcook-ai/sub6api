package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const ManagedOAuthContract = "tosub2-managed-oauth/v1"
const ManagedOAuthExtraKey = "managed_oauth"
const managedOAuthMaxEvents = 128
const managedOAuthMaxRevision = int64(9007199254740991)

var ErrManagedOAuthRefresh = infraerrors.New(http.StatusConflict, "MANAGED_OAUTH_EXTERNAL_REFRESH_REQUIRED", "OAuth credentials are managed externally; use the controller to restore authorization")
var ErrManagedOAuthConflict = infraerrors.New(http.StatusConflict, "MANAGED_OAUTH_REVISION_CONFLICT", "managed OAuth identity or credential revision changed")
var ErrManagedOAuthInvalid = infraerrors.New(http.StatusBadRequest, "MANAGED_OAUTH_INVALID", "invalid managed OAuth contract or token identity")
var ErrManagedOAuthUnavailable = infraerrors.New(http.StatusServiceUnavailable, "MANAGED_OAUTH_UNAVAILABLE", "managed OAuth credentials are unavailable; controller recovery is required")

type ManagedOAuthIdentity struct {
	Contract      string `json:"contract"`
	BackendID     string `json:"backend_id"`
	BindingRef    string `json:"binding_ref"`
	ProfileRef    string `json:"profile_ref"`
	Provider      string `json:"provider"`
	Subject       string `json:"subject"`
	WorkspaceID   string `json:"workspace_id"`
	IdentityEpoch string `json:"identity_epoch"`
}

type ManagedOAuthInstallRequest struct {
	ManagedOAuthIdentity
	ExpectedTokenRevision int64  `json:"expected_token_revision"`
	TokenRevision         int64  `json:"token_revision"`
	OperationID           string `json:"operation_id"`
	AccessToken           string `json:"access_token"`
	ExpiresAt             string `json:"expires_at"`
}

type ManagedOAuthAuthEvent struct {
	ManagedOAuthIdentity
	Cursor              int64  `json:"cursor"`
	EventID             int64  `json:"event_id"`
	FailedTokenRevision int64  `json:"failed_token_revision"`
	Origin              string `json:"origin"`
	Code                string `json:"code"`
	ObservedAt          string `json:"observed_at"`
}

type ManagedOAuthState struct {
	ManagedOAuthIdentity
	TokenRevision     int64                   `json:"token_revision"`
	ExpiresAt         string                  `json:"expires_at,omitempty"`
	LastOperationID   string                  `json:"last_operation_id,omitempty"`
	LastOperationHash string                  `json:"last_operation_hash,omitempty"`
	AuthBlocked       bool                    `json:"auth_blocked"`
	EventCursor       int64                   `json:"event_cursor"`
	Events            []ManagedOAuthAuthEvent `json:"events"`
}

type ManagedOAuthPublicState struct {
	ManagedOAuthIdentity
	AccountID       int64  `json:"account_id"`
	TokenRevision   int64  `json:"token_revision"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	LastOperationID string `json:"last_operation_id,omitempty"`
	AuthBlocked     bool   `json:"auth_blocked"`
	EventCursor     int64  `json:"event_cursor"`
	Ready           bool   `json:"ready"`
	Initialized     bool   `json:"initialized"`
}

type ManagedOAuthEventPage struct {
	Events     []ManagedOAuthAuthEvent `json:"events"`
	NextCursor int64                   `json:"next_cursor"`
	CursorGap  bool                    `json:"cursor_gap"`
}

// Kept separate from AccountRepository so unrelated providers and test doubles
// cannot silently implement controller writes through a read-modify-write path.
type ManagedOAuthRepository interface {
	InstallManagedOAuthCredentialsIfMatch(context.Context, int64, ManagedOAuthState, ManagedOAuthState, string, string) (bool, error)
	RecordManagedOAuthEventIfMatch(context.Context, int64, ManagedOAuthState, ManagedOAuthState, string) (bool, error)
}

type ManagedOAuthAdminService interface {
	GetManagedOAuthState(context.Context, int64) (*ManagedOAuthPublicState, error)
	InstallManagedOAuthCredentials(context.Context, int64, *ManagedOAuthInstallRequest) (*ManagedOAuthPublicState, error)
	ListManagedOAuthAuthEvents(context.Context, int64, int64) (*ManagedOAuthEventPage, error)
}

// Presence is sufficient to fence refresh even when persisted metadata is invalid.
func ManagedOAuthDeclared(account *Account) bool {
	if account == nil {
		return false
	}
	_, ok := account.Extra[ManagedOAuthExtraKey]
	return ok
}

func validManagedOAuthOpaque(value string) bool {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}

func (id ManagedOAuthIdentity) Validate() error {
	if id.Contract != ManagedOAuthContract || id.Provider != "codex" {
		return ErrManagedOAuthInvalid
	}
	for _, value := range []string{id.BackendID, id.BindingRef, id.ProfileRef, id.Subject, id.WorkspaceID, id.IdentityEpoch} {
		if !validManagedOAuthOpaque(value) {
			return ErrManagedOAuthInvalid
		}
	}
	return nil
}

func managedOAuthStateFromAccount(account *Account) (*ManagedOAuthState, error) {
	if !ManagedOAuthDeclared(account) || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || strings.TrimSpace(account.GetCredential("refresh_token")) != "" || account.IsCredentialShadow() {
		return nil, ErrManagedOAuthInvalid
	}
	raw, err := json.Marshal(account.Extra[ManagedOAuthExtraKey])
	if err != nil || len(raw) > 256<<10 {
		return nil, ErrManagedOAuthInvalid
	}
	var state ManagedOAuthState
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&state) != nil || state.Validate() != nil || state.TokenRevision < 0 || state.TokenRevision > managedOAuthMaxRevision || state.EventCursor < 0 || state.EventCursor > managedOAuthMaxRevision || len(state.Events) > managedOAuthMaxEvents {
		return nil, ErrManagedOAuthInvalid
	}
	if state.TokenRevision > 0 {
		expiry, err := time.Parse(time.RFC3339Nano, state.ExpiresAt)
		op, opErr := uuid.Parse(state.LastOperationID)
		hash, hashErr := hex.DecodeString(state.LastOperationHash)
		if err != nil || expiry.IsZero() || opErr != nil || op == uuid.Nil || op.String() != state.LastOperationID || hashErr != nil || len(hash) != 32 {
			return nil, ErrManagedOAuthInvalid
		}
	} else if state.ExpiresAt != "" || state.LastOperationID != "" || state.LastOperationHash != "" || state.AuthBlocked || state.EventCursor != 0 || len(state.Events) != 0 {
		return nil, ErrManagedOAuthInvalid
	}
	var prior int64
	for _, event := range state.Events {
		if event.ManagedOAuthIdentity != state.ManagedOAuthIdentity || event.Cursor <= prior || event.Cursor > state.EventCursor || event.EventID != event.Cursor || event.FailedTokenRevision <= 0 || event.FailedTokenRevision > state.TokenRevision || event.Origin != "upstream_oauth" || event.Code != "upstream_oauth_unauthorized" {
			return nil, ErrManagedOAuthInvalid
		}
		_, observedErr := time.Parse(time.RFC3339Nano, event.ObservedAt)
		if observedErr != nil {
			return nil, ErrManagedOAuthInvalid
		}
		prior = event.Cursor
	}
	if (state.EventCursor == 0) != (len(state.Events) == 0) || (len(state.Events) > 0 && prior != state.EventCursor) {
		return nil, ErrManagedOAuthInvalid
	}
	return &state, nil
}

func managedOAuthReady(state *ManagedOAuthState, now time.Time) bool {
	if state == nil || state.TokenRevision == 0 || state.AuthBlocked {
		return false
	}
	expiry, err := time.Parse(time.RFC3339Nano, state.ExpiresAt)
	return err == nil && now.Before(expiry)
}

func ManagedOAuthSchedulingReady(account *Account) bool {
	state, err := managedOAuthStateFromAccount(account)
	return err == nil && managedOAuthReady(state, time.Now())
}

func managedOAuthPublic(account *Account, state *ManagedOAuthState) *ManagedOAuthPublicState {
	return &ManagedOAuthPublicState{ManagedOAuthIdentity: state.ManagedOAuthIdentity, AccountID: account.ID, TokenRevision: state.TokenRevision, ExpiresAt: state.ExpiresAt, LastOperationID: state.LastOperationID, AuthBlocked: state.AuthBlocked, EventCursor: state.EventCursor, Ready: managedOAuthReady(state, time.Now()), Initialized: state.TokenRevision > 0}
}

func prepareManagedOAuthCreate(input *CreateAccountInput, extra map[string]any) (map[string]any, error) {
	_, hasMarker := extra[ManagedOAuthExtraKey]
	if input.ManagedOAuth == nil {
		if hasMarker {
			return nil, ErrManagedOAuthInvalid
		}
		return extra, nil
	}
	if hasMarker || input.ManagedOAuth.Validate() != nil || input.Platform != PlatformOpenAI || input.Type != AccountTypeOAuth {
		return nil, ErrManagedOAuthInvalid
	}
	for key := range input.Credentials {
		if key != "model_mapping" && key != "compact_model_mapping" {
			return nil, ErrManagedOAuthInvalid
		}
	}
	if input.Schedulable != nil && *input.Schedulable {
		return nil, ErrManagedOAuthInvalid
	}
	paused := false
	input.Schedulable = &paused
	if extra == nil {
		extra = map[string]any{}
	}
	extra[ManagedOAuthExtraKey] = ManagedOAuthState{ManagedOAuthIdentity: *input.ManagedOAuth, Events: []ManagedOAuthAuthEvent{}}
	// Managed v1 intentionally admits only the HTTP transport.
	extra["openai_oauth_responses_websockets_v2_enabled"] = false
	extra["openai_oauth_responses_websockets_v2_mode"] = "off"
	return extra, nil
}

func (s *adminServiceImpl) GetManagedOAuthState(ctx context.Context, id int64) (*ManagedOAuthPublicState, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	return managedOAuthPublic(account, state), nil
}

func (s *adminServiceImpl) InstallManagedOAuthCredentials(ctx context.Context, id int64, input *ManagedOAuthInstallRequest) (*ManagedOAuthPublicState, error) {
	if input == nil || input.ManagedOAuthIdentity.Validate() != nil || input.ExpectedTokenRevision < 0 || input.TokenRevision <= input.ExpectedTokenRevision || input.TokenRevision > managedOAuthMaxRevision || len(input.AccessToken) > 64<<10 || strings.TrimSpace(input.AccessToken) != input.AccessToken || strings.ContainsAny(input.AccessToken, "\r\n") {
		return nil, ErrManagedOAuthInvalid
	}
	op, err := uuid.Parse(input.OperationID)
	if err != nil || op == uuid.Nil || op.String() != input.OperationID {
		return nil, ErrManagedOAuthInvalid
	}
	expiry, err := time.Parse(time.RFC3339Nano, input.ExpiresAt)
	if err != nil || !time.Now().Before(expiry) || validateManagedOAuthToken(input.AccessToken, input.ManagedOAuthIdentity, expiry) != nil {
		return nil, ErrManagedOAuthInvalid
	}
	inputCopy := *input
	inputCopy.ExpiresAt = expiry.UTC().Format(time.RFC3339Nano)
	canonical, _ := json.Marshal(inputCopy)
	sum := sha256.Sum256(canonical)
	payloadHash := hex.EncodeToString(sum[:])
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	if state.ManagedOAuthIdentity != input.ManagedOAuthIdentity {
		return nil, ErrManagedOAuthConflict
	}
	if state.LastOperationID == input.OperationID {
		if state.LastOperationHash != payloadHash {
			return nil, ErrManagedOAuthConflict
		}
		return managedOAuthPublic(account, state), nil
	}
	if state.TokenRevision != input.ExpectedTokenRevision {
		return nil, ErrManagedOAuthConflict
	}
	repo, ok := s.accountRepo.(ManagedOAuthRepository)
	if !ok {
		return nil, ErrManagedOAuthUnavailable
	}
	next := *state
	next.TokenRevision = input.TokenRevision
	next.ExpiresAt = inputCopy.ExpiresAt
	next.LastOperationID = input.OperationID
	next.LastOperationHash = payloadHash
	next.AuthBlocked = false
	updated, err := repo.InstallManagedOAuthCredentialsIfMatch(ctx, id, *state, next, input.AccessToken, expiry.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	if !updated {
		return nil, ErrManagedOAuthConflict
	}
	return managedOAuthPublic(account, &next), nil
}

// These are claim-consistency checks on a controller-supplied token, not JWT
// signature verification or proof that the upstream accepts the authorization.
func validateManagedOAuthToken(token string, identity ManagedOAuthIdentity, expiry time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ErrManagedOAuthInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrManagedOAuthInvalid
	}
	var claims struct {
		Sub      string `json:"sub"`
		ClientID string `json:"client_id"`
		AZP      string `json:"azp"`
		Exp      int64  `json:"exp"`
		Auth     struct {
			Workspace string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Sub != identity.Subject || claims.Auth.Workspace != identity.WorkspaceID || claims.Exp <= 0 || expiry.After(time.Unix(claims.Exp, 0)) {
		return ErrManagedOAuthInvalid
	}
	if claims.ClientID == "" && claims.AZP == "" {
		return ErrManagedOAuthInvalid
	}
	if (claims.ClientID != "" && claims.ClientID != openai.ClientID) || (claims.AZP != "" && claims.AZP != openai.ClientID) {
		return ErrManagedOAuthInvalid
	}
	return nil
}

func (s *adminServiceImpl) ListManagedOAuthAuthEvents(ctx context.Context, id, after int64) (*ManagedOAuthEventPage, error) {
	if after < 0 {
		return nil, ErrManagedOAuthInvalid
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	state, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return nil, err
	}
	if after > state.EventCursor {
		return nil, infraerrors.New(http.StatusConflict, "MANAGED_OAUTH_CURSOR_EXPIRED", "read current managed credential state before restarting event consumption")
	}
	events := make([]ManagedOAuthAuthEvent, 0)
	for _, event := range state.Events {
		if event.Cursor > after {
			events = append(events, event)
		}
	}
	return &ManagedOAuthEventPage{Events: events, NextCursor: state.EventCursor, CursorGap: len(state.Events) > 0 && after < state.Events[0].Cursor-1}, nil
}

func recordManagedOAuthUnauthorized(ctx context.Context, repo AccountRepository, account *Account, rejectedToken string) error {
	if !ManagedOAuthDeclared(account) || rejectedToken == "" {
		return nil
	}
	if repo == nil {
		return ErrManagedOAuthUnavailable
	}
	attempt, err := managedOAuthStateFromAccount(account)
	if err != nil {
		return err
	}
	current, err := repo.GetByID(ctx, account.ID)
	if err != nil {
		return err
	}
	state, err := managedOAuthStateFromAccount(current)
	if err != nil {
		return err
	}
	if state.TokenRevision == 0 || state.TokenRevision != attempt.TokenRevision || state.ManagedOAuthIdentity != attempt.ManagedOAuthIdentity || current.GetCredential("access_token") != rejectedToken || state.AuthBlocked {
		return nil
	}
	if state.EventCursor == managedOAuthMaxRevision {
		return ErrManagedOAuthUnavailable
	}
	writer, ok := repo.(ManagedOAuthRepository)
	if !ok {
		return ErrManagedOAuthUnavailable
	}
	next := *state
	next.AuthBlocked = true
	next.EventCursor++
	event := ManagedOAuthAuthEvent{ManagedOAuthIdentity: state.ManagedOAuthIdentity, Cursor: next.EventCursor, EventID: next.EventCursor, FailedTokenRevision: state.TokenRevision, Origin: "upstream_oauth", Code: "upstream_oauth_unauthorized", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	next.Events = append(append([]ManagedOAuthAuthEvent{}, state.Events...), event)
	if len(next.Events) > managedOAuthMaxEvents {
		next.Events = next.Events[len(next.Events)-managedOAuthMaxEvents:]
	}
	_, err = writer.RecordManagedOAuthEventIfMatch(ctx, current.ID, *state, next, rejectedToken)
	return err
}

type managedOAuthAttemptContextKey struct{}

const managedOAuthAttemptGinKey = "managed_oauth_attempt"

// Read AT and revision together. The returned snapshot is private and immutable
// for the whole request, including the interval before entering the transport.
func readManagedOAuthCredentials(ctx context.Context, repo AccountRepository, account *Account) (*Account, error) {
	if repo == nil {
		return nil, ErrManagedOAuthUnavailable
	}
	current, err := repo.GetByID(ctx, account.ID)
	if err != nil || current == nil || current.ID != account.ID {
		return nil, ErrManagedOAuthUnavailable
	}
	state, err := managedOAuthStateFromAccount(current)
	token := current.GetCredential("access_token")
	if err != nil || !current.IsActive() || !managedOAuthReady(state, time.Now()) || token == "" {
		return nil, ErrManagedOAuthUnavailable
	}
	snapshot := *current
	snapshot.Credentials = map[string]any{"access_token": token}
	snapshot.Extra = map[string]any{ManagedOAuthExtraKey: *state}
	return &snapshot, nil
}

func managedOAuthCredentialsForRequest(ctx context.Context, repo AccountRepository, account *Account) (context.Context, string, error) {
	attempt, err := readManagedOAuthCredentials(ctx, repo, account)
	if err != nil {
		return ctx, "", err
	}
	return context.WithValue(ctx, managedOAuthAttemptContextKey{}, attempt), attempt.GetCredential("access_token"), nil
}

func openAIProbeCredentialsForRequest(ctx context.Context, repo AccountRepository, account *Account) (context.Context, string, error) {
	if ManagedOAuthDeclared(account) {
		return managedOAuthCredentialsForRequest(ctx, repo, account)
	}
	return ctx, account.GetOpenAIAccessToken(), nil
}

// Transport may verify the token's original snapshot, never infer a newer
// revision from bearer equality by rereading the database.
func managedOAuthAttemptForRequest(request *http.Request, account *Account) (*Account, error) {
	if !ManagedOAuthDeclared(account) {
		return nil, nil
	}
	attempt, _ := request.Context().Value(managedOAuthAttemptContextKey{}).(*Account)
	if attempt == nil || attempt.ID != account.ID || request.Header.Get("Authorization") != "Bearer "+attempt.GetCredential("access_token") {
		return nil, ErrManagedOAuthUnavailable
	}
	return attempt, nil
}

func attachManagedOAuthAttempt(resp *http.Response, request *http.Request, attempt *Account) {
	if resp != nil && request != nil && attempt != nil {
		resp.Request = request.Clone(context.WithValue(request.Context(), managedOAuthAttemptContextKey{}, attempt))
	}
}

func bindManagedOAuthResponseAttempt(c *gin.Context, resp *http.Response) {
	if c == nil {
		return
	}
	var attempt *Account
	if resp != nil && resp.Request != nil {
		attempt, _ = resp.Request.Context().Value(managedOAuthAttemptContextKey{}).(*Account)
	}
	c.Set(managedOAuthAttemptGinKey, attempt)
}

func (s *OpenAIGatewayService) recordManagedOAuthStreamUnauthorized(c *gin.Context, account *Account) {
	if s == nil || c == nil || c.Request == nil {
		return
	}
	value, _ := c.Get(managedOAuthAttemptGinKey)
	attempt, _ := value.(*Account)
	if attempt != nil && account != nil && attempt.ID == account.ID {
		_ = recordManagedOAuthUnauthorized(c.Request.Context(), s.accountRepo, attempt, attempt.GetCredential("access_token"))
	}
}

func managedOAuthAccessToken(ctx context.Context, repo AccountRepository, account *Account) (string, error) {
	snapshot, err := readManagedOAuthCredentials(ctx, repo, account)
	if err != nil {
		return "", err
	}
	return snapshot.GetCredential("access_token"), nil
}

func managedOAuthAuthFieldsUnchanged(current *Account, input *UpdateAccountInput) error {
	if input.Extra != nil {
		if _, ok := input.Extra[ManagedOAuthExtraKey]; ok {
			return ErrManagedOAuthRefresh
		}
	}
	if !ManagedOAuthDeclared(current) {
		return nil
	}
	if input.Credentials != nil {
		return ErrManagedOAuthRefresh
	}
	if input.Type != "" && input.Type != AccountTypeOAuth {
		return ErrManagedOAuthRefresh
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "chatgpt_account_id", "client_id", "auth_mode", "openai_auth_mode"} {
		if _, ok := input.Credentials[key]; ok {
			return ErrManagedOAuthRefresh
		}
	}
	return nil
}

var _ ManagedOAuthAdminService = (*adminServiceImpl)(nil)
