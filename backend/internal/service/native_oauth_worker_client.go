package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

const coreWorkerContract = "tosub2-managed-oauth/v1"
const coreWorkerMaxBody = 1 << 20
const coreWorkerEventLimit = 128

type coreOAuthWorkerState struct {
	Contract             string    `json:"contract"`
	BackendID            string    `json:"backend_id"`
	BindingRef           string    `json:"binding_ref"`
	ProfileRef           string    `json:"profile_ref"`
	Provider             string    `json:"provider"`
	Subject              string    `json:"subject"`
	WorkspaceID          string    `json:"workspace_id"`
	IdentityEpoch        string    `json:"identity_epoch"`
	TokenRevision        int64     `json:"token_revision"`
	Initialized          bool      `json:"initialized"`
	CredentialGeneration string    `json:"credential_generation"`
	ExpiresAt            time.Time `json:"expires_at"`
	LastEventID          uint64    `json:"last_event_id"`
	LastOperationID      string    `json:"last_operation_id"`
}
type coreWorkerEvent struct {
	EventID             uint64    `json:"event_id"`
	Origin              string    `json:"origin"`
	FailedTokenRevision int64     `json:"failed_token_revision"`
	IdentityEpoch       string    `json:"identity_epoch"`
	ProfileRef          string    `json:"profile_ref"`
	BindingRef          string    `json:"binding_ref"`
	BackendID           string    `json:"backend_id"`
	CreatedAt           time.Time `json:"created_at"`
}
type coreWorkerEvents struct {
	Events        []coreWorkerEvent `json:"events"`
	NextCursor    uint64            `json:"next_cursor"`
	OldestEventID uint64            `json:"oldest_event_id"`
	CursorGap     bool              `json:"cursor_gap"`
}

func coreWorkerURL(s *OpenAIGatewayService, a *Account, explicitURL, key string) (string, string, error) {
	if a == nil || s == nil || a.ID <= 0 || a.Concurrency <= 0 || a.Platform != PlatformOpenAI || a.Type != AccountTypeAPIKey {
		return "", "", ErrCoreOAuthInvalid
	}
	if explicitURL == "" {
		explicitURL = a.GetCredential("core_oauth_control_url")
	}
	if key == "" {
		key = a.GetCredential("core_oauth_control_key")
	}
	if len(key) < 32 || len(key) > 4096 || !coreOAuthSecretText(key) || key == a.GetCredential("api_key") || key == a.GetCredential("bps_files_service_key") {
		return "", "", ErrCoreOAuthInvalid
	}
	raw, err := url.Parse(explicitURL)
	if err != nil || raw.Hostname() == "" || raw.User != nil || raw.RawQuery != "" || raw.ForceQuery || strings.Contains(explicitURL, "#") || raw.RawPath != "" || strings.Contains(raw.Path, "\\") {
		return "", "", ErrCoreOAuthInvalid
	}
	normalized, err := validateOutboundURLWithConfig(s.cfg, explicitURL)
	if err != nil {
		return "", "", ErrCoreOAuthInvalid
	}
	u, err := url.Parse(normalized)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Scheme != "http" && u.Scheme != "https" {
		return "", "", ErrCoreOAuthInvalid
	}
	base, err := url.Parse(strings.TrimRight(a.GetCredential("base_url"), "/"))
	if err != nil || base.Scheme != u.Scheme || !strings.EqualFold(base.Host, u.Host) {
		return "", "", ErrCoreOAuthInvalid
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(u.Path, "/v1") {
		u.Path = strings.TrimSuffix(u.Path, "/v1")
	}
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return "", "", ErrCoreOAuthInvalid
	}
	return strings.TrimRight(u.String(), "/"), key, nil
}
func (s *OpenAIGatewayService) configureCoreOAuthWorkerAccount(a *Account, controlURL, controlKey string) (*Account, error) {
	u, k, err := coreWorkerURL(s, a, controlURL, controlKey)
	if err != nil {
		return nil, ErrCoreOAuthInvalid
	}
	clone := *a
	clone.Credentials = coreOAuthCopyMap(a.Credentials)
	clone.Credentials["core_oauth_control_url"] = u
	clone.Credentials["core_oauth_control_key"] = k
	return &clone, nil
}
func (s *OpenAIGatewayService) coreWorkerRequest(ctx context.Context, a *Account, method, endpoint string, body, out any) error {
	baseURL, key, err := coreWorkerURL(s, a, "", "")
	if err != nil {
		return err
	}
	if s.httpUpstream == nil || ctx.Err() != nil || a.ProxyID != nil && a.Proxy == nil {
		return ErrCoreOAuthUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u := baseURL + endpoint
	var r io.Reader
	if body != nil {
		raw, e := json.Marshal(body)
		if e != nil {
			return ErrCoreOAuthUnavailable
		}
		r = bytes.NewReader(raw)
	}
	req, e := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), method, u, r)
	if e != nil {
		return ErrCoreOAuthUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.GetBody = nil
	proxyURL := ""
	if a.Proxy != nil {
		proxyURL = a.Proxy.URL()
	}
	resp, e := s.httpUpstream.Do(req, proxyURL, a.ID, a.Concurrency)
	if e != nil || resp == nil || resp.Body == nil {
		return ErrCoreOAuthUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrCoreOAuthUnavailable
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, coreWorkerMaxBody+1))
	if e != nil || len(raw) > coreWorkerMaxBody {
		return ErrCoreOAuthUnavailable
	}
	parsed, e := managedfiles.Parse(raw)
	if e != nil {
		return ErrCoreOAuthUnavailable
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return ErrCoreOAuthUnavailable
	}
	switch out.(type) {
	case *coreOAuthWorkerState:
		if !coreWorkerShape(object, "contract backend_id binding_ref profile_ref provider subject workspace_id identity_epoch credential_generation expires_at last_operation_id", "token_revision last_event_id", "initialized", "") {
			return ErrCoreOAuthUnavailable
		}
	case *coreWorkerEvents:
		if !coreWorkerShape(object, "", "next_cursor oldest_event_id", "cursor_gap", "events") {
			return ErrCoreOAuthUnavailable
		}
		events := object["events"].([]any)
		if len(events) > coreWorkerEventLimit {
			return ErrCoreOAuthUnavailable
		}
		for _, value := range events {
			event, ok := value.(map[string]any)
			if !ok || !coreWorkerShape(event, "origin identity_epoch profile_ref binding_ref backend_id created_at", "event_id failed_token_revision", "", "") {
				return ErrCoreOAuthUnavailable
			}
		}
	default:
		return ErrCoreOAuthUnavailable
	}
	if json.Unmarshal(raw, out) != nil {
		return ErrCoreOAuthUnavailable
	}
	return nil
}

