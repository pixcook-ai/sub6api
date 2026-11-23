//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type resourceAuthorizationKeyRepo struct {
	service.APIKeyRepository
	fresh      *service.APIKey
	err        error
	reads      int
	credential string
	afterRead  func()
}

func (r *resourceAuthorizationKeyRepo) GetByKey(_ context.Context, credential string) (*service.APIKey, error) {
	r.reads++
	r.credential = credential
	if r.afterRead != nil {
		r.afterRead()
	}
	if r.fresh == nil {
		return nil, r.err
	}
	clone := *r.fresh
	return &clone, r.err
}

type resourceAuthorizationSubscriptionRepo struct {
	service.UserSubscriptionRepository
	fresh *service.UserSubscription
	err   error
	reads int
}

func (r *resourceAuthorizationSubscriptionRepo) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*service.UserSubscription, error) {
	r.reads++
	if r.fresh == nil {
		return nil, r.err
	}
	clone := *r.fresh
	return &clone, r.err
}

func resourceAuthorizationTestKey() *service.APIKey {
	groupID := int64(9)
	return &service.APIKey{ID: 3, UserID: 7, GroupID: &groupID, Key: "resource-authorization-secret", Status: service.StatusActive,
		User:  &service.User{ID: 7, Status: service.StatusActive, Balance: 10},
		Group: &service.Group{ID: groupID, Status: service.StatusActive, Hydrated: true, Platform: service.PlatformOpenAI, SubscriptionType: service.SubscriptionTypeStandard}}
}

func resourceAuthorizationBind(t *testing.T, key *service.APIKey, subs *service.SubscriptionService, method, path string) (service.BPSRequestReauthorize, func(context.Context) error, *resourceAuthorizationKeyRepo, *http.Request) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &resourceAuthorizationKeyRepo{fresh: key}
	cfg := &config.Config{}
	keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key); c.Next() })
	router.Use(NewResourceRequestAuthorizationMiddleware(keys, subs, cfg))
	var callback service.BPSRequestReauthorize
	var upload func(context.Context) error
	router.Handle(method, path, func(c *gin.Context) {
		callback = service.BPSRequestReauthorizeFromContext(c.Request.Context())
		upload = FileUploadReauthorize(c)
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.RemoteAddr = "192.0.2.1:1234"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return callback, upload, repo, req
}

func resourceAuthorizationCheck(callback service.BPSRequestReauthorize, model string) error {
	return service.CheckBPSRequestAuthorization(service.WithBPSRequestReauthorize(context.Background(), callback), model)
}

func TestResourceRequestAuthorizationBindsOnlyInferencePathsLazily(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/v1/responses", true}, {http.MethodPost, "/responses", true},
		{http.MethodPost, "/backend-api/codex/responses", true}, {http.MethodPost, "/openai/v1/responses", true},
		{http.MethodPost, "/v1/responses/compact", true}, {http.MethodPost, "/v1/chat/completions", true},
		{http.MethodPost, "/chat/completions", true}, {http.MethodPost, "/v1/messages", true},
		{http.MethodGet, "/v1/models", false}, {http.MethodPost, "/v1/messages/count_tokens", false},
		{http.MethodGet, "/v1/responses/owned", false}, {http.MethodPost, "/v1/files", false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			callback, _, repo, _ := resourceAuthorizationBind(t, resourceAuthorizationTestKey(), nil, tc.method, tc.path)
			require.Equal(t, tc.want, callback != nil)
			if tc.want {
				require.Zero(t, repo.reads, "binding must not query the database")
			}
		})
	}
}

