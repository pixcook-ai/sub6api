package admin

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const oauthControllerTestKey = "controller-test-secret-0123456789-abcdefghijklmnop"
const oauthControllerTestConfig = `[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"]}]`

type oauthControllerAdminStub struct {
	service.AdminService
	state                   *service.ManagedOAuthPublicState
	stateErr                error
	reads, installs, events int
	last                    *service.ManagedOAuthInstallRequest
}

func (s *oauthControllerAdminStub) GetManagedOAuthState(context.Context, int64) (*service.ManagedOAuthPublicState, error) {
	s.reads++
	return s.state, s.stateErr
}
func (s *oauthControllerAdminStub) InstallManagedOAuthCredentials(_ context.Context, _ int64, input *service.ManagedOAuthInstallRequest) (*service.ManagedOAuthPublicState, error) {
	s.installs++
	s.last = input
	return s.state, nil
}
func (s *oauthControllerAdminStub) ListManagedOAuthAuthEvents(context.Context, int64, int64) (*service.ManagedOAuthEventPage, error) {
	s.events++
	return &service.ManagedOAuthEventPage{Events: []service.ManagedOAuthAuthEvent{}, NextCursor: 0}, nil
}
func oauthControllerTestState() *service.ManagedOAuthPublicState {
	return &service.ManagedOAuthPublicState{AccountID: 17, ManagedOAuthIdentity: service.ManagedOAuthIdentity{Contract: service.ManagedOAuthContract, Provider: "codex", ProfileRef: "profile", BackendID: "backend", BindingRef: "binding", IdentityEpoch: "epoch", Subject: "subject", WorkspaceID: "workspace"}}
}
func oauthControllerTestRouter(t *testing.T, stub *oauthControllerAdminStub, raw string) *gin.Engine {
	t.Helper()
	t.Setenv(oauthControllersEnv, raw)
	t.Setenv("TEST_OAUTH_CONTROLLER_KEY", oauthControllerTestKey)
	gin.SetMode(gin.TestMode)
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	g := r.Group("/accounts", NewOAuthControllerAuth(h))
	g.GET("/:id/managed-credentials", h.GetManagedOAuthCredentials)
	g.PUT("/:id/managed-credentials", h.InstallManagedOAuthCredentials)
	g.GET("/:id/managed-credentials/auth-events", h.ListManagedOAuthAuthEvents)
	return r
}
func oauthControllerRequest(r *gin.Engine, method, path, body, key string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestOAuthControllerConfigurationRejectsUnsafeInputs(t *testing.T) {
	for _, raw := range []string{
		"", `null`, `[]`, `{}`, `[null]`, `[{}]`, oauthControllerTestConfig + ` []`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"],"unknown":true}]`,
		`[{"KEY_ENV":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":["backend"]}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","key_env":"OTHER","profile_refs":["profile"],"backend_ids":["backend"]}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":[],"backend_ids":["backend"]}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["profile"],"backend_ids":null}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["*"],"backend_ids":["backend"]}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":["profile","profile"],"backend_ids":["backend"]}]`,
		`[{"key_env":"TEST_OAUTH_CONTROLLER_KEY","profile_refs":[" profile"],"backend_ids":["backend"]}]`,
		`[{"key_env":"1INVALID","profile_refs":["profile"],"backend_ids":["backend"]}]`,
		`[{"key_env":"SUB6_OAUTH_CONTROLLERS","profile_refs":["profile"],"backend_ids":["backend"]}]`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseOAuthControllers(raw, func(string) (string, bool) { return oauthControllerTestKey, true })
			require.Error(t, err)
			require.NotContains(t, err.Error(), oauthControllerTestKey)
		})
	}
	for _, ids := range []string{`null`, `[]`, `[0]`, `[-1]`, `[17,17]`, `[1.5]`, `["17"]`} {
		raw := strings.TrimSuffix(oauthControllerTestConfig, `}]`) + `,"account_ids":` + ids + `}]`
		_, err := parseOAuthControllers(raw, func(string) (string, bool) { return oauthControllerTestKey, true })
		require.Error(t, err, ids)
	}
	for _, secret := range []string{"", strings.Repeat("x", 31), strings.Repeat("x", 4097), strings.Repeat("x", 32) + " ", strings.Repeat("x", 32) + "\n", strings.Repeat("x", 32) + "中"} {
		_, err := parseOAuthControllers(oauthControllerTestConfig, func(string) (string, bool) { return secret, true })
		require.Error(t, err)
	}
	_, err := parseOAuthControllers(oauthControllerTestConfig, func(string) (string, bool) { return "", false })
	require.Error(t, err)
	entry := strings.TrimSuffix(strings.TrimPrefix(oauthControllerTestConfig, `[`), `]`)
	for _, second := range []string{entry, strings.ReplaceAll(entry, "TEST_OAUTH_CONTROLLER_KEY", "OTHER_KEY")} {
		_, err := parseOAuthControllers(`[`+entry+`,`+second+`]`, func(string) (string, bool) { return oauthControllerTestKey, true })
		require.Error(t, err, "duplicate env names and duplicate secret values both fail closed")
	}
}

