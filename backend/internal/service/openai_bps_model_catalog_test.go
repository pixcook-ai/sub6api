package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// bpsAccessStub answers the BPS access endpoint; GET requests carry no body.
type bpsAccessStub struct {
	HTTPUpstream
	body   string
	status int
	calls  int
	method string
	uri    string
}

func (u *bpsAccessStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls++
	u.method = req.Method
	u.uri = req.URL.RequestURI()
	status := u.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}

const bpsCatalogBody = `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra"},{"id":"gpt-5.6-sol"}]}}`

func bpsCatalogAccount(id int64, token string, mapping map[string]any) Account {
	creds := map[string]any{"access_token": token, "chatgpt_account_id": "ws-1"}
	if mapping != nil {
		creds["model_mapping"] = mapping
	}
	return Account{ID: id, Platform: PlatformOpenAIBPS, Type: AccountTypeOAuth, Credentials: creds}
}

func bpsCatalogService(upstream HTTPUpstream, accounts ...Account) *OpenAIGatewayService {
	return &OpenAIGatewayService{accountRepo: &compositeOwnershipAccountRepo{accounts: accounts}, httpUpstream: upstream}
}

func TestOpenAIBPSAvailableModels_RespectsAccountWhitelistAndUpstreamCatalog(t *testing.T) {
	upstream := &bpsAccessStub{body: bpsCatalogBody}
	mapping := map[string]any{
		"alias":       "gpt-6-astra",
		"gpt-5.6-sol": "gpt-5.6-sol",
		"gone":        "gpt-5.6-terra",
	}
	svc := bpsCatalogService(upstream, bpsCatalogAccount(1, "token-intersect", mapping))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	// The account's non-empty mapping limits client-facing IDs, while upstream
	// authorization still determines which mapped targets can be advertised.
	require.Equal(t, []string{"alias", "gpt-5.6-sol"}, ids)
	require.Equal(t, http.MethodGet, upstream.method)
	require.Equal(t, "/basispoints/api/responses/access?include_models=true", upstream.uri)
}

func TestOpenAIBPSAvailableModels_ExpandsWildcardWhitelistAgainstUpstreamCatalog(t *testing.T) {
	upstream := &bpsAccessStub{body: bpsCatalogBody}
	svc := bpsCatalogService(upstream, bpsCatalogAccount(5, "token-wildcard", map[string]any{"gpt-*": "gpt-6-astra"}))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-6-astra"}, ids)
}

func TestOpenAIBPSAvailableModels_HidesWildcardWhenTargetUnavailable(t *testing.T) {
	upstream := &bpsAccessStub{body: bpsCatalogBody}
	svc := bpsCatalogService(upstream, bpsCatalogAccount(6, "token-wildcard-denied", map[string]any{"gpt-*": "gpt-5.6-terra"}))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	require.Empty(t, ids)
}

func TestOpenAIBPSAvailableModels_AccountWithoutMappingExposesWholeCatalog(t *testing.T) {
	svc := bpsCatalogService(&bpsAccessStub{body: bpsCatalogBody}, bpsCatalogAccount(2, "token-nomap", map[string]any{}))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-6-astra"}, ids)
}

func TestOpenAIBPSAvailableModels_UpstreamFailureFallsBack(t *testing.T) {
	svc := bpsCatalogService(&bpsAccessStub{status: http.StatusForbidden, body: `{}`}, bpsCatalogAccount(3, "token-fail", nil))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.False(t, ok, "caller must fall back to the configured list")
	require.Empty(t, ids)
}

func TestOpenAIBPSAvailableModels_UpstreamFailureKeepsAccountRestrictions(t *testing.T) {
	svc := bpsCatalogService(&bpsAccessStub{status: http.StatusForbidden, body: "{}"},
		bpsCatalogAccount(7, "token-restricted-fallback", map[string]any{"alias": "gpt-6-astra"}))
	groupID := int64(3)

	ids, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	require.Equal(t, []string{"alias"}, ids)
}

func TestOpenAIBPSAvailableModels_ReusesFreshCache(t *testing.T) {
	upstream := &bpsAccessStub{body: bpsCatalogBody}
	svc := bpsCatalogService(upstream, bpsCatalogAccount(4, "token-cache", nil))
	groupID := int64(3)

	_, ok := svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	_, ok = svc.OpenAIBPSAvailableModels(context.Background(), &groupID)
	require.True(t, ok)
	require.Equal(t, 1, upstream.calls, "second lookup must be served from the fresh cache")
}
