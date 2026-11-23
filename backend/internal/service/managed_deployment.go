package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strings"
)

const ManagedDeploymentContract = "tosub2-managed-deployment/v1"
const ManagedDeploymentAccountExtraKey = "managed_deployment_binding"

var ErrManagedDeploymentInvalid = infraerrors.New(http.StatusBadRequest, "MANAGED_DEPLOYMENT_INVALID", "invalid managed deployment or session")
var ErrManagedDeploymentConflict = infraerrors.New(http.StatusConflict, "MANAGED_DEPLOYMENT_CONFLICT", "deployment revision or immutable binding changed")
var ErrManagedDeploymentUnavailable = infraerrors.New(http.StatusServiceUnavailable, "MANAGED_DEPLOYMENT_UNAVAILABLE", "selected deployment account is unavailable; restore its authorization or scheduling explicitly")
var ErrManagedDeploymentContext = infraerrors.New(http.StatusConflict, "MANAGED_DEPLOYMENT_CONTEXT_REQUIRED", "a trusted session or resource owner is required; rebuild context with a new explicit session")

type ManagedDeploymentBinding struct {
	Provider      string `json:"provider"`
	AccountID     int64  `json:"account_id"`
	BackendID     string `json:"backend_id"`
	BindingRef    string `json:"binding_ref"`
	ProfileRef    string `json:"profile_ref"`
	IdentityEpoch string `json:"identity_epoch"`
	WorkspaceID   string `json:"workspace_id"`
	Subject       string `json:"subject"`
}
type ManagedDeployment struct {
	Contract         string                   `json:"contract"`
	DeploymentID     string                   `json:"deployment_id"`
	GroupID          int64                    `json:"group_id"`
	RouteRevision    int64                    `json:"route_revision"`
	SelectedProvider string                   `json:"selected_provider"`
	BPS              ManagedDeploymentBinding `json:"bps"`
	Codex            ManagedDeploymentBinding `json:"codex"`
}
type ManagedDeploymentUpdate struct {
	ManagedDeployment
	ExpectedRouteRevision int64 `json:"expected_route_revision"`
}
type ManagedDeploymentSession struct {
	DeploymentID  string                   `json:"deployment_id"`
	GroupID       int64                    `json:"group_id"`
	UserID        int64                    `json:"user_id"`
	APIKeyID      int64                    `json:"api_key_id"`
	SessionKey    string                   `json:"-"`
	RouteRevision int64                    `json:"route_revision"`
	Binding       ManagedDeploymentBinding `json:"binding"`
}

// Optional narrow contract: unrelated account repositories need no deployment support.
type ManagedDeploymentRepository interface {
	GetManagedDeployment(context.Context, string) (*ManagedDeployment, error)
	GetManagedDeploymentForGroup(context.Context, int64) (*ManagedDeployment, error)
	PutManagedDeployment(context.Context, *ManagedDeploymentUpdate) (*ManagedDeployment, error)
	BindManagedDeploymentSession(context.Context, int64, int64, int64, string, bool) (*ManagedDeploymentSession, error)
	BindManagedDeploymentResource(context.Context, *ManagedDeploymentSession, string, string) error
	CheckManagedDeploymentResource(context.Context, *ManagedDeploymentSession, string, string) (bool, error)
	ResolveManagedDeploymentResource(context.Context, int64, int64, int64, string, string) (*ManagedDeploymentSession, error)
}
type ManagedDeploymentAdminService interface {
	GetManagedDeployment(context.Context, string) (*ManagedDeployment, error)
	PutManagedDeployment(context.Context, *ManagedDeploymentUpdate) (*ManagedDeployment, error)
}
type managedDeploymentSessionKey struct{}