func TestResourceRequestAuthorizationRechecksCurrentAuthority(t *testing.T) {
	for _, tc := range []struct {
		name            string
		change          func(*service.APIKey)
		lookupErr, want error
	}{
		{"valid", nil, nil, nil},
		{"deleted key", nil, service.ErrAPIKeyNotFound, service.ErrBPSRequestAuthorizationRevoked},
		{"database error", nil, errors.New("sk-private https://internal.invalid"), service.ErrBPSRequestAuthorizationUnavailable},
		{"other key", func(k *service.APIKey) { k.ID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"other credential", func(k *service.APIKey) { k.Key = "other-secret" }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"other user", func(k *service.APIKey) { k.UserID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"other user record", func(k *service.APIKey) { k.User.ID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"deleted user", func(k *service.APIKey) { now := time.Now(); k.User.DeletedAt = &now }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled user", func(k *service.APIKey) { k.User.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"missing user", func(k *service.APIKey) { k.User = nil }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"other group", func(k *service.APIKey) { id := int64(88); k.GroupID = &id }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"other group record", func(k *service.APIKey) { k.Group.ID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"missing group", func(k *service.APIKey) { k.Group = nil }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled group", func(k *service.APIKey) { k.Group.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"permission revoked", func(k *service.APIKey) { k.User.RestrictPublicGroups = true }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"disabled key", func(k *service.APIKey) { k.Status = service.StatusDisabled }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"expired key", func(k *service.APIKey) { past := time.Now().Add(-time.Hour); k.ExpiresAt = &past }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"quota exhausted", func(k *service.APIKey) { k.Status = service.StatusAPIKeyQuotaExhausted }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"quota amount", func(k *service.APIKey) { k.Quota = 1; k.QuotaUsed = 1 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"no balance", func(k *service.APIKey) { k.User.Balance = 0 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"5h limit", func(k *service.APIKey) { now := time.Now(); k.Window5hStart = &now; k.RateLimit5h = 1; k.Usage5h = 1 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"1d limit", func(k *service.APIKey) { now := time.Now(); k.Window1dStart = &now; k.RateLimit1d = 1; k.Usage1d = 1 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"7d limit", func(k *service.APIKey) { now := time.Now(); k.Window7dStart = &now; k.RateLimit7d = 1; k.Usage7d = 1 }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"model removed", func(k *service.APIKey) {
			k.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"other-model"}}
		}, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"IP denied", func(k *service.APIKey) { k.IPBlacklist = []string{"192.0.2.1"} }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"IP no longer allowed", func(k *service.APIKey) { k.IPWhitelist = []string{"198.51.100.1"} }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"wrong platform", func(k *service.APIKey) { k.Group.Platform = service.PlatformAnthropic }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"native BPS", func(k *service.APIKey) { k.Group.Platform = service.PlatformOpenAIBPS }, nil, nil},
		{"composite", func(k *service.APIKey) { k.Group.Platform = service.PlatformComposite }, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback, _, repo, _ := resourceAuthorizationBind(t, resourceAuthorizationTestKey(), nil, http.MethodPost, "/v1/responses")
			repo.fresh = resourceAuthorizationTestKey()
			if tc.change != nil {
				tc.change(repo.fresh)
			}
			repo.err = tc.lookupErr
			err := resourceAuthorizationCheck(callback, "allowed-model")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.NotContains(t, err.Error(), "sk-private")
				require.NotContains(t, err.Error(), "internal.invalid")
			}
			require.Equal(t, 1, repo.reads)
		})
	}
}

func TestResourceRequestAuthorizationFreezesIngressFacts(t *testing.T) {
	key := resourceAuthorizationTestKey()
	callback, upload, repo, req := resourceAuthorizationBind(t, key, nil, http.MethodPost, "/v1/responses")
	repo.fresh = resourceAuthorizationTestKey()
	repo.fresh.IPWhitelist = []string{"192.0.2.1"}
	req.RemoteAddr = "198.51.100.1:9999"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	key.Key = "forged-secret"
	key.ID++
	key.UserID++
	*key.GroupID++
	require.NoError(t, resourceAuthorizationCheck(callback, "allowed-model"))
	require.Equal(t, "resource-authorization-secret", repo.credential)
	require.NoError(t, upload(context.Background()))
	repo.fresh.User.Status = service.StatusDisabled
	require.ErrorIs(t, upload(context.Background()), service.ErrFileAuthorizationRevoked)
}

func TestResourceRequestAuthorizationPreservesUnbilledFilesOnly(t *testing.T) {
	key := resourceAuthorizationTestKey()
	key.Status = service.StatusAPIKeyQuotaExhausted
	key.Quota = 1
	key.QuotaUsed = 1
	key.User.Balance = 0
	key.Group.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"other-model"}}
	callback, upload, repo, _ := resourceAuthorizationBind(t, key, nil, http.MethodPost, "/v1/files")
	require.Nil(t, callback)
	require.NotNil(t, upload)
	require.NoError(t, upload(context.Background()))
	facts := captureRequestAuthorizationFacts(key, "192.0.2.1")
	keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, &config.Config{})
	check := newBPSRequestReauthorize(keys, nil, &config.Config{}, key.Key, facts)
	require.ErrorIs(t, resourceAuthorizationCheck(check, "other-model"), service.ErrBPSRequestAuthorizationRevoked)
	key.IPBlacklist = []string{"192.0.2.1"}
	require.ErrorIs(t, upload(context.Background()), service.ErrFileAuthorizationRevoked)
}

func TestResourceRequestAuthorizationRechecksSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		change          func(*service.UserSubscription)
		lookupErr, want error
	}{
		{"active", nil, nil, nil},
		{"not found", nil, service.ErrSubscriptionNotFound, service.ErrBPSRequestAuthorizationRevoked},
		{"unavailable", nil, errors.New("private database"), service.ErrBPSRequestAuthorizationUnavailable},
		{"wrong owner", func(s *service.UserSubscription) { s.UserID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"wrong group", func(s *service.UserSubscription) { s.GroupID++ }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"deleted", func(s *service.UserSubscription) { now := time.Now(); s.DeletedAt = &now }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"expired", func(s *service.UserSubscription) { s.ExpiresAt = time.Now().Add(-time.Hour) }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"suspended", func(s *service.UserSubscription) { s.Status = service.SubscriptionStatusSuspended }, nil, service.ErrBPSRequestAuthorizationRevoked},
		{"daily limit", func(s *service.UserSubscription) { s.DailyUsageUSD = 2 }, nil, service.ErrBPSRequestAuthorizationRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := resourceAuthorizationTestKey()
			key.Group.SubscriptionType = service.SubscriptionTypeSubscription
			limit := 1.0
			key.Group.DailyLimitUSD = &limit
			now := time.Now()
			repo := &resourceAuthorizationSubscriptionRepo{fresh: &service.UserSubscription{ID: 5, UserID: 7, GroupID: 9, Status: service.SubscriptionStatusActive, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}}
			subs := service.NewSubscriptionService(nil, repo, nil, nil, &config.Config{})
			t.Cleanup(subs.Stop)
			callback, _, _, _ := resourceAuthorizationBind(t, key, subs, http.MethodPost, "/v1/responses")
			require.Zero(t, repo.reads)
			if tc.change != nil {
				tc.change(repo.fresh)
			}
			repo.err = tc.lookupErr
			err := resourceAuthorizationCheck(callback, "allowed-model")
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
			require.Equal(t, 1, repo.reads)
		})
	}
}

