package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

type CoreOAuthService struct {
	repo      CoreOAuthRepository
	encryptor SecretEncryptor
	protocols map[string]CoreOAuthProtocol
	proxyRepo ProxyRepository
	mu        sync.Mutex
	attempts  map[string]*CoreOAuthProtocolAttempt
}

type coreOAuthStoredGrant struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ClientID     string
	Subject      string
	WorkspaceID  string
	Email        string
	ExpiresAt    time.Time
}
type coreOAuthStoredAttempt struct {
	ID        string
	ActorID   int64
	Status    string
	ExpiresAt time.Time
}
type coreOAuthOperation struct {
	ID        string
	Kind      string
	ExpiresAt time.Time
}
type coreOAuthHandoffRecord struct {
	OperationID        string
	Provider           string
	ProfileRef         string
	SourceProfileRef   string
	Subject            string
	WorkspaceID        string
	IdentityEpoch      string
	TokenRevision      int64
	PayloadDigest      string
	ProtectedPayload   string
	ActorID            int64
	Status             string
	ExpiresAt          time.Time
	TransferContract   string
	TransferPublicKey  string
	TransferPrivateKey string
}
type coreOAuthRecord struct {
	Contract           string
	ProfileRef         string
	Version            int64
	Provider           string
	Name               string
	ProxyID            *int64
	Grant              *coreOAuthStoredGrant
	IdentityEpoch      string
	TokenRevision      int64
	Attempt            *coreOAuthStoredAttempt
	Operation          *coreOAuthOperation
	Handoff            *coreOAuthHandoffRecord
	NeedsAuthorization bool
	LastErrorCode      string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func coreOAuthHandoffReceipt(record *coreOAuthRecord) *CoreOAuthHandoffReceipt {
	if record == nil || record.Handoff == nil {
		return nil
	}
	h := record.Handoff
	return &CoreOAuthHandoffReceipt{OperationID: h.OperationID, Provider: h.Provider, ProfileRef: h.ProfileRef,
		SourceProfileRef: h.SourceProfileRef,
		Subject:          h.Subject, WorkspaceID: h.WorkspaceID, IdentityEpoch: h.IdentityEpoch,
		TokenRevision: h.TokenRevision, Status: h.Status, ExpiresAt: h.ExpiresAt, TransferSupported: h.Status == "accepted"}
}

func validCoreOAuthHandoffPayload(input CoreOAuthHandoffInput) bool {
	if input.ActorID <= 0 || input.OperationID == "" || input.ProfileRef == "" || input.SourceProfileRef == "" || input.Provider == "" ||
		input.Subject == "" || input.WorkspaceID == "" || input.IdentityEpoch == "" || input.TokenRevision < 1 ||
		input.ExpiresAt.IsZero() {
		return false
	}
	return coreOAuthRef(input.OperationID) && coreOAuthRef(input.ProfileRef) && coreOAuthHandoffSourceRef(input.SourceProfileRef) &&
		coreOAuthRef(input.IdentityEpoch) && coreOAuthProvider(input.Provider) &&
		coreOAuthIdentityText(input.Subject) && coreOAuthIdentityText(input.WorkspaceID) && coreOAuthProtectedPayload(input.ProtectedPayload)
}

func coreOAuthHandoffSourceRef(value string) bool {
	return strings.HasPrefix(value, "oauth_") && coreOAuthRef(strings.TrimPrefix(value, "oauth_"))
}

func coreOAuthProtectedPayload(value string) bool {
	if len(value) < 32 || len(value) > 256<<10 {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '=') {
			return false
		}
	}
	return true
}

func coreOAuthHandoffDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func NewCoreOAuthService(repo CoreOAuthRepository, encryptor SecretEncryptor, protocols map[string]CoreOAuthProtocol, proxyRepo ProxyRepository) (*CoreOAuthService, error) {
	if repo == nil || encryptor == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	copied := make(map[string]CoreOAuthProtocol, len(protocols))
	for provider, protocol := range protocols {
		if !coreOAuthProvider(provider) || protocol == nil {
			return nil, ErrCoreOAuthUnavailable
		}
		copied[provider] = protocol
	}
	return &CoreOAuthService{repo: repo, encryptor: encryptor, protocols: copied, proxyRepo: proxyRepo, attempts: make(map[string]*CoreOAuthProtocolAttempt)}, nil
}

func (s *CoreOAuthService) CreateProfile(ctx context.Context, input CoreOAuthCreateInput) (*CoreOAuthProfile, error) {
	if s == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	name := strings.TrimSpace(input.Name)
	if !coreOAuthProvider(input.Provider) || name == "" || len(name) > 120 || strings.ContainsFunc(name, unicode.IsControl) || (input.ProxyID != nil && *input.ProxyID <= 0) {
		return nil, ErrCoreOAuthInvalid
	}
	now := time.Now().UTC()
	record := &coreOAuthRecord{Contract: CoreOAuthContract, ProfileRef: uuid.NewString(), Version: 1, Provider: input.Provider, Name: name, ProxyID: input.ProxyID, NeedsAuthorization: true, CreatedAt: now, UpdatedAt: now}
	sealed, err := s.seal(record)
	if err != nil {
		return nil, err
	}
	if err = s.repo.CreateProfile(ctx, *sealed); err != nil {
		if errors.Is(err, ErrCoreOAuthConflict) {
			return nil, err
		}
		return nil, ErrCoreOAuthStorage
	}
	view := s.view(record)
	return &view, nil
}

func coreOAuthProvider(provider string) bool {
	return provider == CoreOAuthProviderCodex || provider == CoreOAuthProviderExcel
}
func coreOAuthRef(ref string) bool {
	id, err := uuid.Parse(ref)
	return err == nil && id != uuid.Nil && id.String() == ref
}