func TestOAuthControllerConfigurationKeepsOnlyHashAndExplicitScopes(t *testing.T) {
	raw := `[{"key_env":"KEY_A","profile_refs":["p1","p2"],"backend_ids":["b1","b2"],"account_ids":[17,18]},{"key_env":"KEY_B","profile_refs":["p3"],"backend_ids":["b3"]}]`
	scopes, err := parseOAuthControllers(raw, func(name string) (string, bool) { return oauthControllerTestKey + name, true })
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	require.Equal(t, sha256.Sum256([]byte(oauthControllerTestKey+"KEY_A")), scopes[0].keyHash)
	require.Equal(t, map[string]bool{"p1": true, "p2": true}, scopes[0].profiles)
	require.Equal(t, map[string]bool{"b1": true, "b2": true}, scopes[0].backends)
	require.Equal(t, map[int64]bool{17: true, 18: true}, scopes[0].accountIDs)
	require.Nil(t, scopes[1].accountIDs, "account allowlist may be omitted for pre-registration")
}

func TestOAuthControllerInvalidKeyDoesNotReadAccountMetadata(t *testing.T) {
	stub := &oauthControllerAdminStub{state: oauthControllerTestState()}
	r := oauthControllerTestRouter(t, stub, oauthControllerTestConfig)
	for _, id := range []string{"17", "999999", "bad"} {
		for _, key := range []string{"", "wrong-key"} {
			w := oauthControllerRequest(r, http.MethodGet, "/accounts/"+id+"/managed-credentials", "", key)
			require.Equal(t, 401, w.Code)
			require.Contains(t, w.Body.String(), "authorization required")
		}
	}
	for _, headers := range []http.Header{
		{"X-Api-Key": {oauthControllerTestKey}},
		{"Authorization": {"Basic " + oauthControllerTestKey}},
		{"Authorization": {"Bearer " + oauthControllerTestKey, "Bearer " + oauthControllerTestKey}},
		{"Authorization": {"Bearer  " + oauthControllerTestKey}},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/accounts/17/managed-credentials", nil)
		req.Header = headers
		r.ServeHTTP(w, req)
		require.Equal(t, 401, w.Code)
	}
	require.Zero(t, stub.reads)
	require.Zero(t, stub.installs)
}

