package service

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"
)

const coreOAuthTransferCiphertextLimit = 160 << 10
const coreOAuthTransferMaxRevision int64 = 9007199254740991

type coreOAuthTransferPlaintext struct {
	Contract          string                 `json:"contract"`
	OperationID       string                 `json:"operation_id"`
	ProfileRef        string                 `json:"profile_ref"`
	SourceProfileRef  string                 `json:"source_profile_ref"`
	Provider          string                 `json:"provider"`
	Subject           string                 `json:"subject"`
	WorkspaceID       string                 `json:"workspace_id"`
	IdentityEpoch     string                 `json:"identity_epoch"`
	TokenRevision     int64                  `json:"token_revision"`
	SourceQuiesced    bool                   `json:"source_quiesced"`
	PublicationFences []json.RawMessage      `json:"publication_fences"`
	Grant             coreOAuthTransferGrant `json:"grant"`
}

// This type is private and never used as a response DTO.
type coreOAuthTransferGrant struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	ClientID     string    `json:"client_id"`
	Subject      string    `json:"subject"`
	WorkspaceID  string    `json:"workspace_id"`
	Email        string    `json:"email"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func coreOAuthInvitation(h *coreOAuthHandoffRecord) *CoreOAuthHandoffInvitation {
	return &CoreOAuthHandoffInvitation{Contract: h.TransferContract, OperationID: h.OperationID, ProfileRef: h.ProfileRef,
		SourceProfileRef: h.SourceProfileRef, Provider: h.Provider, Subject: h.Subject, WorkspaceID: h.WorkspaceID,
		IdentityEpoch: h.IdentityEpoch, TokenRevision: h.TokenRevision, ExpiresAt: h.ExpiresAt, PublicKey: h.TransferPublicKey}
}

func coreOAuthInvitationMatches(h *coreOAuthHandoffRecord, in CoreOAuthHandoffInvitationInput) bool {
	return h.TransferContract == CoreOAuthHandoffTransferContract && h.OperationID == in.OperationID && h.ProfileRef == in.ProfileRef &&
		h.SourceProfileRef == in.SourceProfileRef && h.Provider == in.Provider && h.Subject == in.Subject &&
		h.WorkspaceID == in.WorkspaceID && h.IdentityEpoch == in.IdentityEpoch && h.TokenRevision == in.TokenRevision && h.ExpiresAt.Equal(in.ExpiresAt)
}

func coreOAuthInvitationInputValid(ref string, in CoreOAuthHandoffInvitationInput) bool {
	return in.ActorID > 0 && ref == in.ProfileRef && coreOAuthRef(ref) && coreOAuthRef(in.OperationID) &&
		coreOAuthHandoffSourceRef(in.SourceProfileRef) && coreOAuthProvider(in.Provider) && coreOAuthIdentityText(in.Subject) &&
		coreOAuthIdentityText(in.WorkspaceID) && coreOAuthRef(in.IdentityEpoch) && in.TokenRevision > 0 && in.TokenRevision <= coreOAuthTransferMaxRevision && !in.ExpiresAt.IsZero()
}

// CreateHandoffInvitation installs the durable receiver barrier before returning
// its public key. Its private key exists only inside the sealed profile record.
func (s *CoreOAuthService) CreateHandoffInvitation(ctx context.Context, ref string, in CoreOAuthHandoffInvitationInput) (*CoreOAuthHandoffInvitation, error) {
	if s == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	if !coreOAuthInvitationInputValid(ref, in) {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	if h := record.Handoff; h != nil {
		if h.ActorID != in.ActorID {
			return nil, ErrCoreOAuthAttemptForbidden
		}
		if !coreOAuthInvitationMatches(h, in) {
			return nil, ErrCoreOAuthConflict
		}
		return coreOAuthInvitation(h), nil
	}
	now := time.Now().UTC()
	if !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(15*time.Minute)) {
		return nil, ErrCoreOAuthInvalid
	}
	if record.Provider != in.Provider || record.Grant != nil || record.TokenRevision != 0 || record.IdentityEpoch != "" ||
		!record.NeedsAuthorization || record.Operation != nil || record.Attempt != nil {
		return nil, ErrCoreOAuthConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, ErrCoreOAuthUnavailable
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, ErrCoreOAuthUnavailable
	}
	defer clear(privateDER)
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, ErrCoreOAuthUnavailable
	}
	public := base64.RawURLEncoding.EncodeToString(publicDER)
	record.Handoff = &coreOAuthHandoffRecord{OperationID: in.OperationID, Provider: in.Provider, ProfileRef: ref, SourceProfileRef: in.SourceProfileRef,
		Subject: in.Subject, WorkspaceID: in.WorkspaceID, IdentityEpoch: in.IdentityEpoch, TokenRevision: in.TokenRevision, ActorID: in.ActorID,
		Status: "pending", ExpiresAt: in.ExpiresAt.UTC(), TransferContract: CoreOAuthHandoffTransferContract, TransferPublicKey: public,
		TransferPrivateKey: base64.RawURLEncoding.EncodeToString(privateDER), PayloadDigest: coreOAuthHandoffDigest(public)}
	if err = s.save(ctx, record); err != nil {
		// A concurrent identical invitation can be reconciled without publishing
		// this uncommitted key. Storage errors are never reported as success.
		if err == ErrCoreOAuthConflict {
			if current, readErr := s.load(ctx, ref); readErr == nil && current.Handoff != nil && current.Handoff.ActorID == in.ActorID && coreOAuthInvitationMatches(current.Handoff, in) {
				return coreOAuthInvitation(current.Handoff), nil
			}
		}
		return nil, err
	}
	return coreOAuthInvitation(record.Handoff), nil
}

func coreOAuthRawURL(value string, limit int) ([]byte, bool) {
	if value == "" || len(value) > base64.RawURLEncoding.EncodedLen(limit) {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return decoded, err == nil && len(decoded) <= limit && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func coreOAuthEnvelopeDigest(e CoreOAuthHandoffEnvelope) (string, bool) {
	if !coreOAuthRef(e.OperationID) {
		return "", false
	}
	wrapped, ok := coreOAuthRawURL(e.WrappedKey, 256)
	if !ok || len(wrapped) != 256 {
		return "", false
	}
	nonce, ok := coreOAuthRawURL(e.Nonce, 12)
	if !ok || len(nonce) != 12 {
		return "", false
	}
	sealed, ok := coreOAuthRawURL(e.Ciphertext, coreOAuthTransferCiphertextLimit)
	if !ok || len(sealed) < 17 {
		return "", false
	}
	return coreOAuthHandoffDigest(e.OperationID + "\n" + e.WrappedKey + "\n" + e.Nonce + "\n" + e.Ciphertext), true
}

// AcceptProtectedHandoff relies on the trusted source protocol to freeze and
// drain the source. A required internal verifier checks every submitted source
// inventory entry against trusted saved endpoints. It cannot establish that no
// unknown vault copies or external systems exist. Activation and its receipt
// are one CAS; no token request occurs here.
func (s *CoreOAuthService) AcceptProtectedHandoff(ctx context.Context, ref string, actorID int64, envelope CoreOAuthHandoffEnvelope, verifier CoreOAuthHandoffVerifier) (*CoreOAuthHandoffReceipt, error) {
	if s == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	if actorID <= 0 || !coreOAuthRef(ref) {
		return nil, ErrCoreOAuthInvalid
	}
	digest, valid := coreOAuthEnvelopeDigest(envelope)
	if !valid {
		return nil, ErrCoreOAuthInvalid
	}
	record, err := s.load(ctx, ref)
	if err != nil {
		return nil, err
	}
	h := record.Handoff
	if h == nil || h.OperationID != envelope.OperationID || h.TransferContract != CoreOAuthHandoffTransferContract {
		return nil, ErrCoreOAuthConflict
	}
	if h.ActorID != actorID {
		return nil, ErrCoreOAuthAttemptForbidden
	}
	if verifier == nil {
		return nil, ErrCoreOAuthUnavailable
	}
	if h.Status == "accepted" {
		if h.PayloadDigest != digest {
			return nil, ErrCoreOAuthConflict
		}
		return coreOAuthHandoffReceipt(record), nil
	}
	if h.Status != "pending" || record.Grant != nil || record.Operation != nil || record.Attempt != nil {
		return nil, ErrCoreOAuthConflict
	}
	if !h.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrCoreOAuthHandoffExpired
	}
	plain, err := coreOAuthOpenTransfer(h, envelope)
	if err != nil {
		return nil, ErrCoreOAuthInvalid
	}
	defer clear(plain)
	payload, err := coreOAuthDecodeTransfer(plain)
	if err != nil || !coreOAuthTransferMatches(h, payload) {
		return nil, ErrCoreOAuthInvalid
	}
	grant := CoreOAuthGrant{AccessToken: payload.Grant.AccessToken, RefreshToken: payload.Grant.RefreshToken, IDToken: payload.Grant.IDToken,
		ClientID: payload.Grant.ClientID, Subject: payload.Grant.Subject, WorkspaceID: payload.Grant.WorkspaceID, Email: payload.Grant.Email, ExpiresAt: payload.Grant.ExpiresAt}
	if !coreOAuthValidGrant(record.Provider, &grant, nil) || len(grant.Email) > 254 || strings.ContainsAny(grant.Email, "\r\n\x00") {
		return nil, ErrCoreOAuthInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	verification := CoreOAuthHandoffVerification{Invitation: *coreOAuthInvitation(h), SourceQuiesced: true, PublicationFences: payload.PublicationFences}
	if err = verifier(ctx, verification); err != nil {
		return nil, ErrCoreOAuthConflict
	}
	if !h.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrCoreOAuthHandoffExpired
	}
	if !grant.ExpiresAt.After(time.Now().UTC()) {
		return nil, ErrCoreOAuthInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record.Grant = coreOAuthStoreGrant(grant)
	record.IdentityEpoch = h.IdentityEpoch
	record.TokenRevision = h.TokenRevision
	record.NeedsAuthorization = false
	record.LastErrorCode = ""
	h.Status = "accepted"
	h.PayloadDigest = digest
	h.TransferPrivateKey = ""
	h.ProtectedPayload = ""
	if err = s.save(ctx, record); err != nil {
		if err == ErrCoreOAuthConflict {
			if current, readErr := s.load(ctx, ref); readErr == nil && current.Handoff != nil && current.Handoff.Status == "accepted" && current.Handoff.ActorID == actorID && current.Handoff.OperationID == envelope.OperationID && current.Handoff.PayloadDigest == digest {
				return coreOAuthHandoffReceipt(current), nil
			}
		}
		return nil, err
	}
	return coreOAuthHandoffReceipt(record), nil
}

func coreOAuthOpenTransfer(h *coreOAuthHandoffRecord, envelope CoreOAuthHandoffEnvelope) ([]byte, error) {
	privateDER, ok := coreOAuthRawURL(h.TransferPrivateKey, 4096)
	if !ok {
		return nil, ErrCoreOAuthInvalid
	}
	defer clear(privateDER)
	parsed, err := x509.ParsePKCS8PrivateKey(privateDER)
	if err != nil {
		return nil, ErrCoreOAuthInvalid
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok || key.N.BitLen() != 2048 {
		return nil, ErrCoreOAuthInvalid
	}
	wrapped, _ := coreOAuthRawURL(envelope.WrappedKey, 256)
	symmetric, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, wrapped, nil)
	if err != nil || len(symmetric) != 32 {
		return nil, ErrCoreOAuthInvalid
	}
	defer clear(symmetric)
	block, err := aes.NewCipher(symmetric)
	if err != nil {
		return nil, ErrCoreOAuthInvalid
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCoreOAuthInvalid
	}
	nonce, _ := coreOAuthRawURL(envelope.Nonce, 12)
	sealed, _ := coreOAuthRawURL(envelope.Ciphertext, coreOAuthTransferCiphertextLimit)
	aad := []byte(CoreOAuthHandoffTransferContract + "\n" + h.ProfileRef + "\n" + h.OperationID)
	return gcm.Open(nil, nonce, sealed, aad)
}

func coreOAuthTransferMatches(h *coreOAuthHandoffRecord, p *coreOAuthTransferPlaintext) bool {
	return p != nil && p.Contract == h.TransferContract && p.OperationID == h.OperationID && p.ProfileRef == h.ProfileRef &&
		p.SourceProfileRef == h.SourceProfileRef && p.Provider == h.Provider && p.Subject == h.Subject && p.WorkspaceID == h.WorkspaceID &&
		p.IdentityEpoch == h.IdentityEpoch && p.TokenRevision == h.TokenRevision && p.SourceQuiesced &&
		p.Grant.Subject == h.Subject && p.Grant.WorkspaceID == h.WorkspaceID
}

func coreOAuthTransferRecordValid(record *coreOAuthRecord) bool {
	h := record.Handoff
	if h == nil || h.TransferContract != CoreOAuthHandoffTransferContract || h.ProfileRef != record.ProfileRef || h.Provider != record.Provider ||
		!coreOAuthRef(h.OperationID) || !coreOAuthHandoffSourceRef(h.SourceProfileRef) || !coreOAuthIdentityText(h.Subject) || !coreOAuthIdentityText(h.WorkspaceID) ||
		!coreOAuthRef(h.IdentityEpoch) || h.TokenRevision < 1 || h.TokenRevision > coreOAuthTransferMaxRevision || h.ActorID < 1 || h.ExpiresAt.IsZero() || len(h.PayloadDigest) != 64 || h.ProtectedPayload != "" {
		return false
	}
	der, ok := coreOAuthRawURL(h.TransferPublicKey, 1024)
	if !ok {
		return false
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	public, keyOK := parsed.(*rsa.PublicKey)
	if err != nil || !keyOK || public.N.BitLen() != 2048 || public.E != 65537 {
		return false
	}
	switch h.Status {
	case "pending":
		if record.Grant != nil || !record.NeedsAuthorization || record.Operation != nil || record.Attempt != nil || record.TokenRevision != 0 || record.IdentityEpoch != "" || h.PayloadDigest != coreOAuthHandoffDigest(h.TransferPublicKey) {
			return false
		}
		privateDER, ok := coreOAuthRawURL(h.TransferPrivateKey, 4096)
		if !ok {
			return false
		}
		defer clear(privateDER)
		privateValue, err := x509.ParsePKCS8PrivateKey(privateDER)
		private, keyOK := privateValue.(*rsa.PrivateKey)
		return err == nil && keyOK && private.Validate() == nil && private.N.Cmp(public.N) == 0 && private.E == public.E
	case "accepted":
		// The accepted receipt keeps the original revision; normal core refreshes
		// may advance the live grant without changing the immutable receipt.
		return h.TransferPrivateKey == "" && record.Grant != nil && record.IdentityEpoch == h.IdentityEpoch && record.TokenRevision >= h.TokenRevision &&
			record.Grant.Subject == h.Subject && record.Grant.WorkspaceID == h.WorkspaceID && record.Grant.ClientID == coreOAuthClientID(h.Provider)
	case "abandoned":
		return h.TransferPrivateKey == ""
	default:
		return false
	}
}

func coreOAuthDecodeTransfer(raw []byte) (*coreOAuthTransferPlaintext, error) {
	if len(raw) > coreOAuthTransferCiphertextLimit-16 || !coreOAuthTransferUniqueJSON(raw) {
		return nil, ErrCoreOAuthInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 12 {
		return nil, ErrCoreOAuthInvalid
	}
	for _, name := range []string{"contract", "operation_id", "profile_ref", "source_profile_ref", "provider", "subject", "workspace_id", "identity_epoch", "token_revision", "source_quiesced", "publication_fences", "grant"} {
		if value, exists := fields[name]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrCoreOAuthInvalid
		}
	}
	var grantFields map[string]json.RawMessage
	if json.Unmarshal(fields["grant"], &grantFields) != nil || len(grantFields) != 8 {
		return nil, ErrCoreOAuthInvalid
	}
	for _, name := range []string{"access_token", "refresh_token", "id_token", "client_id", "subject", "workspace_id", "email", "expires_at"} {
		if value, exists := grantFields[name]; !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, ErrCoreOAuthInvalid
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload coreOAuthTransferPlaintext
	if dec.Decode(&payload) != nil || dec.Decode(new(any)) != io.EOF || payload.PublicationFences == nil || len(payload.PublicationFences) > 128 {
		return nil, ErrCoreOAuthInvalid
	}
	for _, proof := range payload.PublicationFences {
		var object map[string]json.RawMessage
		if len(proof) > 64<<10 || json.Unmarshal(proof, &object) != nil || len(object) == 0 {
			return nil, ErrCoreOAuthInvalid
		}
	}
	return &payload, nil
}

// Reject duplicate keys at every depth, including the opaque proof objects.
func coreOAuthTransferUniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