func WithManagedDeploymentSession(ctx context.Context, s *ManagedDeploymentSession) context.Context {
	return context.WithValue(ctx, managedDeploymentSessionKey{}, s)
}
func ManagedDeploymentSessionFromContext(ctx context.Context) (*ManagedDeploymentSession, bool) {
	s, ok := ctx.Value(managedDeploymentSessionKey{}).(*ManagedDeploymentSession)
	return s, ok && s != nil
}
func ManagedDeploymentOwnerHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (b ManagedDeploymentBinding) valid(provider string) bool {
	if (provider != "bps_excel" && provider != "codex") || b.Provider != provider || b.AccountID <= 0 {
		return false
	}
	for _, p := range []struct{ value, prefix string }{{b.BindingRef, "bind_"}, {b.ProfileRef, "oauth_"}, {b.IdentityEpoch, ""}} {
		value := strings.TrimPrefix(p.value, p.prefix)
		u, err := uuid.Parse(value)
		if err != nil || u == uuid.Nil || u.String() != value || (value == p.value && p.prefix != "") {
			return false
		}
	}
	for _, v := range []string{b.BackendID, b.Subject, b.WorkspaceID} {
		if len(v) == 0 || len(v) > 1024 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\r\n\x00") {
			return false
		}
	}
	return true
}
func (b ManagedDeploymentBinding) Validate() error {
	if !b.valid(b.Provider) {
		return ErrManagedDeploymentInvalid
	}
	return nil
}
func (d ManagedDeployment) Validate() error {
	id := strings.TrimPrefix(d.DeploymentID, "deployment_")
	u, err := uuid.Parse(id)
	if d.Contract != ManagedDeploymentContract || id == d.DeploymentID || err != nil || u == uuid.Nil || u.String() != id || d.GroupID <= 0 || d.RouteRevision <= 0 || d.RouteRevision > 9007199254740991 || (d.SelectedProvider != "bps_excel" && d.SelectedProvider != "codex") || !d.BPS.valid("bps_excel") || !d.Codex.valid("codex") || d.BPS.AccountID == d.Codex.AccountID {
		return ErrManagedDeploymentInvalid
	}
	return nil
}
func managedDeploymentRepo(repo AccountRepository) (ManagedDeploymentRepository, error) {
	r, ok := repo.(ManagedDeploymentRepository)
	if !ok {
		return nil, ErrManagedDeploymentUnavailable
	}
	return r, nil
}
func (s *adminServiceImpl) GetManagedDeployment(ctx context.Context, id string) (*ManagedDeployment, error) {
	r, e := managedDeploymentRepo(s.accountRepo)
	if e != nil {
		return nil, e
	}
	return r.GetManagedDeployment(ctx, id)
}
func (s *adminServiceImpl) PutManagedDeployment(ctx context.Context, in *ManagedDeploymentUpdate) (*ManagedDeployment, error) {
	if in == nil || in.Validate() != nil || in.ExpectedRouteRevision < 0 || in.RouteRevision != in.ExpectedRouteRevision+1 {
		return nil, ErrManagedDeploymentInvalid
	}
	for _, b := range []ManagedDeploymentBinding{in.BPS, in.Codex} {
		a, e := s.accountRepo.GetByID(ctx, b.AccountID)
		if e != nil || a == nil || a.Platform != PlatformOpenAI {
			return nil, ErrManagedDeploymentInvalid
		}
		if e = ValidateManagedDeploymentAccount(a, b); e != nil {
			return nil, e
		}
	}
	r, e := managedDeploymentRepo(s.accountRepo)
	if e != nil {
		return nil, e
	}
	return r.PutManagedDeployment(ctx, in)
}
func ValidateManagedDeploymentAccount(a *Account, b ManagedDeploymentBinding) error {
	if a == nil || a.ID != b.AccountID || a.Platform != PlatformOpenAI {
		return ErrManagedDeploymentUnavailable
	}
	if CoreOAuthDeclared(a) {
		marker, e := CoreOAuthAccountBindingFromAccount(a)
		actual, bindingErr := coreOAuthRuntimeBinding(a)
		if e != nil || bindingErr != nil || actual != b || marker.Provider != b.Provider || "oauth_"+marker.ProfileRef != b.ProfileRef || marker.Subject != b.Subject || marker.WorkspaceID != b.WorkspaceID || marker.IdentityEpoch != b.IdentityEpoch {
			return ErrManagedDeploymentConflict
		}
		return nil
	}
	if b.Provider == "codex" {
		st, e := managedOAuthStateFromAccount(a)
		if e != nil || st.BindingRef != b.BindingRef || st.ProfileRef != b.ProfileRef || st.IdentityEpoch != b.IdentityEpoch || st.WorkspaceID != b.WorkspaceID || st.Subject != b.Subject || st.BackendID != b.BackendID {
			return ErrManagedDeploymentConflict
		}
		return nil
	}
	if b.Provider != "bps_excel" || a.Type != AccountTypeAPIKey {
		return ErrManagedDeploymentInvalid
	}
	raw, e := json.Marshal(a.Extra[ManagedDeploymentAccountExtraKey])
	if e != nil {
		return ErrManagedDeploymentConflict
	}
	var actual ManagedDeploymentBinding
	if json.Unmarshal(raw, &actual) != nil {
		return ErrManagedDeploymentConflict
	}
	actual.AccountID = a.ID
	if actual != b {
		return ErrManagedDeploymentConflict
	}
	return nil
}