func TestOAuthControllerScopeAndMalformedMetadataFailClosed(t *testing.T) {
	for _, name := range []string{"profile", "backend", "account", "different-id", "null-marker", "invalid-marker", "not-managed", "not-found"} {
		t.Run(name, func(t *testing.T) {
			stub := &oauthControllerAdminStub{state: oauthControllerTestState()}
			raw := oauthControllerTestConfig
			switch name {
			case "profile":
				stub.state.ProfileRef = "other"
			case "backend":
				stub.state.BackendID = "other"
			case "account":
				raw = strings.TrimSuffix(raw, `}]`) + `,"account_ids":[18]}]`
			case "different-id":
				stub.state.AccountID = 18
			case "null-marker":
				stub.state = nil
			case "invalid-marker":
				stub.state.ManagedOAuthIdentity = service.ManagedOAuthIdentity{}
			case "not-managed":
				stub.stateErr = service.ErrManagedOAuthInvalid
			case "not-found":
				stub.stateErr = errors.New("secret internal account lookup detail")
			}
			r := oauthControllerTestRouter(t, stub, raw)
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				w := oauthControllerRequest(r, method, "/accounts/17/managed-credentials", `{"access_token":"sensitive-at"}`, oauthControllerTestKey)
				require.Equal(t, 403, w.Code, w.Body.String())
				require.JSONEq(t, `{"code":403,"message":"OAuth controller account access denied"}`, w.Body.String())
			}
			require.Zero(t, stub.installs)
			if name == "account" {
				require.Zero(t, stub.reads)
			}
		})
	}
}

