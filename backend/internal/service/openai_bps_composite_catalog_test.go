package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type bpsCompositeCatalogAccountRepo struct {
	AccountRepository
	accounts []Account
}

func (r bpsCompositeCatalogAccountRepo) ListByGroup(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}

func newBPSCompositeCatalogAccount(mapping map[string]any) Account {
	return Account{
		ID:          1,
		Platform:    PlatformOpenAIBPS,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token":       "test-token",
			"chatgpt_account_id": "workspace",
			"model_mapping":      mapping,
		},
	}
}

func TestCompleteCompositeBPSModelCatalogRequiresKnownTarget(t *testing.T) {
	defaults := OpenAIBPSDefaultModels()
	require.NotEmpty(t, defaults)
	for _, tc := range []struct {
		name    string
		mapping map[string]any
		target  string
		listed  bool
	}{
		{name: "mapping value", mapping: map[string]any{"configured-alias": "custom-upstream-model"}, target: "custom-upstream-model", listed: true},
		{name: "mapping key", mapping: map[string]any{"configured-alias": "custom-upstream-model"}, target: "configured-alias", listed: true},
		{name: "default model", target: defaults[0], listed: true},
		{name: "default with alias mapping", mapping: map[string]any{"configured-alias": "custom-upstream-model"}, target: defaults[0], listed: true},
		{name: "unknown target", mapping: map[string]any{"configured-alias": "custom-upstream-model"}, target: "unavailable-model"},
		{name: "wildcard is not target evidence", mapping: map[string]any{"*": "custom-upstream-model"}, target: "unavailable-model"},
		{name: "wildcard target is not enumerable", mapping: map[string]any{"*": "custom-upstream-model"}, target: "*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(91)
			account := newBPSCompositeCatalogAccount(tc.mapping)
			svc := &GatewayService{
				accountRepo: bpsCompositeCatalogAccountRepo{accounts: []Account{account}},
				compositeResolver: NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{{
					ID: 1, GroupID: groupID, PublicModel: "bps-public", MatchType: CompositeRouteMatchExact,
					TargetPlatform: PlatformOpenAIBPS, UpstreamModel: tc.target, Endpoint: CompositeRouteEndpointResponses, Enabled: true,
				}}}),
			}

			models, err := svc.CompleteCompositeBPSModelCatalog(context.Background(), &groupID, nil)
			require.NoError(t, err)
			if tc.listed {
				require.Equal(t, []string{"bps-public"}, models)
			} else {
				require.NotNil(t, models, "configured empty catalog must disable static fallback")
				require.Empty(t, models)
			}
		})
	}
}

func TestCompleteCompositeBPSModelCatalogExactRouteOverridesPrefix(t *testing.T) {
	groupID := int64(91)
	account := newBPSCompositeCatalogAccount(map[string]any{"bps-prefix": "custom-upstream-model"})
	prefix := CompositeModelRoute{
		ID: 1, GroupID: groupID, PublicModel: "bps-", MatchType: CompositeRouteMatchPrefix,
		TargetPlatform: PlatformOpenAIBPS, Endpoint: CompositeRouteEndpointResponses, Enabled: true,
	}
	svc := &GatewayService{
		accountRepo:       bpsCompositeCatalogAccountRepo{accounts: []Account{account}},
		compositeResolver: NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{prefix}}),
	}
	models, err := svc.CompleteCompositeBPSModelCatalog(context.Background(), &groupID, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"bps-prefix"}, models)

	exact := prefix
	exact.ID = 2
	exact.PublicModel = "bps-prefix"
	exact.MatchType = CompositeRouteMatchExact
	exact.UpstreamModel = "unavailable-model"
	svc.compositeResolver = NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{prefix, exact}})
	models, err = svc.CompleteCompositeBPSModelCatalog(context.Background(), &groupID, nil)
	require.NoError(t, err)
	require.NotNil(t, models)
	require.Empty(t, models)
}

func TestBPSCompositeCatalogDoesNotRestrictRequestAdmission(t *testing.T) {
	account := newBPSCompositeCatalogAccount(map[string]any{"configured-alias": "custom-upstream-model"})
	require.False(t, bpsCompositeCatalogTargetKnown(&account, "new-upstream-model"))
	require.True(t, account.IsModelSupported("new-upstream-model"))
	require.True(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(context.Background(), &account, PlatformOpenAIBPS, "new-upstream-model", false, OpenAIEndpointCapabilityResponses))
}
