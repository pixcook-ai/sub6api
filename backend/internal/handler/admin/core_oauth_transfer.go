package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type coreOAuthTransferService interface {
	CreateHandoffInvitation(context.Context, string, service.CoreOAuthHandoffInvitationInput) (*service.CoreOAuthHandoffInvitation, error)
	AcceptProtectedHandoff(context.Context, string, int64, service.CoreOAuthHandoffEnvelope, service.CoreOAuthHandoffVerifier) (*service.CoreOAuthHandoffReceipt, error)
	GetHandoff(context.Context, string, string, int64) (*service.CoreOAuthHandoffReceipt, error)
	AbandonHandoff(context.Context, string, string, int64) (*service.CoreOAuthHandoffReceipt, error)
}

// The ordinary constructor stays useful for tests; only production wiring installs live proof verification.
func ProvideCoreOAuthHandler(core *service.CoreOAuthService, cfg *config.Config, adminSvc service.AdminService, upstream service.HTTPUpstream) *CoreOAuthHandler {
	h := NewCoreOAuthHandler(core)
	h.SetHandoffVerifier(newCoreOAuthHandoffVerifier(adminSvc, upstream, cfg))
	return h
}

func (h *CoreOAuthHandler) transferService(c *gin.Context) (coreOAuthTransferService, bool) {
	if !h.available(c) {
		return nil, false
	}
	s, ok := h.core.(coreOAuthTransferService)
	if !ok {
		response.ErrorFrom(c, service.ErrCoreOAuthUnavailable)
	}
	return s, ok
}

func decodeCoreOAuthTransferJSON(raw []byte, target any, fields ...string) bool {
	if !exactPublicationFenceFields(raw, fields...) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func decodeCoreOAuthTransferBody(c *gin.Context, target any, fields ...string) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10))
	if err == nil && decodeCoreOAuthTransferJSON(raw, target, fields...) {
		return true
	}
	response.ErrorFrom(c, service.ErrCoreOAuthInvalidInput)
	return false
}

func (h *CoreOAuthHandler) CreateHandoffInvitation(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok {
		return
	}
	s, ok := h.transferService(c)
	if !ok {
		return
	}
	var in service.CoreOAuthHandoffInvitationInput
	if !decodeCoreOAuthTransferBody(c, &in, "operation_id", "profile_ref", "source_profile_ref", "provider", "subject", "workspace_id", "identity_epoch", "token_revision", "expires_at") {
		return
	}
	in.ActorID = actor
	result, err := s.CreateHandoffInvitation(c.Request.Context(), c.Param("ref"), in)
	if response.ErrorFrom(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, result)
}

func (h *CoreOAuthHandler) AcceptProtectedHandoff(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok {
		return
	}
	s, ok := h.transferService(c)
	if !ok {
		return
	}
	if h.handoffVerifier == nil {
		response.ErrorFrom(c, service.ErrCoreOAuthUnavailable)
		return
	}
	var in service.CoreOAuthHandoffEnvelope
	if !decodeCoreOAuthTransferBody(c, &in, "operation_id", "wrapped_key", "nonce", "ciphertext") {
		return
	}
	result, err := s.AcceptProtectedHandoff(c.Request.Context(), c.Param("ref"), actor, in, h.handoffVerifier)
	if response.ErrorFrom(c, err) {
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, result)
}

func (h *CoreOAuthHandler) GetHandoff(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok {
		return
	}
	s, ok := h.transferService(c)
	if !ok {
		return
	}
	result, err := s.GetHandoff(c.Request.Context(), c.Param("ref"), c.Param("operation_id"), actor)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, result)
}

func (h *CoreOAuthHandler) AbandonHandoff(c *gin.Context) {
	actor, ok := coreOAuthAdminActor(c)
	if !ok {
		return
	}
	s, ok := h.transferService(c)
	if !ok {
		return
	}
	var in struct{}
	if !decodeCoreOAuthTransferBody(c, &in) {
		return
	}
	result, err := s.AbandonHandoff(c.Request.Context(), c.Param("ref"), c.Param("operation_id"), actor)
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, result)
}

type coreOAuthHandoffProofAccountReader interface {
	GetAccount(context.Context, int64) (*service.Account, error)
}
type coreOAuthHandoffPublicationProof struct {
	Kind      string          `json:"kind"`
	AccountID int64           `json:"account_id"`
	Receipt   json.RawMessage `json:"receipt"`
}

func coreOAuthHandoffIdentityMatches(id service.ManagedOAuthIdentity, invitation service.CoreOAuthHandoffInvitation) bool {
	return id.Contract == service.ManagedOAuthContract && id.ProfileRef == invitation.SourceProfileRef && id.Provider == invitation.Provider &&
		id.Subject == invitation.Subject && id.WorkspaceID == invitation.WorkspaceID && id.IdentityEpoch == invitation.IdentityEpoch
}