func TestOAuthControllerRegisteredRevisionZeroAndATOnlyHandlers(t *testing.T) {
	stub := &oauthControllerAdminStub{state: oauthControllerTestState()}
	r := oauthControllerTestRouter(t, stub, oauthControllerTestConfig)
	for _, path := range []string{"/accounts/17/managed-credentials", "/accounts/17/managed-credentials/auth-events"} {
		w := oauthControllerRequest(r, http.MethodGet, path, "", oauthControllerTestKey)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"code":0`)
		for _, secret := range []string{"access_token", "refresh_token", "id_token", "operation_hash", oauthControllerTestKey} {
			require.NotContains(t, w.Body.String(), secret)
		}
	}
	w := oauthControllerRequest(r, http.MethodPut, "/accounts/17/managed-credentials", `{"access_token":"synthetic-at","expected_token_revision":0,"token_revision":1}`, oauthControllerTestKey)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, stub.installs)
	require.Equal(t, "synthetic-at", stub.last.AccessToken)
	require.NotContains(t, w.Body.String(), "synthetic-at")
	for _, body := range []string{`{"refresh_token":"rt"}`, `{"id_token":"id"}`, `{"access_token":"a","access_token":"b"}`, `{"access_token":"a"} {}`, `[]`} {
		w := oauthControllerRequest(r, http.MethodPut, "/accounts/17/managed-credentials", body, oauthControllerTestKey)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	require.Equal(t, 1, stub.installs)
	require.Equal(t, 1, stub.events)
}

func TestOAuthControllerInvalidConfigurationFailsClosed(t *testing.T) {
	for _, raw := range []string{"", `[]`, `{"key":"inline-secret-is-forbidden"}`} {
		stub := &oauthControllerAdminStub{state: oauthControllerTestState()}
		r := oauthControllerTestRouter(t, stub, raw)
		w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/managed-credentials", "", oauthControllerTestKey)
		require.Equal(t, 503, w.Code)
		require.Zero(t, stub.reads)
	}
}

func TestOAuthControllerDoesNotCreateAdministratorPrincipal(t *testing.T) {
	t.Setenv(oauthControllersEnv, oauthControllerTestConfig)
	t.Setenv("TEST_OAUTH_CONTROLLER_KEY", oauthControllerTestKey)
	stub := &oauthControllerAdminStub{state: oauthControllerTestState()}
	r := gin.New()
	r.GET("/accounts/:id", NewOAuthControllerAuth(&AccountHandler{adminService: stub}), func(c *gin.Context) {
		for _, key := range []string{"user", "user_role", "auth_method", "user_id"} {
			_, exists := c.Get(key)
			require.False(t, exists, key)
		}
		c.Status(http.StatusNoContent)
	})
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17", "", oauthControllerTestKey)
	require.Equal(t, http.StatusNoContent, w.Code)
}

type oauthControllerPersistentStub struct {
	*oauthControllerAdminStub
	registered                                           *service.ManagedOAuthControllerKeyState
	authenticated                                        *service.ManagedOAuthControllerKeyState
	authErr                                              error
	registerErr, revokeErr                               error
	authCalls, registrations, revocations                int
	registeredAccount                                    int64
	registeredOperation, registeredKey, revokedOperation string
}

func (s *oauthControllerPersistentStub) RegisterManagedOAuthControllerKey(_ context.Context, id int64, operation, key string) (*service.ManagedOAuthControllerKeyState, error) {
	s.registrations++
	s.registeredAccount, s.registeredOperation, s.registeredKey = id, operation, key
	if s.registerErr != nil {
		return nil, s.registerErr
	}
	return s.registered, nil
}
func (s *oauthControllerPersistentStub) RevokeManagedOAuthControllerKey(_ context.Context, id int64, operation string) (*service.ManagedOAuthControllerKeyState, error) {
	s.revocations++
	s.registeredAccount, s.revokedOperation = id, operation
	if s.revokeErr != nil {
		return nil, s.revokeErr
	}
	return s.registered, nil
}
func (s *oauthControllerPersistentStub) AuthenticateManagedOAuthControllerKey(context.Context, string) (*service.ManagedOAuthControllerKeyState, error) {
	s.authCalls++
	return s.authenticated, s.authErr
}
func oauthControllerPersistentState() *service.ManagedOAuthControllerKeyState {
	return &service.ManagedOAuthControllerKeyState{ManagedOAuthIdentity: oauthControllerTestState().ManagedOAuthIdentity, AccountID: 17, OperationID: "c45ce361-c1d8-44b2-89b3-d0955e17cc88", Active: true}
}
func oauthControllerPersistentRouter(t *testing.T, stub *oauthControllerPersistentStub, raw string) *gin.Engine {
	t.Helper()
	t.Setenv(oauthControllersEnv, raw)
	t.Setenv("TEST_OAUTH_CONTROLLER_KEY", oauthControllerTestKey)
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	g := r.Group("/accounts", NewOAuthControllerAuth(h))
	g.GET("/:id/managed-credentials", h.GetManagedOAuthCredentials)
	g.PUT("/:id/managed-credentials", h.InstallManagedOAuthCredentials)
	g.GET("/:id/managed-credentials/auth-events", h.ListManagedOAuthAuthEvents)
	return r
}

func TestOAuthControllerPersistentKeyWorksWithoutEnvironmentScopes(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authenticated: oauthControllerPersistentState()}
	r := oauthControllerPersistentRouter(t, stub, "")
	for _, path := range []string{"/accounts/17/managed-credentials", "/accounts/17/managed-credentials/auth-events"} {
		w := oauthControllerRequest(r, http.MethodGet, path, "", oauthControllerTestKey)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.NotContains(t, w.Body.String(), oauthControllerTestKey)
	}
	w := oauthControllerRequest(r, http.MethodPut, "/accounts/17/managed-credentials", `{"access_token":"persistent-synthetic-at","expected_token_revision":0,"token_revision":1}`, oauthControllerTestKey)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, stub.installs)
	require.Equal(t, 3, stub.authCalls)
	require.NotContains(t, w.Body.String(), "persistent-synthetic-at")
}

func TestOAuthControllerPersistentKeyPrecedesStaticEnvironmentScope(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authenticated: oauthControllerPersistentState()}
	raw := strings.ReplaceAll(oauthControllerTestConfig, `"profile"`, `"unrelated-profile"`)
	r := oauthControllerPersistentRouter(t, stub, raw)
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/managed-credentials", "", oauthControllerTestKey)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, stub.authCalls)
}

func TestOAuthControllerPersistentKeyRequiresExactAccountAndCurrentIdentity(t *testing.T) {
	for _, field := range []string{"account", "profile", "backend", "binding", "epoch", "subject", "workspace", "provider"} {
		t.Run(field, func(t *testing.T) {
			stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authenticated: oauthControllerPersistentState()}
			path := "/accounts/17/managed-credentials"
			switch field {
			case "account":
				path = "/accounts/18/managed-credentials"
			case "profile":
				stub.state.ProfileRef = "different-profile"
			case "backend":
				stub.state.BackendID = "different-backend"
			case "binding":
				stub.state.BindingRef = "different-binding"
			case "epoch":
				stub.state.IdentityEpoch = "different-epoch"
			case "subject":
				stub.state.Subject = "different-subject"
			case "workspace":
				stub.state.WorkspaceID = "different-workspace"
			case "provider":
				stub.state.Provider = "bps"
			}
			r := oauthControllerPersistentRouter(t, stub, "")
			w := oauthControllerRequest(r, http.MethodGet, path, "", oauthControllerTestKey)
			require.Equal(t, 403, w.Code, w.Body.String())
			require.Zero(t, stub.installs)
			if field == "account" {
				require.Zero(t, stub.reads)
			}
		})
	}
}

func TestOAuthControllerPersistentRevokedOrStaleKeyDoesNotAuthorize(t *testing.T) {
	for _, name := range []string{"revoked", "stale-epoch", "unknown"} {
		t.Run(name, func(t *testing.T) {
			stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authErr: service.ErrManagedOAuthInvalid}
			r := oauthControllerPersistentRouter(t, stub, "")
			for _, id := range []string{"17", "999999"} {
				w := oauthControllerRequest(r, http.MethodGet, "/accounts/"+id+"/managed-credentials", "", oauthControllerTestKey)
				require.Equal(t, 401, w.Code, w.Body.String())
			}
			require.Zero(t, stub.reads)
		})
	}
}

func TestOAuthControllerUnknownPersistentKeyMayUseExplicitStaticScope(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authErr: service.ErrManagedOAuthControllerKeyUnknown}
	r := oauthControllerPersistentRouter(t, stub, oauthControllerTestConfig)
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/managed-credentials", "", oauthControllerTestKey)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, stub.authCalls)
}

func TestOAuthControllerInvalidPersistentKeyCannotFallBackToSameStaticSecret(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authErr: service.ErrManagedOAuthInvalid}
	r := oauthControllerPersistentRouter(t, stub, oauthControllerTestConfig)
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/managed-credentials", "", oauthControllerTestKey)
	require.Equal(t, 401, w.Code, w.Body.String())
	require.Equal(t, 1, stub.authCalls)
	require.Zero(t, stub.reads, "revoked or stale persistent keys must not recover access through a matching static secret")
	require.Zero(t, stub.installs)
}

func TestOAuthControllerMalformedStaticConfigurationDisablesPersistentKeys(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, authenticated: oauthControllerPersistentState()}
	r := oauthControllerPersistentRouter(t, stub, `[{"key_env":"incomplete"}]`)
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/managed-credentials", "", oauthControllerTestKey)
	require.Equal(t, 503, w.Code, w.Body.String())
	require.Zero(t, stub.authCalls)
	require.Zero(t, stub.reads)
}

func oauthControllerRegistrationRouter(stub *oauthControllerPersistentStub) *gin.Engine {
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	r.POST("/accounts/:id/oauth-controller-credentials", h.RegisterOAuthControllerCredentials)
	r.DELETE("/accounts/:id/oauth-controller-credentials/:operation_id", h.RevokeOAuthControllerCredentials)
	return r
}

func TestOAuthControllerRegistrationRejectsNonKeyOrAmbiguousPayloads(t *testing.T) {
	op := oauthControllerPersistentState().OperationID
	valid := `{"operation_id":"` + op + `","key":"` + oauthControllerTestKey + `"}`
	for _, body := range []string{
		`[]`, `null`, `{}`, valid + ` {}`, strings.Repeat("a", 129<<10),
		strings.TrimSuffix(valid, `}`) + `,"refresh_token":"rt"}`,
		strings.TrimSuffix(valid, `}`) + `,"id_token":"id"}`,
		strings.TrimSuffix(valid, `}`) + `,"access_token":"at"}`,
		strings.TrimSuffix(valid, `}`) + `,"schedulable":true}`,
		strings.TrimSuffix(valid, `}`) + `,"key":"second"}`,
		strings.ReplaceAll(valid, `"operation_id"`, `"Operation_ID"`),
		strings.ReplaceAll(valid, op, "not-uuid"),
		strings.ReplaceAll(valid, op, "00000000-0000-0000-0000-000000000000"),
		strings.ReplaceAll(valid, op, strings.ToUpper(op)),
		strings.ReplaceAll(valid, oauthControllerTestKey, strings.Repeat("x", 31)),
	} {
		stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, registered: oauthControllerPersistentState()}
		r := oauthControllerRegistrationRouter(stub)
		w := oauthControllerRequest(r, http.MethodPost, "/accounts/17/oauth-controller-credentials", body, "")
		require.Equal(t, 400, w.Code, w.Body.String())
		require.Zero(t, stub.registrations)
	}
}

func TestOAuthControllerRegistrationRetriesAndRevocationReturnOnlyPublicState(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, registered: oauthControllerPersistentState()}
	r := oauthControllerRegistrationRouter(stub)
	op := stub.registered.OperationID
	body := `{"operation_id":"` + op + `","key":"` + oauthControllerTestKey + `"}`
	var first string
	for i := 0; i < 2; i++ {
		w := oauthControllerRequest(r, http.MethodPost, "/accounts/17/oauth-controller-credentials", body, "")
		require.Equal(t, 200, w.Code, w.Body.String())
		for _, secret := range []string{oauthControllerTestKey, `"key":`, "key_hash", "refresh_token", "access_token"} {
			require.NotContains(t, w.Body.String(), secret)
		}
		require.Contains(t, w.Body.String(), `"active":true`)
		if i == 0 {
			first = w.Body.String()
		} else {
			require.JSONEq(t, first, w.Body.String(), "retry after lost registration response is safe")
		}
	}
	require.Equal(t, 2, stub.registrations)
	require.EqualValues(t, 17, stub.registeredAccount)
	require.Equal(t, op, stub.registeredOperation)
	require.Equal(t, oauthControllerTestKey, stub.registeredKey)
	stub.registered.Active = false
	for i := 0; i < 2; i++ {
		w := oauthControllerRequest(r, http.MethodDelete, "/accounts/17/oauth-controller-credentials/"+op, "", "")
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"active":false`)
		require.NotContains(t, w.Body.String(), oauthControllerTestKey)
	}
	require.Equal(t, 2, stub.revocations)
	require.Equal(t, op, stub.revokedOperation)
}