func coreWorkerShape(object map[string]any, strs, nums, bools, arrays string) bool {
	stringsFields, numericFields, booleanFields, arrayFields := strings.Fields(strs), strings.Fields(nums), strings.Fields(bools), strings.Fields(arrays)
	if len(object) != len(stringsFields)+len(numericFields)+len(booleanFields)+len(arrayFields) {
		return false
	}
	for _, key := range stringsFields {
		if _, ok := object[key].(string); !ok {
			return false
		}
	}
	for _, key := range numericFields {
		if _, ok := object[key].(json.Number); !ok {
			return false
		}
	}
	for _, key := range booleanFields {
		if _, ok := object[key].(bool); !ok {
			return false
		}
	}
	for _, key := range arrayFields {
		if _, ok := object[key].([]any); !ok {
			return false
		}
	}
	return true
}

func coreWorkerIdentityMatches(s coreOAuthWorkerState, b ManagedDeploymentBinding) bool {
	return s.Initialized && s.Contract == coreWorkerContract && s.BackendID == b.BackendID && s.BindingRef == b.BindingRef && s.ProfileRef == b.ProfileRef && s.Provider == b.Provider && s.Subject == b.Subject && s.WorkspaceID == b.WorkspaceID && s.IdentityEpoch == b.IdentityEpoch && s.CredentialGeneration == b.IdentityEpoch && s.TokenRevision > 0 && s.TokenRevision <= 9007199254740991 && !s.ExpiresAt.IsZero() && coreOAuthRef(s.LastOperationID)
}

func coreWorkerBindingValid(a *Account, b ManagedDeploymentBinding) bool {
	return a != nil && a.ID == b.AccountID && b.valid(CoreOAuthProviderExcel)
}

