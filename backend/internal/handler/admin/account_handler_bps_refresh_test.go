//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/openai"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type bpsHandlerRefreshRepo struct {
	service.AccountRepository
	account  *service.Account
	casCalls int
}

func (r *bpsHandlerRefreshRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}
func (r *bpsHandlerRefreshRepo) UpdateOpenAIBPSOAuthCredentialsIfUnchanged(_ context.Context, _ *service.Account, creds map[string]any, _, _ bool) (bool, error) {
	r.casCalls++
	r.account.Credentials = creds
	return true, nil
}

type bpsHandlerRefreshCache struct{ service.GeminiTokenCache }

func (*bpsHandlerRefreshCache) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}
func (*bpsHandlerRefreshCache) ReleaseRefreshLock(context.Context, string) error { return nil }
func (*bpsHandlerRefreshCache) DeleteAccessToken(context.Context, string) error  { return nil }

type bpsHandlerRefreshClient struct {
	service.OpenAIOAuthClient
	calls int
}

func (c *bpsHandlerRefreshClient) RefreshTokenWithClientID(_ context.Context, _ string, _ string, id string) (*openai.TokenResponse, error) {
	c.calls++
	return &openai.TokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, nil
}

type bpsHandlerRefreshAdmin struct {
	*stubAdminService
	account      *service.Account
	privacyCalls int
}

func (s *bpsHandlerRefreshAdmin) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}
func (s *bpsHandlerRefreshAdmin) EnsureOpenAIPrivacy(context.Context, *service.Account) string {
	s.privacyCalls++
	return ""
}

func TestAccountHandlerRefreshBPSExcelUsesSharedAPI(t *testing.T) {
	a := &service.Account{ID: 42, Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: false, Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "client_id": openai.ExcelClientID, "chatgpt_account_id": "chosen-workspace", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	repo := &bpsHandlerRefreshRepo{account: a}
	client := &bpsHandlerRefreshClient{}
	adminSvc := &bpsHandlerRefreshAdmin{stubAdminService: newStubAdminService(), account: a}
	h := NewAccountHandler(adminSvc, nil, service.NewOpenAIOAuthService(nil, client), nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h.SetOpenAIBPSRefreshAPI(service.NewOAuthRefreshAPI(repo, &bpsHandlerRefreshCache{}))
	router := gin.New()
	router.POST("/accounts/:id/refresh", h.Refresh)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/42/refresh", nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, repo.casCalls)
	require.Zero(t, adminSvc.privacyCalls)
	require.Equal(t, "new-access", a.GetCredential("access_token"))
	require.Equal(t, "chosen-workspace", a.GetCredential("chatgpt_account_id"))
	require.False(t, a.Schedulable)
}
func TestAccountHandlerRefreshBPSATOnlyExplicitlyRejected(t *testing.T) {
	a := &service.Account{ID: 42, Platform: service.PlatformOpenAIBPS, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "manual-access"}}
	adminSvc := &bpsHandlerRefreshAdmin{stubAdminService: newStubAdminService(), account: a}
	h := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/accounts/:id/refresh", h.Refresh)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/accounts/42/refresh", nil))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "BPS_MANUAL_TOKEN_ONLY")
	require.Zero(t, adminSvc.privacyCalls)
}
