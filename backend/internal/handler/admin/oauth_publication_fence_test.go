//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type publicationFenceAdminStub struct {
	*oauthControllerAdminStub
	input *service.ManagedOAuthPublicationFenceInput
	calls int
	err   error
}

func (s *publicationFenceAdminStub) FenceManagedOAuthPublication(_ context.Context, id int64, input *service.ManagedOAuthPublicationFenceInput) (*service.ManagedOAuthPublicationFence, error) {
	s.calls++
	s.input = input
	if s.err != nil {
		return nil, s.err
	}
	return &service.ManagedOAuthPublicationFence{ManagedOAuthPublicationFenceInput: *input, AccountID: id, Fenced: true}, nil
}
func (s *publicationFenceAdminStub) GetManagedOAuthPublicationFence(_ context.Context, id int64) (*service.ManagedOAuthPublicationFence, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.input == nil {
		return nil, service.ErrManagedOAuthFenceMissing
	}
	return &service.ManagedOAuthPublicationFence{ManagedOAuthPublicationFenceInput: *s.input, AccountID: id, Fenced: true}, nil
}
func publicationFenceRouter(stub *publicationFenceAdminStub) *gin.Engine {
	h := &AccountHandler{adminService: stub}
	r := gin.New()
	r.POST("/accounts/:id/oauth-publication-fence", h.FenceOAuthPublication)
	r.GET("/accounts/:id/oauth-publication-fence", h.GetOAuthPublicationFence)
	return r
}
func TestOAuthPublicationFenceStrictInputAndReceipt(t *testing.T) {
	input := service.ManagedOAuthPublicationFenceInput{OperationID: oauthControllerPersistentState().OperationID, Identity: oauthControllerTestState().ManagedOAuthIdentity, TokenRevision: 2}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	for _, body := range []string{
		`null`, `{}`, `[]`, string(raw) + `{}`, strings.Repeat("x", 8193),
		strings.TrimSuffix(string(raw), `}`) + `,"refresh_token":"secret"}`,
		strings.TrimSuffix(string(raw), `}`) + `,"token_revision":3}`,
		strings.Replace(string(raw), `"operation_id"`, `"Operation_ID"`, 1),
		strings.Replace(string(raw), `"provider":"codex"`, `"provider":"codex","subject":"other"`, 1),
		strings.Replace(string(raw), `"provider":"codex"`, `"Provider":"codex"`, 1),
		strings.Replace(string(raw), `"token_revision":2`, `"token_revision":null`, 1),
		strings.Replace(string(raw), `"token_revision":2`, `"token_revision":-1`, 1),
		strings.Replace(string(raw), `"token_revision":2`, `"token_revision":9007199254740992`, 1),
	} {
		s := &publicationFenceAdminStub{oauthControllerAdminStub: &oauthControllerAdminStub{}}
		w := oauthControllerRequest(publicationFenceRouter(s), http.MethodPost, "/accounts/17/oauth-publication-fence", body, "")
		require.Equal(t, 400, w.Code, body)
		require.Zero(t, s.calls)
	}
	s := &publicationFenceAdminStub{oauthControllerAdminStub: &oauthControllerAdminStub{}}
	r := publicationFenceRouter(s)
	w := oauthControllerRequest(r, http.MethodGet, "/accounts/17/oauth-publication-fence", "", "")
	require.Equal(t, 404, w.Code)
	w = oauthControllerRequest(r, http.MethodPost, "/accounts/17/oauth-publication-fence", string(raw), "")
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, input, *s.input)
	read := oauthControllerRequest(r, http.MethodGet, "/accounts/17/oauth-publication-fence", "", "")
	require.Equal(t, 200, read.Code)
	require.JSONEq(t, w.Body.String(), read.Body.String())
	for _, secret := range []string{"access_token", "refresh_token", "key_hash", `"key":`} {
		require.NotContains(t, read.Body.String(), secret)
	}
	s.err = service.ErrManagedOAuthConflict
	w = oauthControllerRequest(r, http.MethodPost, "/accounts/17/oauth-publication-fence", string(raw), "")
	require.Equal(t, 409, w.Code)
}