func (s *OpenAIGatewayService) publishCoreOAuthWorker(ctx context.Context, a *Account, b ManagedDeploymentBinding, p *CoreOAuthCredentialSnapshot) error {
	if !coreWorkerBindingValid(a, b) || p == nil || !coreOAuthBindingMatchesProfile(b, p.Profile) || p.Profile.TokenRevision > 9007199254740991 {
		return ErrCoreOAuthInvalid
	}
	if _, err := CoreOAuthAccountBindingForSnapshot(p); err != nil {
		return err
	}
	var state coreOAuthWorkerState
	if err := s.coreWorkerRequest(ctx, a, http.MethodGet, "/_control/v1/credentials", nil, &state); err != nil {
		return err
	}
	if !state.Initialized {
		if state.Contract != coreWorkerContract || state.BackendID != b.BackendID || state.Provider != b.Provider || state.TokenRevision != 0 || state.BindingRef != "" || state.ProfileRef != "" || state.Subject != "" || state.WorkspaceID != "" || state.IdentityEpoch != "" || state.CredentialGeneration != "" || !state.ExpiresAt.IsZero() || state.LastEventID != 0 || state.LastOperationID != "" {
			return ErrCoreOAuthConflict
		}
	} else if !coreWorkerIdentityMatches(state, b) {
		return ErrCoreOAuthConflict
	}
	if state.Initialized && state.TokenRevision > p.Profile.TokenRevision {
		return ErrCoreOAuthConflict
	}
	if state.Initialized && state.TokenRevision == p.Profile.TokenRevision && (!state.ExpiresAt.Equal(p.Profile.ExpiresAt.UTC()) || state.LastOperationID == "") {
		return ErrCoreOAuthConflict
	}
	if state.Initialized && state.TokenRevision == p.Profile.TokenRevision {
		return nil
	}
	op := coreOAuthRuntimeUUID("core-worker-operation/v1", b.BindingRef, strconv.FormatInt(p.Profile.TokenRevision, 10), p.Profile.ExpiresAt.UTC().Format(time.RFC3339Nano))
	payload := map[string]any{"contract": coreWorkerContract, "backend_id": b.BackendID, "binding_ref": b.BindingRef, "profile_ref": b.ProfileRef, "provider": b.Provider, "subject": b.Subject, "workspace_id": b.WorkspaceID, "identity_epoch": b.IdentityEpoch, "token_revision": p.Profile.TokenRevision, "expected_token_revision": state.TokenRevision, "operation_id": op, "access_token": p.AccessToken, "expires_at": p.Profile.ExpiresAt.UTC()}
	var receipt coreOAuthWorkerState
	if err := s.coreWorkerRequest(ctx, a, http.MethodPut, "/_control/v1/credentials", payload, &receipt); err != nil {
		return err
	}
	if !coreWorkerIdentityMatches(receipt, b) || receipt.TokenRevision != p.Profile.TokenRevision || !receipt.ExpiresAt.Equal(p.Profile.ExpiresAt.UTC()) || receipt.LastOperationID != op {
		return ErrCoreOAuthConflict
	}
	return nil
}
func (s *OpenAIGatewayService) coreOAuthWorkerUnauthorized(ctx context.Context, a *Account, b ManagedDeploymentBinding) ([]CoreOAuthUnauthorizedInput, error) {
	if !coreWorkerBindingValid(a, b) {
		return nil, ErrCoreOAuthInvalid
	}
	var page coreWorkerEvents
	if err := s.coreWorkerRequest(ctx, a, http.MethodGet, "/_control/v1/auth-events?after=0", nil, &page); err != nil {
		return nil, err
	}
	var state coreOAuthWorkerState
	if err := s.coreWorkerRequest(ctx, a, http.MethodGet, "/_control/v1/credentials", nil, &state); err != nil {
		return nil, err
	}
	if !coreWorkerIdentityMatches(state, b) || page.NextCursor != state.LastEventID {
		return nil, ErrCoreOAuthConflict
	}
	if page.NextCursor == 0 {
		if len(page.Events) != 0 || page.OldestEventID != 0 || page.CursorGap {
			return nil, ErrCoreOAuthConflict
		}
		return nil, nil
	}
	// The worker retains exactly the last 128 events, with contiguous IDs.
	count := min(page.NextCursor, coreWorkerEventLimit)
	oldest := page.NextCursor - count + 1
	if uint64(len(page.Events)) != count || page.OldestEventID != oldest || page.CursorGap != (oldest > 1) {
		return nil, ErrCoreOAuthConflict
	}
	profileRef, ok := strings.CutPrefix(b.ProfileRef, "oauth_")
	if !ok || !coreOAuthRef(profileRef) {
		return nil, ErrCoreOAuthConflict
	}
	out := make([]CoreOAuthUnauthorizedInput, 0, len(page.Events))
	var priorRevision int64
	for i, ev := range page.Events {
		if ev.EventID != oldest+uint64(i) || ev.Origin != "upstream_oauth" || ev.BackendID != b.BackendID || ev.BindingRef != b.BindingRef || ev.ProfileRef != b.ProfileRef || ev.IdentityEpoch != b.IdentityEpoch || ev.FailedTokenRevision <= priorRevision || ev.FailedTokenRevision > state.TokenRevision || ev.CreatedAt.IsZero() {
			return nil, ErrCoreOAuthConflict
		}
		priorRevision = ev.FailedTokenRevision
		out = append(out, CoreOAuthUnauthorizedInput{ProfileRef: profileRef, IdentityEpoch: ev.IdentityEpoch, FailedRevision: int64(ev.FailedTokenRevision), StatusCode: http.StatusUnauthorized, Origin: "upstream_oauth"})
	}
	return out, nil
}
