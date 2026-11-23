package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var errBPSSessionBinding = errors.New("BPS session binding is unavailable")

type bpsSessionBindingKey struct{}
type bpsSessionBindingState struct {
	caller                     context.Context
	accountID                  int64
	config                     BPSFilesBackendConfig
	owner, group, conversation string
	authorize                  func(context.Context) error
	negotiated                 atomic.Bool
}

func bpsSessionBindingFrom(ctx context.Context) *bpsSessionBindingState {
	state, _ := ctx.Value(bpsSessionBindingKey{}).(*bpsSessionBindingState)
	return state
}
func bpsSessionBindingConversation(ctx context.Context) string {
	if state := bpsSessionBindingFrom(ctx); state != nil {
		return state.conversation
	}
	return ""
}
func declaresBPSSessionBinding(account *Account) bool {
	if account == nil {
		return false
	}
	for name := range account.Credentials {
		if strings.HasPrefix(name, "bps_files_") {
			return true
		}
	}
	capabilities, _ := account.openAIEndpointCapabilitySet()
	return capabilities[string(OpenAIEndpointCapabilityBPSFiles)]
}

func bpsSessionConversation(c *gin.Context, body []byte) (owner, group, reference string, err error) {
	if c == nil {
		return "", "", "", errBPSSessionBinding
	}
	key := getAPIKeyFromContext(c)
	if key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 {
		return "", "", "", errBPSSessionBinding
	}
	if session, managed := ManagedDeploymentSessionFromContext(c.Request.Context()); managed {
		if session.UserID != key.UserID || session.APIKeyID != key.ID || session.GroupID != *key.GroupID || session.SessionKey == "" || session.Binding.Provider != "bps_excel" {
			return "", "", "", errBPSSessionBinding
		}
		encoded, _ := json.Marshal([]any{"bps-managed-conversation/1", session.DeploymentID, session.UserID, session.APIKeyID, session.GroupID, session.SessionKey, session.Binding})
		digest := sha256.Sum256(encoded)
		return strconv.FormatInt(key.UserID, 10), strconv.FormatInt(*key.GroupID, 10), "sha256:" + hex.EncodeToString(digest[:]), nil
	}
	conversation := explicitOpenAIRequestSessionID(c, body)
	if conversation == "" {
		conversation = deriveOpenAIContentSessionSeed(body)
	}
	if conversation == "" {
		return "", "", "", errBPSSessionBinding
	}
	encoded, _ := json.Marshal([]any{"bps-authenticated-conversation/1", key.UserID, key.ID, *key.GroupID, conversation})
	digest := sha256.Sum256(encoded)
	return strconv.FormatInt(key.UserID, 10), strconv.FormatInt(*key.GroupID, 10), "sha256:" + hex.EncodeToString(digest[:]), nil
}

// Bind the authenticated caller before any model/body transformation. Backend
// identity remains in the gateway's outer scope so the conversation ref also
// survives an explicitly authorized backend migration.
func (s *OpenAIGatewayService) prepareBPSSessionBinding(ctx context.Context, c *gin.Context, account *Account, body []byte) (context.Context, error) {
	if !declaresBPSSessionBinding(account) || StoredResponsePlanFromContext(ctx) != nil {
		return ctx, nil
	}
	if c == nil || c.Request == nil {
		return ctx, errBPSSessionBinding
	}
	key := getAPIKeyFromContext(c)
	if key == nil || key.ID <= 0 || key.UserID <= 0 || key.GroupID == nil || *key.GroupID <= 0 {
		return ctx, errBPSSessionBinding
	}
	config, err := BPSFilesConfigForAccount(account)
	if err != nil {
		return ctx, errBPSSessionBinding
	}
	groupID := *key.GroupID
	owner, group, conversation, err := bpsSessionConversation(c, body)
	if err != nil {
		return ctx, err
	}
	model := gjson.GetBytes(body, "model").String()
	if public, ok := RequestedPublicModelFromContext(ctx); ok {
		model = public
	} else if public, ok := ctx.Value(ctxkey.Model).(string); ok {
		model = public
	}
	state := &bpsSessionBindingState{caller: c.Request.Context(), accountID: account.ID, config: config,
		owner: owner, group: group, conversation: conversation}
	state.authorize = func(check context.Context) error {
		if err := state.caller.Err(); err != nil {
			return err
		}
		if err := CheckBPSRequestAuthorization(check, model); err != nil {
			return err
		}
		current, err := s.AuthorizeManagedFileAccount(check, groupID, model, account)
		if err != nil {
			return err
		}
		observed, err := BPSFilesConfigForAccount(current)
		if err != nil || observed != state.config {
			return errBPSSessionBinding
		}
		return state.caller.Err()
	}
	bound := context.WithValue(ctx, bpsSessionBindingKey{}, state)
	if err := state.authorize(bound); err != nil {
		return ctx, err
	}
	// File preparation performs the one target Describe immediately before its
	// admission. Ordinary inference negotiates here before any model request.
	if ManagedFilePlanFromContext(ctx) == nil {
		backend, err := NewBPSFilesBackend(config, s.httpUpstream, s.cfg)
		if err != nil {
			return ctx, errBPSSessionBinding
		}
		if _, err = describeBPSSessionBinding(bound, backend); err != nil {
			return ctx, err
		}
	}
	return bound, nil
}