func TestResourceRequestAuthorizationFailsClosedAndCancels(t *testing.T) {
	key := resourceAuthorizationTestKey()
	facts := captureRequestAuthorizationFacts(key, "192.0.2.1")
	require.ErrorIs(t, resourceAuthorizationCheck(newBPSRequestReauthorize(nil, nil, nil, key.Key, facts), "model"), service.ErrBPSRequestAuthorizationUnavailable)
	callback, _, repo, _ := resourceAuthorizationBind(t, key, nil, http.MethodPost, "/v1/responses")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, callback(ctx, "model"), context.Canceled)
	require.Zero(t, repo.reads)
	during, cancelDuring := context.WithCancel(context.Background())
	defer cancelDuring()
	repo.afterRead = cancelDuring
	require.ErrorIs(t, callback(during, "model"), context.Canceled)
	repo.afterRead = nil
	repo.fresh = nil
	require.ErrorIs(t, resourceAuthorizationCheck(callback, "model"), service.ErrBPSRequestAuthorizationUnavailable)
	key = resourceAuthorizationTestKey()
	key.Group.SubscriptionType = service.SubscriptionTypeSubscription
	callback, _, _, _ = resourceAuthorizationBind(t, key, nil, http.MethodPost, "/v1/responses")
	require.ErrorIs(t, resourceAuthorizationCheck(callback, "model"), service.ErrBPSRequestAuthorizationUnavailable)
	require.Nil(t, FileUploadReauthorize(nil))
}

func TestResourceRequestAuthorizationRejectsMissingAuthenticatedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(NewResourceRequestAuthorizationMiddleware(nil, nil, nil))
	router.GET("/v1/files", func(c *gin.Context) { t.Fatal("unauthenticated handler reached") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/files", nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestResourceRequestAuthorizationEagerResourceReadUsesFreshAuthority(t *testing.T) {
	for _, path := range []string{"/v1/files/owned", "/openai/v1/files/owned/content", "/v1/responses/owned", "/responses/owned/input_items"} {
		for _, unavailable := range []bool{false, true} {
			t.Run(path+map[bool]string{false: " revoked", true: " unavailable"}[unavailable], func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				key := resourceAuthorizationTestKey()
				fresh := resourceAuthorizationTestKey()
				fresh.User.Status = service.StatusDisabled
				repo := &resourceAuthorizationKeyRepo{fresh: fresh}
				want := http.StatusForbidden
				if unavailable {
					repo.err = errors.New("sk-secret https://private.invalid")
					want = http.StatusServiceUnavailable
				}
				cfg := &config.Config{}
				keys := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
				router := gin.New()
				router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key); c.Next() })
				router.Use(NewResourceRequestAuthorizationMiddleware(keys, nil, cfg))
				router.GET(path, func(c *gin.Context) { t.Fatal("stale authority reached resource handler") })
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				require.Equal(t, want, w.Code)
				require.NotContains(t, w.Body.String(), "sk-secret")
				require.NotContains(t, w.Body.String(), "private.invalid")
				require.Equal(t, 1, repo.reads)
			})
		}
	}
}

func TestUnbilledResourceRequestDoesNotExemptInference(t *testing.T) {
	for _, root := range []string{"", "/v1", "/openai/v1", "/backend-api/codex"} {
		require.True(t, IsUnbilledResourceRequest(http.MethodGet, root+"/files"))
		require.True(t, IsUnbilledResourceRequest(http.MethodPost, root+"/files"))
		require.True(t, IsUnbilledResourceRequest(http.MethodDelete, root+"/files/file_a"))
		require.True(t, IsUnbilledResourceRequest(http.MethodGet, root+"/responses/resp_a"))
		require.False(t, IsUnbilledResourceRequest(http.MethodPost, root+"/responses"))
		require.False(t, IsUnbilledResourceRequest(http.MethodPost, root+"/responses/compact"))
		require.False(t, IsUnbilledResourceRequest(http.MethodPost, root+"/chat/completions"))
	}
}