func newCoreOAuthHandoffVerifier(adminSvc coreOAuthHandoffProofAccountReader, upstream service.HTTPUpstream, cfg *config.Config) service.CoreOAuthHandoffVerifier {
	return func(ctx context.Context, verification service.CoreOAuthHandoffVerification) error {
		if adminSvc == nil || !verification.SourceQuiesced || len(verification.PublicationFences) == 0 || len(verification.PublicationFences) > 128 {
			return service.ErrCoreOAuthConflict
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		invitation := verification.Invitation
		seenAccounts, seenBindings := map[int64]bool{}, map[string]bool{}
		for _, raw := range verification.PublicationFences {
			var proof coreOAuthHandoffPublicationProof
			if !decodeCoreOAuthTransferJSON(raw, &proof, "kind", "account_id", "receipt") || proof.AccountID <= 0 || seenAccounts[proof.AccountID] {
				return service.ErrCoreOAuthConflict
			}
			seenAccounts[proof.AccountID] = true
			account, err := adminSvc.GetAccount(ctx, proof.AccountID)
			if err != nil || account == nil || account.ID != proof.AccountID || account.Platform != service.PlatformOpenAI {
				return service.ErrCoreOAuthConflict
			}
			var identity service.ManagedOAuthIdentity
			switch proof.Kind {
			case "codex_controller":
				if invitation.Provider != service.CoreOAuthProviderCodex || account.Type != service.AccountTypeOAuth {
					return service.ErrCoreOAuthConflict
				}
				var receipt service.ManagedOAuthPublicationFence
				if !decodeCoreOAuthTransferJSON(proof.Receipt, &receipt, "operation_id", "identity", "token_revision", "account_id", "fenced") {
					return service.ErrCoreOAuthConflict
				}
				var receiptFields map[string]json.RawMessage
				_ = json.Unmarshal(proof.Receipt, &receiptFields)
				if !exactPublicationFenceFields(receiptFields["identity"], "contract", "backend_id", "binding_ref", "profile_ref", "provider", "subject", "workspace_id", "identity_epoch") {
					return service.ErrCoreOAuthConflict
				}
				identity = receipt.Identity
				if identity.Validate() != nil || !coreOAuthHandoffIdentityMatches(identity, invitation) || !receipt.Fenced || receipt.AccountID != proof.AccountID || receipt.OperationID != invitation.OperationID || receipt.TokenRevision != invitation.TokenRevision {
					return service.ErrCoreOAuthConflict
				}
				var state service.ManagedOAuthState
				saved, err := json.Marshal(account.Extra[service.ManagedOAuthExtraKey])
				if err != nil || json.Unmarshal(saved, &state) != nil || state.ManagedOAuthIdentity != identity || state.TokenRevision != receipt.TokenRevision || state.PublicationFenceOperationID != receipt.OperationID {
					return service.ErrCoreOAuthConflict
				}
				reader, ok := adminSvc.(service.ManagedOAuthPublicationFenceService)
				if !ok {
					return service.ErrCoreOAuthUnavailable
				}
				live, err := reader.GetManagedOAuthPublicationFence(ctx, proof.AccountID)
				if err != nil || live == nil || *live != receipt {
					return service.ErrCoreOAuthConflict
				}
			case "bps_worker":
				if invitation.Provider != service.CoreOAuthProviderExcel || account.Type != service.AccountTypeAPIKey || upstream == nil {
					return service.ErrCoreOAuthConflict
				}
				var receipt service.BPSPublicationFenceReceipt
				if !decodeCoreOAuthTransferJSON(proof.Receipt, &receipt, "contract", "backend_id", "binding_ref", "profile_ref", "provider", "subject", "workspace_id", "identity_epoch", "token_revision", "operation_id") {
					return service.ErrCoreOAuthConflict
				}
				identity = receipt.ManagedOAuthIdentity
				if !coreOAuthHandoffIdentityMatches(identity, invitation) || receipt.OperationID != invitation.OperationID || receipt.TokenRevision != invitation.TokenRevision {
					return service.ErrCoreOAuthConflict
				}
				var binding service.ManagedDeploymentBinding
				saved, err := json.Marshal(account.Extra[service.ManagedDeploymentAccountExtraKey])
				if err != nil || json.Unmarshal(saved, &binding) != nil {
					return service.ErrCoreOAuthConflict
				}
				if binding.AccountID == 0 {
					binding.AccountID = account.ID
				}
				if binding.Validate() != nil || binding.AccountID != account.ID || binding.Provider != identity.Provider || binding.BackendID != identity.BackendID || binding.BindingRef != identity.BindingRef || binding.ProfileRef != identity.ProfileRef || binding.IdentityEpoch != identity.IdentityEpoch || binding.Subject != identity.Subject || binding.WorkspaceID != identity.WorkspaceID {
					return service.ErrCoreOAuthConflict
				}
				// An unloaded or unrelated saved proxy must never turn proof
				// verification into a direct request or use another proxy.
				if account.ProxyID != nil {
					if *account.ProxyID <= 0 || account.Proxy == nil || account.Proxy.ID != *account.ProxyID {
						return service.ErrCoreOAuthConflict
					}
				} else if account.Proxy != nil {
					return service.ErrCoreOAuthConflict
				}
				backendConfig, err := service.BPSFilesConfigForAccount(account)
				if err != nil || backendConfig.Expected.BackendID != identity.BackendID || backendConfig.Expected.CredentialGeneration != identity.IdentityEpoch {
					return service.ErrCoreOAuthConflict
				}
				backend, err := service.NewBPSFilesBackend(backendConfig, upstream, cfg)
				if err != nil {
					return service.ErrCoreOAuthConflict
				}
				live, err := backend.PublicationFence(ctx)
				if err != nil || live == nil || !live.Fenced || live.Receipt == nil || *live.Receipt != receipt {
					return service.ErrCoreOAuthConflict
				}
			default:
				return service.ErrCoreOAuthConflict
			}
			if seenBindings[identity.BindingRef] {
				return service.ErrCoreOAuthConflict
			}
			seenBindings[identity.BindingRef] = true
		}
		return nil
	}
}