// ManagedDeploymentResourceRef names an upstream-owned object, never a client asserted owner.
type ManagedDeploymentResourceRef struct {
	Kind string
	ID   string
}

func (s *OpenAIGatewayService) ManagedDeploymentForGroup(ctx context.Context, groupID *int64) (*ManagedDeployment, error) {
	if s == nil || groupID == nil || *groupID <= 0 {
		return nil, nil
	}
	r, ok := s.accountRepo.(ManagedDeploymentRepository)
	if !ok {
		return nil, nil
	}
	return r.GetManagedDeploymentForGroup(ctx, *groupID)
}
func (s *OpenAIGatewayService) ResolveManagedDeploymentResource(ctx context.Context, group, user, key int64, kind, id string) (*ManagedDeploymentSession, error) {
	if group <= 0 || user <= 0 || key <= 0 || !validManagedDeploymentResource(kind, id) {
		return nil, ErrManagedDeploymentInvalid
	}
	r, e := managedDeploymentRepo(s.accountRepo)
	if e != nil {
		return nil, e
	}
	return r.ResolveManagedDeploymentResource(ctx, group, user, key, kind, id)
}
func validManagedDeploymentResource(kind, id string) bool {
	return kind != "" && len(kind) <= 64 && id != "" && len(id) <= 1048576 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\r\n\x00")
}
func (s *OpenAIGatewayService) RecordManagedDeploymentResource(ctx context.Context, session *ManagedDeploymentSession, kind, id string) error {
	if session == nil || !validManagedDeploymentResource(kind, id) {
		return ErrManagedDeploymentInvalid
	}
	r, e := managedDeploymentRepo(s.accountRepo)
	if e != nil {
		return e
	}
	return r.BindManagedDeploymentResource(ctx, session, kind, id)
}

// BindManagedDeploymentRequest resolves only authenticated durable ownership. A
// fresh explicit session snapshots the current selection exactly once. Resource
// continuations without a session recover their original binding, never today's
// selection. Every reference must agree on the full immutable identity.
func (s *OpenAIGatewayService) BindManagedDeploymentRequest(ctx context.Context, group, user, key int64, explicit string, refs []ManagedDeploymentResourceRef) (*ManagedDeploymentSession, error) {
	if group <= 0 || user <= 0 || key <= 0 || len(explicit) > 1024 || strings.TrimSpace(explicit) != explicit || strings.ContainsAny(explicit, "\r\n\x00") {
		return nil, ErrManagedDeploymentInvalid
	}
	r, e := managedDeploymentRepo(s.accountRepo)
	if e != nil {
		return nil, e
	}
	var owner *ManagedDeploymentSession
	for _, ref := range refs {
		if !validManagedDeploymentResource(ref.Kind, ref.ID) {
			return nil, ErrManagedDeploymentInvalid
		}
		o, err := r.ResolveManagedDeploymentResource(ctx, group, user, key, ref.Kind, ref.ID)
		if err != nil {
			return nil, err
		}
		if o == nil {
			return nil, ErrManagedDeploymentContext
		}
		if owner != nil && (owner.DeploymentID != o.DeploymentID || owner.Binding != o.Binding) {
			return nil, ErrManagedDeploymentContext
		}
		owner = o
	}
	var session *ManagedDeploymentSession
	if explicit != "" {
		session, e = r.BindManagedDeploymentSession(ctx, group, user, key, ManagedDeploymentOwnerHash(explicit), false)
	} else if owner != nil {
		session = owner
	} else {
		return nil, ErrManagedDeploymentContext
	}
	if e != nil {
		return nil, e
	}
	if session == nil || session.GroupID != group || session.UserID != user || session.APIKeyID != key || !session.Binding.valid(session.Binding.Provider) {
		return nil, ErrManagedDeploymentConflict
	}
	if owner != nil && (session.DeploymentID != owner.DeploymentID || session.Binding != owner.Binding) {
		return nil, ErrManagedDeploymentContext
	}
	return session, nil
}