func describeBPSSessionBinding(ctx context.Context, backend *BPSFilesBackend) (managedfiles.DescribeResult, error) {
	if backend == nil {
		return managedfiles.DescribeResult{}, errBPSSessionBinding
	}
	state := bpsSessionBindingFrom(ctx)
	if state == nil {
		return backend.Describe(ctx)
	}
	if backend == nil || backend.config != state.config || state.authorize == nil {
		return managedfiles.DescribeResult{}, errBPSSessionBinding
	}
	if err := state.authorize(ctx); err != nil {
		return managedfiles.DescribeResult{}, err
	}
	description, err := backend.DescribeSessionBinding(ctx)
	if err != nil {
		return managedfiles.DescribeResult{}, err
	}
	if err = state.authorize(ctx); err != nil {
		return managedfiles.DescribeResult{}, err
	}
	state.negotiated.Store(true)
	return description, nil
}

// Header maps can retain arbitrary wire casing. Remove all forms, including
// values injected by caller header forwarding or account overrides.
func stripBPSSessionBindingHeaders(headers http.Header) {
	for name := range headers {
		if strings.EqualFold(name, managedfiles.SessionBindingHeader) || strings.EqualFold(name, managedfiles.SessionBindingFeatureHeader) {
			delete(headers, name)
		}
	}
}

// Called after recording and every body/header compatibility transformation,
// immediately before the actual ordinary HTTP transport. Existing private
// admissions carry their bound conversation in the manifest instead.
func prepareBPSSessionBindingSend(request *http.Request, account *Account) error {
	if request == nil || request.URL == nil {
		return errBPSSessionBinding
	}
	stripBPSSessionBindingHeaders(request.Header)
	state := bpsSessionBindingFrom(request.Context())
	if state == nil {
		if declaresBPSSessionBinding(account) && !isStoredResponseGeneration(request.Context()) {
			return errBPSSessionBinding
		}
		return nil
	}
	if account == nil || account.ID != state.accountID || !state.negotiated.Load() || state.authorize == nil {
		return errBPSSessionBinding
	}
	if err := state.authorize(request.Context()); err != nil {
		return err
	}
	current, err := BPSFilesConfigForAccount(account)
	if err != nil || current != state.config {
		return errBPSSessionBinding
	}
	if IsManagedFileGeneration(request.Context()) {
		return nil
	}
	if isStoredResponseGeneration(request.Context()) {
		return errBPSSessionBinding
	}
	for name := range request.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-bps-") {
			return errBPSSessionBinding
		}
	}
	if request.Method != http.MethodPost || (request.URL.Path != "/v1/responses" && request.URL.Path != "/responses") || request.GetBody == nil || request.ContentLength <= 0 {
		return errBPSSessionBinding
	}
	reader, err := request.GetBody()
	if err != nil {
		return errBPSSessionBinding
	}
	wireDigest := sha256.New()
	size, readErr := io.Copy(wireDigest, io.LimitReader(reader, request.ContentLength+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || size != request.ContentLength {
		return errBPSSessionBinding
	}
	if err := state.authorize(request.Context()); err != nil {
		return err
	}
	claims := managedfiles.SessionBindingClaims{Version: managedfiles.SessionBindingContract, OwnerRef: state.owner, GroupRef: state.group,
		ConversationRef: state.conversation, Expected: state.config.Expected, Method: request.Method, Path: request.URL.Path,
		WireSHA256: hex.EncodeToString(wireDigest.Sum(nil)), ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
	proof, err := managedfiles.SignSessionBinding(state.config.ServiceKey, claims)
	if err != nil {
		return errBPSSessionBinding
	}
	request.Header.Set(managedfiles.SessionBindingHeader, proof)
	request.GetBody = nil
	*request = *request.WithContext(WithHTTPUpstreamRedirectsDisabled(request.Context()))
	return nil
}