func TestOAuthControllerRegistrationAndRevocationSurfaceConflicts(t *testing.T) {
	stub := &oauthControllerPersistentStub{oauthControllerAdminStub: &oauthControllerAdminStub{state: oauthControllerTestState()}, registerErr: service.ErrManagedOAuthConflict, revokeErr: service.ErrManagedOAuthConflict}
	r := oauthControllerRegistrationRouter(stub)
	op := oauthControllerPersistentState().OperationID
	w := oauthControllerRequest(r, http.MethodPost, "/accounts/17/oauth-controller-credentials", `{"operation_id":"`+op+`","key":"`+oauthControllerTestKey+`"}`, "")
	require.Equal(t, 409, w.Code, w.Body.String())
	w = oauthControllerRequest(r, http.MethodDelete, "/accounts/17/oauth-controller-credentials/"+op, "", "")
	require.Equal(t, 409, w.Code, w.Body.String(), "unknown operation must not claim revocation")
	require.NotContains(t, w.Body.String(), `"active":false`)
	for _, operation := range []string{"bad", "00000000-0000-0000-0000-000000000000", strings.ToUpper(op)} {
		w = oauthControllerRequest(r, http.MethodDelete, "/accounts/17/oauth-controller-credentials/"+operation, "", "")
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	require.Equal(t, 1, stub.revocations)
}
