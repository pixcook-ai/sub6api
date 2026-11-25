package service

import (
	"context"
	"net/http"
)

const BPSPublicationFenceProofContract = "bps-publication-fence-proof/v1"

type BPSPublicationIdentity struct {
	ManagedOAuthIdentity
	TokenRevision int64 `json:"token_revision"`
}

type BPSPublicationFenceReceipt struct {
	BPSPublicationIdentity
	OperationID string `json:"operation_id"`
}

type BPSPublicationFenceProof struct {
	Contract string                      `json:"contract"`
	Identity BPSPublicationIdentity      `json:"identity"`
	Fenced   bool                        `json:"fenced"`
	Receipt  *BPSPublicationFenceReceipt `json:"receipt"`
}

// Read with the saved Files service key. It confers no worker control writes.
func (b *BPSFilesBackend) PublicationFence(ctx context.Context) (*BPSPublicationFenceProof, error) {
	req, err := b.request(ctx, http.MethodGet, "publication-fence", nil, "")
	if err != nil {
		return nil, err
	}
	var proof BPSPublicationFenceProof
	if err := b.execute(req, &proof, false); err != nil {
		return nil, err
	}
	id := proof.Identity
	if proof.Contract != BPSPublicationFenceProofContract || id.Contract != ManagedOAuthContract || id.Provider != CoreOAuthProviderExcel ||
		id.BackendID != b.config.Expected.BackendID || id.IdentityEpoch != b.config.Expected.CredentialGeneration ||
		!coreOAuthHandoffSourceRef(id.ProfileRef) || !validManagedOAuthOpaque(id.BindingRef) || !coreOAuthRef(id.IdentityEpoch) ||
		!coreOAuthIdentityText(id.Subject) || !coreOAuthIdentityText(id.WorkspaceID) || id.TokenRevision < 1 || id.TokenRevision > managedOAuthMaxRevision {
		return nil, bpsFilesError("publication_fence_identity_mismatch")
	}
	if proof.Fenced != (proof.Receipt != nil) || (proof.Receipt != nil &&
		(proof.Receipt.BPSPublicationIdentity != id || !coreOAuthRef(proof.Receipt.OperationID))) {
		return nil, bpsFilesError("publication_fence_receipt_mismatch")
	}
	return &proof, nil
}
