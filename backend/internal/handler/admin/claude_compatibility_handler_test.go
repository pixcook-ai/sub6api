package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type claudeCompatibilityAdminStub struct {
	service.AdminService
	account     *service.Account
	err         error
	calls       int
	requestedID int64
}

func (s *claudeCompatibilityAdminStub) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	s.calls++
	s.requestedID = id
	return s.account, s.err
}

func claudeCompatibilityResponse(stub *claudeCompatibilityAdminStub, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/"+id+"/claude-compatibility", nil)
	c.Params = gin.Params{{Key: "id", Value: id}}
	h := &AccountHandler{adminService: stub}
	h.GetClaudeOAuthCompatibility(c)
	return w
}

func TestClaudeOAuthCompatibilityRejectsInvalidAccountID(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1", "9223372036854775808"} {
		t.Run(id, func(t *testing.T) {
			stub := &claudeCompatibilityAdminStub{}
			w := claudeCompatibilityResponse(stub, id)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Zero(t, stub.calls)
		})
	}
}

func TestClaudeOAuthCompatibilityReturnsAccountNotFound(t *testing.T) {
	stub := &claudeCompatibilityAdminStub{err: service.ErrAccountNotFound}
	w := claudeCompatibilityResponse(stub, "913501")
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, int64(913501), stub.requestedID)
}

func TestClaudeOAuthCompatibilityRejectsOtherAccountKinds(t *testing.T) {
	for _, tc := range []struct{ platform, accountType string }{
		{service.PlatformAnthropic, service.AccountTypeAPIKey},
		{service.PlatformAnthropic, service.AccountTypeSetupToken},
		{service.PlatformOpenAI, service.AccountTypeOAuth},
	} {
		t.Run(tc.platform+"/"+tc.accountType, func(t *testing.T) {
			stub := &claudeCompatibilityAdminStub{account: &service.Account{ID: 913501, Platform: tc.platform, Type: tc.accountType}}
			w := claudeCompatibilityResponse(stub, "913501")
			require.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestClaudeOAuthCompatibilityReturnsTransparentObservationWithoutSensitiveAccountData(t *testing.T) {
	account := &service.Account{ID: 913501, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "never-expose-credential"},
		Extra:       map[string]any{"unrelated_private_value": "never-expose-extra"}}
	w := claudeCompatibilityResponse(&claudeCompatibilityAdminStub{account: account}, "913501")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Equal(t, int64(913501), gjson.Get(body, "data.account_id").Int())
	require.Equal(t, "transparent", gjson.Get(body, "data.attribution_mode").String())
	require.Equal(t, gjson.Null, gjson.Get(body, "data.observation").Type)
	require.Contains(t, body, `"observation":null`)
	require.NotContains(t, body, "never-expose")
	require.NotContains(t, body, "access_token")
}