func (s *CoreOAuthService) seal(record *coreOAuthRecord) (*CoreOAuthSealedProfile, error) {
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrCoreOAuthStorage
	}
	ciphertext, err := s.encryptor.Encrypt(string(raw))
	if err != nil || ciphertext == "" {
		return nil, ErrCoreOAuthStorage
	}
	return &CoreOAuthSealedProfile{ProfileRef: record.ProfileRef, Version: record.Version, Ciphertext: ciphertext}, nil
}
func (s *CoreOAuthService) unseal(row CoreOAuthSealedProfile) (*coreOAuthRecord, error) {
	if !coreOAuthRef(row.ProfileRef) || row.Version < 1 || len(row.Ciphertext) > 2<<20 {
		return nil, ErrCoreOAuthStorage
	}
	plaintext, err := s.encryptor.Decrypt(row.Ciphertext)
	if err != nil || len(plaintext) > 1<<20 {
		return nil, ErrCoreOAuthStorage
	}
	var record coreOAuthRecord
	decoder := json.NewDecoder(strings.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Contract != CoreOAuthContract || record.ProfileRef != row.ProfileRef || record.Version != row.Version || !coreOAuthProvider(record.Provider) || record.TokenRevision < 0 || !coreOAuthHandoffRecordValid(&record) {
		return nil, ErrCoreOAuthStorage
	}
	return &record, nil
}

func coreOAuthHandoffRecordValid(record *coreOAuthRecord) bool {
	if record.Handoff == nil {
		return true
	}
	h := record.Handoff
	if h.TransferContract != "" {
		return coreOAuthTransferRecordValid(record)
	}
	if h.ProfileRef != record.ProfileRef || h.Provider != record.Provider || !coreOAuthRef(h.OperationID) ||
		!coreOAuthHandoffSourceRef(h.SourceProfileRef) || !coreOAuthIdentityText(h.Subject) || !coreOAuthIdentityText(h.WorkspaceID) ||
		!coreOAuthRef(h.IdentityEpoch) || h.TokenRevision < 1 || h.ActorID <= 0 || h.ExpiresAt.IsZero() || len(h.PayloadDigest) != 64 {
		return false
	}
	if h.Status == "abandoned" {
		return h.ProtectedPayload == ""
	}
	return h.Status == "pending" && record.Grant == nil && record.NeedsAuthorization && record.Operation == nil && record.Attempt == nil &&
		coreOAuthProtectedPayload(h.ProtectedPayload) && h.PayloadDigest == coreOAuthHandoffDigest(h.ProtectedPayload)
}
func (s *CoreOAuthService) load(ctx context.Context, ref string) (*coreOAuthRecord, error) {
	if s == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	if !coreOAuthRef(ref) {
		return nil, ErrCoreOAuthInvalid
	}
	row, err := s.repo.ReadProfile(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrCoreOAuthProfileNotFound) {
			return nil, err
		}
		return nil, ErrCoreOAuthStorage
	}
	if row == nil {
		return nil, ErrCoreOAuthStorage
	}
	return s.unseal(*row)
}
func (s *CoreOAuthService) save(ctx context.Context, record *coreOAuthRecord) error {
	old := record.Version
	record.Version++
	record.UpdatedAt = time.Now().UTC()
	row, err := s.seal(record)
	if err != nil {
		return err
	}
	ok, err := s.repo.CompareAndSwapProfile(ctx, record.ProfileRef, old, *row)
	if err != nil {
		return ErrCoreOAuthStorage
	}
	if !ok {
		return ErrCoreOAuthConflict
	}
	return nil
}
func (s *CoreOAuthService) GetProfile(ctx context.Context, ref string) (*CoreOAuthProfile, error) {
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	view := s.view(record)
	return &view, nil
}
func (s *CoreOAuthService) ListProfiles(ctx context.Context) ([]CoreOAuthProfile, error) {
	if s == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	rows, err := s.repo.ListProfiles(ctx)
	if err != nil {
		return nil, ErrCoreOAuthStorage
	}
	out := make([]CoreOAuthProfile, 0, len(rows))
	for _, row := range rows {
		record, err := s.unseal(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s.view(record))
	}
	return out, nil
}
func (s *CoreOAuthService) transport(ctx context.Context, record *coreOAuthRecord) (CoreOAuthTransport, error) {
	transport := CoreOAuthTransport{ProxyID: record.ProxyID}
	if record.ProxyID == nil {
		return transport, nil
	}
	if s.proxyRepo == nil {
		return transport, ErrCoreOAuthUnavailable
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *record.ProxyID)
	if err != nil || proxy == nil {
		return transport, ErrCoreOAuthUnavailable
	}
	transport.ProxyURL = proxy.URL()
	return transport, nil
}
func coreOAuthAttemptValid(provider string, attempt *CoreOAuthProtocolAttempt) bool {
	if attempt == nil || attempt.State == nil || !attempt.ExpiresAt.After(time.Now()) || attempt.ExpiresAt.After(time.Now().Add(31*time.Minute)) {
		return false
	}
	parsed, err := url.Parse(attempt.AuthorizationURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if provider == CoreOAuthProviderExcel {
		return parsed.Path == "/api/accounts/authorize" && attempt.RedirectURI == "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
	}
	return provider == CoreOAuthProviderCodex && parsed.Path == "/oauth/authorize" && attempt.RedirectURI == "http://localhost:1455/auth/callback"
}
func (s *CoreOAuthService) StartAuthorization(ctx context.Context, ref string, actorID int64) (*CoreOAuthAuthorization, error) {
	if actorID <= 0 {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if record.Handoff != nil && record.Handoff.Status == "pending" {
		if !record.Handoff.ExpiresAt.After(time.Now().UTC()) {
			return nil, ErrCoreOAuthHandoffExpired
		}
		return nil, ErrCoreOAuthHandoffPending
	}
	protocol := s.protocols[record.Provider]
	if protocol == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	if record.Operation != nil && record.Operation.ExpiresAt.After(time.Now()) {
		return nil, ErrCoreOAuthBusy
	}
	transport, err := s.transport(ctx, record)
	if err != nil {
		return nil, err
	}
	attempt, err := protocol.Begin(ctx, transport)
	if err != nil || !coreOAuthAttemptValid(record.Provider, attempt) {
		return nil, ErrCoreOAuthProviderRejected
	}
	attemptID := uuid.NewString()
	s.mu.Lock()
	for id, old := range s.attempts {
		if !old.ExpiresAt.After(time.Now()) {
			delete(s.attempts, id)
		}
	}
	if len(s.attempts) >= 256 {
		s.mu.Unlock()
		return nil, ErrCoreOAuthBusy
	}
	s.attempts[attemptID] = attempt
	s.mu.Unlock()
	previousAttempt := record.Attempt
	if record.Operation != nil {
		record.NeedsAuthorization = true
	}
	record.Operation = nil
	record.Attempt = &coreOAuthStoredAttempt{ID: attemptID, ActorID: actorID, Status: "pending", ExpiresAt: attempt.ExpiresAt}
	if err = s.save(ctx, record); err != nil {
		s.forgetAttempt(attemptID)
		return nil, err
	}
	if previousAttempt != nil {
		s.forgetAttempt(previousAttempt.ID)
	}
	return &CoreOAuthAuthorization{Profile: s.view(record), AttemptID: attemptID, AuthorizationURL: attempt.AuthorizationURL, RedirectURI: attempt.RedirectURI, ExpiresAt: attempt.ExpiresAt}, nil
}
func (s *CoreOAuthService) forgetAttempt(id string) {
	s.mu.Lock()
	delete(s.attempts, id)
	s.mu.Unlock()
}
func coreOAuthPersistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func (s *CoreOAuthService) CompleteAuthorization(ctx context.Context, ref string, input CoreOAuthCompleteInput) (*CoreOAuthProfile, error) {
	if input.ActorID <= 0 || !coreOAuthRef(input.AttemptID) || input.CallbackURL == "" || len(input.CallbackURL) > 16384 || strings.ContainsAny(input.CallbackURL, "\r\n") {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if record.Handoff != nil && record.Handoff.Status == "pending" {
		if !record.Handoff.ExpiresAt.After(time.Now().UTC()) {
			return nil, ErrCoreOAuthHandoffExpired
		}
		return nil, ErrCoreOAuthHandoffPending
	}
	attempt := record.Attempt
	if attempt == nil || attempt.ID != input.AttemptID {
		return nil, ErrCoreOAuthConflict
	}
	if attempt.ActorID != input.ActorID {
		return nil, ErrCoreOAuthAttemptForbidden
	}
	if record.Operation != nil {
		if record.Operation.Kind == "exchange" && !record.Operation.ExpiresAt.After(time.Now()) {
			return nil, ErrCoreOAuthAttemptExpired
		}
		return nil, ErrCoreOAuthBusy
	}
	if attempt.Status != "pending" {
		return nil, ErrCoreOAuthConflict
	}
	s.mu.Lock()
	transaction := s.attempts[attempt.ID]
	s.mu.Unlock()
	if !attempt.ExpiresAt.After(time.Now()) || transaction == nil {
		attempt.Status = "expired"
		persistCtx, cancel := coreOAuthPersistContext(ctx)
		defer cancel()
		if err = s.save(persistCtx, record); err != nil {
			return nil, err
		}
		s.forgetAttempt(attempt.ID)
		return nil, ErrCoreOAuthAttemptExpired
	}
	protocol := s.protocols[record.Provider]
	if protocol == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	transport, err := s.transport(ctx, record)
	if err != nil {
		return nil, err
	}
	expected := CoreOAuthIdentity{}
	if record.Grant != nil {
		expected = CoreOAuthIdentity{Subject: record.Grant.Subject, WorkspaceID: record.Grant.WorkspaceID, Email: record.Grant.Email}
	}
	record.Attempt.Status = "exchanging"
	record.Operation = &coreOAuthOperation{ID: uuid.NewString(), Kind: "exchange", ExpiresAt: time.Now().Add(45 * time.Second)}
	if err = s.save(ctx, record); err != nil {
		return nil, err
	}
	s.forgetAttempt(attempt.ID)
	requestCtx, requestCancel := context.WithTimeout(ctx, 35*time.Second)
	grant, exchangeErr := protocol.ExchangeCallback(requestCtx, transaction, input.CallbackURL, expected, transport)
	requestCancel()
	persistCtx, cancel := coreOAuthPersistContext(ctx)
	defer cancel()
	record.Operation = nil
	if exchangeErr != nil || !coreOAuthValidGrant(record.Provider, grant, record.Grant) {
		record.Attempt.Status = "failed"
		result := ErrCoreOAuthProviderRejected
		var classified *CoreOAuthProtocolError
		if !errors.As(exchangeErr, &classified) || classified == nil || classified.OutcomeUnknown {
			record.NeedsAuthorization = true
			result = ErrCoreOAuthAuthorizationRequired
		} else if classified.SafeCode == "CORE_OAUTH_REGION_UNSUPPORTED" {
			result = ErrCoreOAuthRegionUnsupported
		}
		record.LastErrorCode = result.Reason
		if err = s.save(persistCtx, record); err != nil {
			return nil, err
		}
		return nil, result
	}
	// Reauthorization already verified the bound identity above; retain its
	// epoch so existing deployment, session and resource bindings still match.
	if record.Grant == nil {
		record.IdentityEpoch = uuid.NewString()
	}
	record.Grant = coreOAuthStoreGrant(*grant)
	record.TokenRevision++
	record.NeedsAuthorization = false
	record.Attempt.Status = "consumed"
	record.LastErrorCode = ""
	if err = s.save(persistCtx, record); err != nil {
		return nil, err
	}
	view := s.view(record)
	return &view, nil
}
func (s *CoreOAuthService) CancelAuthorization(ctx context.Context, ref, attemptID string, actorID int64) (*CoreOAuthProfile, error) {
	if actorID <= 0 || !coreOAuthRef(attemptID) {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if record.Handoff != nil && record.Handoff.Status == "pending" {
		if !record.Handoff.ExpiresAt.After(time.Now().UTC()) {
			return nil, ErrCoreOAuthHandoffExpired
		}
		return nil, ErrCoreOAuthHandoffPending
	}
	if record.Attempt == nil || record.Attempt.ID != attemptID {
		return nil, ErrCoreOAuthConflict
	}
	if record.Attempt.ActorID != actorID {
		return nil, ErrCoreOAuthAttemptForbidden
	}
	if record.Operation != nil && record.Operation.ExpiresAt.After(time.Now()) {
		return nil, ErrCoreOAuthBusy
	}
	if record.Operation != nil && record.Operation.Kind == "exchange" {
		record.Operation = nil
		record.NeedsAuthorization = true
	} else if record.Attempt.Status == "expired" || !record.Attempt.ExpiresAt.After(time.Now()) {
		// An expired transaction is no longer usable, but it still needs an
		// explicit terminal state so the admin UI can clear the stale attempt.
		record.Attempt.Status = "canceled"
	} else if record.Attempt.Status != "pending" {
		return nil, ErrCoreOAuthConflict
	}
	record.Attempt.Status = "canceled"
	if err = s.save(ctx, record); err != nil {
		return nil, err
	}
	s.forgetAttempt(attemptID)
	view := s.view(record)
	return &view, nil
}

// BeginHandoff records a receiver-side ownership transaction without importing
// a refresh token. It is deliberately a durable closed-world fence: until a
// future authenticated transfer protocol is implemented, the profile remains
// blocked until the reservation is explicitly abandoned; expiry never unlocks it.
func (s *CoreOAuthService) BeginHandoff(ctx context.Context, ref string, input CoreOAuthHandoffInput) (*CoreOAuthHandoffReceipt, error) {
	if s == nil || !validCoreOAuthHandoffPayload(input) || input.ProfileRef != ref {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	digest := coreOAuthHandoffDigest(input.ProtectedPayload)
	if existing := record.Handoff; existing != nil {
		if existing.OperationID == input.OperationID && existing.ActorID == input.ActorID && existing.PayloadDigest == digest &&
			existing.Provider == input.Provider && existing.ProfileRef == input.ProfileRef && existing.SourceProfileRef == input.SourceProfileRef && existing.Subject == input.Subject &&
			existing.WorkspaceID == input.WorkspaceID && existing.IdentityEpoch == input.IdentityEpoch && existing.TokenRevision == input.TokenRevision && existing.ExpiresAt.Equal(input.ExpiresAt) {
			return coreOAuthHandoffReceipt(record), nil
		}
		return nil, ErrCoreOAuthConflict
	}
	if !input.ExpiresAt.After(time.Now().UTC()) || input.ExpiresAt.After(time.Now().UTC().Add(15*time.Minute)) {
		return nil, ErrCoreOAuthInvalid
	}
	if record.Provider != input.Provider {
		return nil, ErrCoreOAuthConflict
	}
	if record.Grant != nil || !record.NeedsAuthorization || record.Operation != nil || record.Attempt != nil {
		return nil, ErrCoreOAuthConflict
	}
	record.Handoff = &coreOAuthHandoffRecord{OperationID: input.OperationID, Provider: input.Provider, ProfileRef: input.ProfileRef,
		SourceProfileRef: input.SourceProfileRef,
		Subject:          input.Subject, WorkspaceID: input.WorkspaceID, IdentityEpoch: input.IdentityEpoch, TokenRevision: input.TokenRevision,
		PayloadDigest: digest, ProtectedPayload: input.ProtectedPayload, ActorID: input.ActorID, Status: "pending", ExpiresAt: input.ExpiresAt.UTC()}
	if err := s.save(ctx, record); err != nil {
		return nil, err
	}
	return coreOAuthHandoffReceipt(record), nil
}

func (s *CoreOAuthService) GetHandoff(ctx context.Context, ref, operationID string, actorID int64) (*CoreOAuthHandoffReceipt, error) {
	if actorID <= 0 || !coreOAuthRef(ref) || !coreOAuthRef(operationID) {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if record.Handoff == nil || record.Handoff.OperationID != operationID {
		return nil, ErrCoreOAuthConflict
	}
	if record.Handoff.ActorID != actorID {
		return nil, ErrCoreOAuthAttemptForbidden
	}
	return coreOAuthHandoffReceipt(record), nil
}

// AbandonHandoff terminates a receiver reservation that has never activated a
// grant. It does not restore source authority or import the staged payload.
func (s *CoreOAuthService) AbandonHandoff(ctx context.Context, ref, operationID string, actorID int64) (*CoreOAuthHandoffReceipt, error) {
	if actorID <= 0 || !coreOAuthRef(ref) || !coreOAuthRef(operationID) {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	h := record.Handoff
	if h == nil || h.OperationID != operationID {
		return nil, ErrCoreOAuthConflict
	}
	if h.ActorID != actorID {
		return nil, ErrCoreOAuthAttemptForbidden
	}
	if h.Status == "abandoned" {
		return coreOAuthHandoffReceipt(record), nil
	}
	if h.Status != "pending" || record.Grant != nil || record.Operation != nil || record.Attempt != nil {
		return nil, ErrCoreOAuthConflict
	}
	h.Status = "abandoned"
	h.ProtectedPayload = ""
	h.TransferPrivateKey = ""
	if err := s.save(ctx, record); err != nil {
		return nil, err
	}
	return coreOAuthHandoffReceipt(record), nil
}

func coreOAuthStoreGrant(grant CoreOAuthGrant) *coreOAuthStoredGrant {
	return &coreOAuthStoredGrant{AccessToken: grant.AccessToken, RefreshToken: grant.RefreshToken, IDToken: grant.IDToken, ClientID: grant.ClientID, Subject: grant.Subject, WorkspaceID: grant.WorkspaceID, Email: grant.Email, ExpiresAt: grant.ExpiresAt}
}
func coreOAuthGrantValue(grant *coreOAuthStoredGrant) CoreOAuthGrant {
	return CoreOAuthGrant{AccessToken: grant.AccessToken, RefreshToken: grant.RefreshToken, IDToken: grant.IDToken, ClientID: grant.ClientID, Subject: grant.Subject, WorkspaceID: grant.WorkspaceID, Email: grant.Email, ExpiresAt: grant.ExpiresAt}
}
func coreOAuthValidGrant(provider string, grant *CoreOAuthGrant, previous *coreOAuthStoredGrant) bool {
	if grant == nil || grant.ClientID != coreOAuthClientID(provider) || !coreOAuthSecretText(grant.AccessToken) || !coreOAuthSecretText(grant.RefreshToken) || !coreOAuthIdentityText(grant.Subject) || !coreOAuthIdentityText(grant.WorkspaceID) || len(grant.IDToken) > 65536 || !grant.ExpiresAt.After(time.Now()) {
		return false
	}
	return previous == nil || (grant.Subject == previous.Subject && grant.WorkspaceID == previous.WorkspaceID && grant.ClientID == previous.ClientID && (previous.Email == "" || strings.EqualFold(previous.Email, grant.Email)))
}
func (s *CoreOAuthService) view(record *coreOAuthRecord) CoreOAuthProfile {
	view := CoreOAuthProfile{ProfileRef: record.ProfileRef, Provider: record.Provider, Name: record.Name, Status: "empty", NeedsAuthorization: record.NeedsAuthorization, IdentityEpoch: record.IdentityEpoch, TokenRevision: record.TokenRevision, LastErrorCode: record.LastErrorCode, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	if grant := record.Grant; grant != nil {
		view.Status = "authorized"
		view.Subject = grant.Subject
		view.WorkspaceID = grant.WorkspaceID
		view.Email = grant.Email
		view.ClientID = grant.ClientID
		expiry := grant.ExpiresAt
		view.ExpiresAt = &expiry
		view.Authorized = !record.NeedsAuthorization && expiry.After(time.Now())
	}
	if record.NeedsAuthorization && record.Grant != nil {
		view.Status = "reauthorization_required"
	}
	if attempt := record.Attempt; attempt != nil {
		status := attempt.Status
		if status == "pending" {
			s.mu.Lock()
			_, local := s.attempts[attempt.ID]
			s.mu.Unlock()
			// PKCE is process-local. Project availability without invalidating an
			// attempt that another live instance can still complete.
			if !local || !attempt.ExpiresAt.After(time.Now()) {
				status = "expired"
			}
		}
		view.Attempt = &CoreOAuthAttemptView{AttemptID: attempt.ID, Provider: record.Provider, Status: status, ExpiresAt: attempt.ExpiresAt}
		if status == "pending" {
			view.Status = "authorizing"
		}
	}
	if op := record.Operation; op != nil {
		if op.Kind == "exchange" {
			view.Status = "authorizing"
			view.Authorized = false
			if !op.ExpiresAt.After(time.Now()) {
				view.Status = "reauthorization_required"
				view.NeedsAuthorization = true
				if view.Attempt != nil {
					view.Attempt.Status = "expired"
				}
			}
		} else {
			view.Status = "refreshing"
			view.Authorized = false
			if !op.ExpiresAt.After(time.Now()) || op.Kind == "refresh_uncertain" {
				view.Status = "refresh_uncertain"
				view.NeedsAuthorization = true
			}
		}
	}
	if record.Handoff != nil && record.Handoff.Status == "pending" {
		view.Status = "handoff_pending"
		view.Authorized = false
		view.NeedsAuthorization = true
	}
	return view
}