// ManagedDeploymentPayloadResources extracts only protocol fields carrying
// upstream state. Opaque encrypted reasoning is scoped as a hashed resource too.
func ManagedDeploymentPayloadResources(raw []byte, output bool) ([]ManagedDeploymentResourceRef, error) {
	if !managedDeploymentUniqueJSON(raw) {
		return nil, ErrManagedDeploymentInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, ErrManagedDeploymentInvalid
	}
	refs := []ManagedDeploymentResourceRef{}
	seen := map[string]bool{}
	add := func(kind, id string) {
		if id != "" && !seen[kind+"\x00"+id] {
			seen[kind+"\x00"+id] = true
			refs = append(refs, ManagedDeploymentResourceRef{kind, id})
		}
	}
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		if depth > 64 {
			return ErrManagedDeploymentInvalid
		}
		switch x := v.(type) {
		case []any:
			for _, v := range x {
				if e := walk(v, depth+1); e != nil {
					return e
				}
			}
		case map[string]any:
			for k, v := range x {
				str, _ := v.(string)
				switch k {
				case "previous_response_id":
					if !output {
						add("response", str)
					}
				case "file_id":
					add("file", str)
				case "container_id":
					add("container", str)
				case "vector_store_id":
					add("vector_store", str)
				case "encrypted_content":
					add("encrypted_reasoning", str)
				case "signature":
					if x["type"] == "thinking" || x["type"] == "signature_delta" {
						signature := strings.TrimSpace(str)
						if signature != "" && !strings.HasPrefix(signature, "gAAAA") {
							add("encrypted_reasoning", signature)
						}
					}
				case "call_id", "tool_call_id", "tool_use_id":
					add("tool_call", str)
				case "id":
					if output {
						switch {
						case x["type"] == "tool_use" || (x["type"] == "function" && x["function"] != nil) || strings.HasPrefix(str, "call_"):
							add("tool_call", str)
						case strings.HasPrefix(str, "resp_"):
							add("response", str)
						case strings.HasPrefix(str, "file-") || strings.HasPrefix(str, "file_"):
							add("file", str)
						case strings.HasPrefix(str, "cntr_"):
							add("container", str)
						case strings.HasPrefix(str, "vs_"):
							add("vector_store", str)
						default:
							add("item", str)
						}
					} else if x["type"] == "tool_use" || (x["type"] == "function" && x["function"] != nil) {
						add("tool_call", str)
					} else if x["type"] == "item_reference" {
						add("item", str)
					}
				}
				if k == "vector_store_ids" {
					if ids, ok := v.([]any); ok {
						for _, id := range ids {
							if id, ok := id.(string); ok {
								add("vector_store", id)
							}
						}
					}
				}
				// Follow protocol-owned structure only. In particular metadata,
				// JSON schemas and function arguments/results are client content;
				// an upstream echo must never mint resource ownership for them.
				protocolChild := false
				switch k {
				case "response", "item", "output", "input", "content", "data", "messages", "tools", "annotations", "results", "container", "attachments", "tool_calls", "function", "summary", "file", "image_file", "content_block", "choices", "message", "delta":
					protocolChild = true
				}
				if k == "input" && x["type"] == "tool_use" {
					protocolChild = false
				}
				if k == "output" && (x["type"] == "function_call_output" || x["type"] == "custom_tool_call_output") {
					protocolChild = false
				}
				if protocolChild {
					if e := walk(v, depth+1); e != nil {
						return e
					}
				}
			}
		}
		return nil
	}
	if e := walk(v, 0); e != nil {
		return nil, e
	}
	if len(refs) > 1024 {
		return nil, ErrManagedDeploymentInvalid
	}
	return refs, nil
}
func (s *OpenAIGatewayService) RecordManagedDeploymentPayload(ctx context.Context, raw []byte) error {
	session, ok := ManagedDeploymentSessionFromContext(ctx)
	if !ok {
		return nil
	}
	refs, e := ManagedDeploymentPayloadResources(raw, true)
	if e != nil {
		return e
	}
	for _, ref := range refs {
		if e = s.RecordManagedDeploymentResource(ctx, session, ref.Kind, ref.ID); e != nil {
			return e
		}
	}
	return nil
}
func (s *OpenAIGatewayService) ValidateManagedDeploymentPayload(ctx context.Context, raw []byte) error {
	session, ok := ManagedDeploymentSessionFromContext(ctx)
	if !ok {
		return nil
	}
	refs, e := ManagedDeploymentPayloadResources(raw, false)
	if e != nil {
		return e
	}
	for _, ref := range refs {
		owner, e := s.ResolveManagedDeploymentResource(ctx, session.GroupID, session.UserID, session.APIKeyID, ref.Kind, ref.ID)
		if e != nil {
			return e
		}
		if owner.DeploymentID != session.DeploymentID || owner.Binding != session.Binding {
			return ErrManagedDeploymentContext
		}
	}
	return nil
}

