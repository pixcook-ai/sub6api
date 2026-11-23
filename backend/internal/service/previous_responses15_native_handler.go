package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// StoredResponseNativeParent15 is private server state for the handler's exact
// owning-account pin. Returning it grants no generation admission: account,
// request authorization and resource validity must be checked after later waits.
type StoredResponseNativeParent15 struct {
	Account  *Account
	Binding  StoredResponseNativeReadBinding15
	Snapshot []byte
}

// ResolveStoredResponseNativeParent15 joins durable owner lookup with the same
// current account/group/route authorizers used before actual BPS admission. The
// caller supplies the original public model and its current server-resolved
// forwarding model; neither is inferred from private history's self-description.
// This helper does not change any public previous_response_id routing policy.
func (s *OpenAIGatewayService) ResolveStoredResponseNativeParent15(ctx context.Context, owner StoredResponseOwner, id, publicModel, forwardModel string) (*StoredResponseNativeParent15, error) {
	if s == nil || s.storedResponses == nil || !s.storedResponses.Enabled() {
		return nil, ErrStoredResponsesDisabled
	}
	if !previousResponseID15(id) {
		return nil, ErrStoredResponseNotFound
	}
	if publicModel == "" || forwardModel == "" || strings.TrimSpace(publicModel) != publicModel || strings.TrimSpace(forwardModel) != forwardModel {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	leaf, err := s.storedResponses.Get(ctx, owner, id)
	if err != nil {
		return nil, storedNativeParentLookupError15(ctx, err)
	}
	if err = storedNativeParentRow15(leaf, owner, id, publicModel); err != nil {
		return nil, err
	}
	accountID, resolvedModel := leaf.AccountID, leaf.ResolvedModel
	if s.accountRepo == nil || s.schedulerSnapshot == nil {
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	var baseline *Account
	var baselineConfig BPSStoredResponsesBackendConfig
	fence := func(account *Account) error {
		if account == nil || account.ID != accountID {
			return ErrBPSRequestAuthorizationRevoked
		}
		cfg, err := BPSStoredResponsesConfigForAccount(account)
		if err != nil || managedfiles.ValidateNativeResponsesServiceKey15(cfg.ServiceKey) != nil {
			return ErrBPSRequestAuthorizationRevoked
		}
		_, upstream := resolveOpenAIForwardMappedModels(account, forwardModel, false)
		if upstream != resolvedModel {
			return ErrBPSRequestAuthorizationRevoked
		}
		if baseline == nil {
			baseline = account
			baselineConfig = cfg
			return nil
		}
		if cfg != baselineConfig || account.GetCredential("api_key") != baseline.GetCredential("api_key") || account.GetCredential("base_url") != baseline.GetCredential("base_url") {
			return ErrBPSRequestAuthorizationRevoked
		}
		for _, model := range []string{publicModel, forwardModel} {
			for _, compact := range []bool{false, true} {
				oldBilling, oldUpstream := resolveOpenAIForwardMappedModels(baseline, model, compact)
				newBilling, newUpstream := resolveOpenAIForwardMappedModels(account, model, compact)
				if oldBilling != newBilling || oldUpstream != newUpstream {
					return ErrBPSRequestAuthorizationRevoked
				}
			}
		}
		return nil
	}
	current := func(checkCtx context.Context) (*Account, error) {
		if err := CheckBPSRequestAuthorization(checkCtx, publicModel); err != nil {
			return nil, err
		}
		selected, err := s.RecheckBPSStoredResponseAccount(checkCtx, &owner.GroupID, accountID, forwardModel)
		if err != nil {
			return nil, storedNativeParentAuthorityError15(checkCtx, err)
		}
		selected, err = cloneStoredNativeParentAccount15(selected)
		if err != nil {
			return nil, err
		}
		if err = fence(selected); err != nil {
			return nil, err
		}
		// This API explicitly requires the original public model. It refreshes
		// group allowlists and composite routing before another owning-row read.
		fresh, err := s.AuthorizeStoredResponseAccount(checkCtx, owner.GroupID, publicModel, selected)
		if err != nil {
			return nil, storedNativeParentAuthorityError15(checkCtx, err)
		}
		fresh, err = cloneStoredNativeParentAccount15(fresh)
		if err != nil {
			return nil, err
		}
		if err = fence(fresh); err != nil {
			return nil, err
		}
		if err = checkCtx.Err(); err != nil {
			return nil, err
		}
		return fresh, nil
	}
	selected, err := current(ctx)
	if err != nil {
		return nil, err
	}
	binding := StoredResponseNativeReadBinding15{PublicModel: publicModel, ResolvedModel: resolvedModel, AccountID: accountID, BackendID: baselineConfig.Expected.BackendID, SubjectDigest: managedfiles.NativeResponsesSubjectDigest15(baselineConfig.Expected), CredentialGeneration: baselineConfig.Expected.CredentialGeneration}
	// Refresh the owning leaf after the initial current-authorization wait.
	// Keep the complete-chain resolver last: it tracks the earliest visited
	// expiry after every account/group wait, so an older ancestor cannot expire
	// during an extra external lookup after that final chain check.
	latest, err := s.storedResponses.Get(ctx, owner, id)
	if err != nil {
		return nil, storedNativeParentLookupError15(ctx, err)
	}
	if err = storedNativeParentRow15(latest, owner, id, publicModel); err != nil {
		return nil, err
	}
	if latest.AccountID != accountID || latest.ResolvedModel != resolvedModel || latest.Version != leaf.Version || !bytes.Equal(latest.Recovery, leaf.Recovery) {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	snapshot, pinned, err := s.storedResponses.ResolvePreviousResponseNative15(ctx, owner, id, binding, func(checkCtx context.Context, currentOwner StoredResponseOwner, record *StoredResponse) error {
		if currentOwner != owner || record == nil || record.AccountID != accountID || record.ResolvedModel != resolvedModel {
			return ErrBPSRequestAuthorizationRevoked
		}
		if err := storedNativeParentRow15(record, owner, record.PublicID, publicModel); err != nil {
			return err
		}
		fresh, err := current(checkCtx)
		if err != nil {
			return err
		}
		// Refresh the resource after the account/group authorizers waited. A
		// detached repository row will not reflect a concurrent soft deletion.
		latest, err := s.storedResponses.Get(checkCtx, owner, record.PublicID)
		if err != nil {
			return storedNativeParentLookupError15(checkCtx, err)
		}
		if err = storedNativeParentRow15(latest, owner, record.PublicID, publicModel); err != nil {
			return err
		}
		if latest.AccountID != accountID || latest.ResolvedModel != resolvedModel || latest.Version != record.Version || !latest.ExpiresAt.Equal(record.ExpiresAt) || !bytes.Equal(latest.Recovery, record.Recovery) {
			return ErrBPSRequestAuthorizationRevoked
		}
		selected = fresh
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pinned != accountID {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &StoredResponseNativeParent15{Account: selected, Binding: binding, Snapshot: append([]byte(nil), snapshot...)}, nil
}

// authorizeStoredResponseNativeParentPlan15 is called by the existing admission
// authorizer at every wait boundary, including the final generation claim. It
// never replaces the owning account selected by the handler and never changes
// the exact parent snapshot that the native preflight already authenticated.
func (s *OpenAIGatewayService) authorizeStoredResponseNativeParentPlan15(ctx context.Context, plan *StoredResponsePlan, selected *Account, publicModel, forwardModel string) error {
	if plan == nil || plan.Request == nil || plan.native15 == nil {
		return ErrStoredResponseInvalid
	}
	if plan.native15.previousID == "" {
		if len(plan.native15.parentSnapshot) != 0 {
			return ErrStoredResponseInvalid
		}
		return ctx.Err()
	}
	if len(plan.native15.parentSnapshot) == 0 || selected == nil {
		return ErrStoredResponseInvalid
	}
	// Detach before external authorization waits so a repository that updates a
	// shared map in place cannot change both sides of the comparison at once.
	before, err := cloneStoredNativeParentAccount15(selected)
	if err != nil {
		return err
	}
	parent, err := s.ResolveStoredResponseNativeParent15(ctx, plan.Owner, plan.native15.previousID, publicModel, forwardModel)
	if err != nil {
		return err
	}
	if parent == nil || parent.Account == nil || parent.Account.ID != before.ID || parent.Binding.AccountID != before.ID || !bytes.Equal(parent.Snapshot, plan.native15.parentSnapshot) {
		return ErrBPSRequestAuthorizationRevoked
	}
	beforeConfig, beforeErr := BPSStoredResponsesConfigForAccount(before)
	afterConfig, afterErr := BPSStoredResponsesConfigForAccount(parent.Account)
	if beforeErr != nil || afterErr != nil || beforeConfig != afterConfig ||
		before.GetCredential("api_key") != parent.Account.GetCredential("api_key") ||
		before.GetCredential("base_url") != parent.Account.GetCredential("base_url") {
		return ErrBPSRequestAuthorizationRevoked
	}
	for _, model := range []string{publicModel, forwardModel} {
		for _, compact := range []bool{false, true} {
			oldBilling, oldUpstream := resolveOpenAIForwardMappedModels(before, model, compact)
			newBilling, newUpstream := resolveOpenAIForwardMappedModels(parent.Account, model, compact)
			if oldBilling != newBilling || oldUpstream != newUpstream {
				return ErrBPSRequestAuthorizationRevoked
			}
		}
	}
	return ctx.Err()
}

func storedNativeParentRow15(record *StoredResponse, owner StoredResponseOwner, id, model string) error {
	if record == nil || !previousResponseID15(id) || record.PublicID != id || record.UserID != owner.UserID || record.APIKeyID != owner.APIKeyID || record.GroupID != owner.GroupID || record.DeletedAt != nil || !record.ExpiresAt.After(time.Now()) {
		return ErrStoredResponseNotFound
	}
	if record.AccountID <= 0 || record.Model != model || record.ResolvedModel == "" {
		return ErrBPSRequestAuthorizationRevoked
	}
	if record.State != "completed" || record.ExecutionState != "terminal" || record.RecoveryVersion != 2 {
		return ErrStoredResponseUnsupported
	}
	return nil
}

func storedNativeParentLookupError15(ctx context.Context, err error) error {
	if cancelled := ctx.Err(); cancelled != nil {
		return cancelled
	}
	if errors.Is(err, ErrStoredResponseNotFound) || errors.Is(err, ErrStoredResponsesDisabled) {
		return err
	}
	return ErrBPSRequestAuthorizationUnavailable
}

func storedNativeParentAuthorityError15(ctx context.Context, err error) error {
	if cancelled := ctx.Err(); cancelled != nil {
		return cancelled
	}
	if errors.Is(err, ErrBPSRequestAuthorizationUnavailable) {
		return ErrBPSRequestAuthorizationUnavailable
	}
	return ErrBPSRequestAuthorizationRevoked
}

// Account repository values may be shared snapshots. Detach the data in memory
// before an authorization wait so in-place credential map updates cannot mutate
// both sides of a fence. Reverse relation edges are not request authority and
// are removed before copying to avoid serializing cyclic account/group graphs.
// No account bytes are persisted, logged or returned to a public client here.
func cloneStoredNativeParentAccount15(account *Account) (*Account, error) {
	if account == nil {
		return nil, ErrBPSRequestAuthorizationRevoked
	}
	flat := *account
	flat.Groups = make([]*Group, len(account.Groups))
	groupCopy := func(group *Group) *Group {
		if group == nil {
			return nil
		}
		copy := *group
		copy.AccountGroups = nil
		return &copy
	}
	for index, group := range account.Groups {
		flat.Groups[index] = groupCopy(group)
	}
	flat.AccountGroups = append([]AccountGroup(nil), account.AccountGroups...)
	for index := range flat.AccountGroups {
		flat.AccountGroups[index].Account = nil
		flat.AccountGroups[index].Group = groupCopy(flat.AccountGroups[index].Group)
	}
	raw, err := json.Marshal(&flat)
	if err != nil {
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	var copied Account
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&copied) != nil {
		return nil, ErrBPSRequestAuthorizationUnavailable
	}
	return &copied, nil
}