// GET/list responses must not mint owners for shared upstream storage.
func (s *OpenAIGatewayService) ValidateManagedDeploymentOutputPayload(ctx context.Context, raw []byte) error {
	session, ok := ManagedDeploymentSessionFromContext(ctx)
	if !ok {
		return nil
	}
	refs, e := ManagedDeploymentPayloadResources(raw, true)
	if e != nil {
		return e
	}
	for _, ref := range refs {
		if storedResponseInputProjectionOwns(ctx, session, ref) {
			continue
		}
		owner, e := s.ResolveManagedDeploymentResource(ctx, session.GroupID, session.UserID, session.APIKeyID, ref.Kind, ref.ID)
		if e != nil {
			return e
		}
		if owner.DeploymentID != session.DeploymentID || owner.Binding != session.Binding {
			return ErrManagedDeploymentContext
		}
	}
	return nil
}

// selectManagedDeploymentAccount bypasses scoring, never policy or concurrency.
// Always re-read the account; an administrator disabling it is an explicit stop,
// not an instruction to drain onto another backend.
func (s *OpenAIGatewayService) selectManagedDeploymentAccount(ctx context.Context, session *ManagedDeploymentSession, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	decision := OpenAIAccountScheduleDecision{Layer: "managed_deployment", CandidateCount: 1, SelectedAccountID: session.Binding.AccountID}
	if s.checkChannelPricingRestriction(ctx, req.GroupID, req.RequestedModel) {
		return nil, decision, ErrManagedDeploymentUnavailable
	}
	if req.GroupID == nil || *req.GroupID != session.GroupID {
		return nil, decision, ErrManagedDeploymentConflict
	}
	if _, excluded := req.ExcludedIDs[session.Binding.AccountID]; excluded {
		return nil, decision, ErrManagedDeploymentUnavailable
	}
	a, e := s.accountRepo.GetByID(ctx, session.Binding.AccountID)
	if e != nil {
		return nil, decision, e
	}
	if e = ValidateManagedDeploymentAccount(a, session.Binding); e != nil {
		return nil, decision, e
	}
	if !a.IsSchedulable() || !openAIStickyAccountMatchesGroup(a, req.GroupID) || a.Platform != req.Platform || a.ParentAccountID != nil || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return nil, decision, ErrManagedDeploymentUnavailable
	}
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	if compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, a, req); !compatible || !scheduler.isAccountTransportCompatible(a, req.RequiredTransport) || (req.RequireCompact && openAICompactSupportTier(a) == 0) {
		return nil, decision, ErrManagedDeploymentUnavailable
	}
	acquired, e := s.tryAcquireAccountSlot(ctx, a.ID, a.Concurrency)
	if e != nil {
		return nil, decision, e
	}
	decision.SelectedAccountType = a.Type
	if acquired != nil && acquired.Acquired {
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: a, Acquired: true, ReleaseFunc: acquired.ReleaseFunc}), decision, nil
	}
	cfg := s.schedulingConfig()
	return attachSelectionProfitGate(ctx, &AccountSelectionResult{Account: a, WaitPlan: &AccountWaitPlan{AccountID: a.ID, MaxConcurrency: a.Concurrency, Timeout: cfg.StickySessionWaitTimeout, MaxWaiting: cfg.StickySessionMaxWaiting}}), decision, nil
}

func managedDeploymentUniqueJSON(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		t, e := dec.Token()
		if e != nil {
			return false
		}
		d, ok := t.(json.Delim)
		if !ok {
			return true
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				v, ok := k.(string)
				if e != nil || !ok || seen[v] {
					return false
				}
				seen[v] = true
				if !walk(depth + 1) {
					return false
				}
			}
			t, e = dec.Token()
			return e == nil && t == json.Delim('}')
		case '[':
			for dec.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			t, e = dec.Token()
			return e == nil && t == json.Delim(']')
		default:
			return false
		}
	}
	return len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' && walk(0) && dec.Decode(new(any)) == io.EOF
}

// ScopeManagedDeploymentAffinity isolates actual upstream wire state. Call this
// at final HTTP/WS serialization too; a scoped database row alone cannot isolate
// a shared upstream account. UUID output preserves official session wire shape.
func ScopeManagedDeploymentAffinity(ctx context.Context, value string) (string, bool) {
	session, ok := ManagedDeploymentSessionFromContext(ctx)
	if !ok {
		return value, false
	}
	scope, _ := json.Marshal(struct {
		Contract, Deployment, SessionKey string
		Group, User, Key                 int64
		Binding                          ManagedDeploymentBinding
		Value                            string
	}{Contract: ManagedDeploymentContract, Deployment: session.DeploymentID, SessionKey: session.SessionKey, Group: session.GroupID, User: session.UserID, Key: session.APIKeyID, Binding: session.Binding, Value: value})
	return uuid.NewSHA1(uuid.NameSpaceURL, scope).String(), true
}
