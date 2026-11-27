//go:build integration

package repository

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/pagination"
	"github.com/MACOS-DO/sub4api/internal/service"
)

func (s *ProxyRepoSuite) TestConfiguredAccountReferencesIncludeBPSGroupsOnce() {
	p1 := s.mustCreateProxy(&service.Proxy{Name: "configured-one", Protocol: "http", Host: "192.0.2.11", Port: 8080, Status: service.StatusActive})
	p2 := s.mustCreateProxy(&service.Proxy{Name: "configured-two", Protocol: "http", Host: "192.0.2.22", Port: 8080, Status: service.StatusActive})
	s.mustInsertAccount("traditional", &p1.ID)
	insert := func(name, platform, accountType string, proxyID *int64, extra map[string]any, deleted bool) {
		data, err := json.Marshal(extra)
		s.Require().NoError(err)
		var deletedAt any
		if deleted {
			deletedAt = time.Now()
		}
		_, err = s.tx.ExecContext(s.ctx, `INSERT INTO accounts (name, platform, type, proxy_id, extra, deleted_at) VALUES ($1,$2,$3,$4,$5,$6)`, name, platform, accountType, proxyID, string(data), deletedAt)
		s.Require().NoError(err)
	}
	group := func(enabled bool, ids ...int64) map[string]any {
		return map[string]any{"openai_excel_bps": enabled, service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: ids}
	}
	insert("both-fields", service.PlatformOpenAIBPS, service.AccountTypeOAuth, &p1.ID, group(true, p1.ID, p1.ID, p2.ID), false)
	insert("saved-disabled-bps", service.PlatformOpenAIBPS, service.AccountTypeOAuth, nil, group(false, p1.ID), false)
	insert("removed-account", service.PlatformOpenAI, service.AccountTypeOAuth, &p1.ID, group(true, p1.ID), true)
	insert("other-platform-extra", service.PlatformAnthropic, service.AccountTypeOAuth, nil, group(true, p1.ID), false)
	insert("other-type-extra", service.PlatformOpenAI, service.AccountTypeAPIKey, nil, group(true, p1.ID), false)
	insert("session-mode", service.PlatformOpenAI, service.AccountTypeOAuth, nil, map[string]any{service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeSession, service.ExcelBPSProxyIDsKey: []int64{p1.ID}}, false)
	insert("invalid-scalar", service.PlatformOpenAI, service.AccountTypeOAuth, nil, map[string]any{service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: "invalid"}, false)
	insert("invalid-elements", service.PlatformOpenAI, service.AccountTypeOAuth, nil, map[string]any{service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: []any{nil, "not-an-id", -1, 0, 1.5, json.Number("9223372036854775808")}}, false)

	accounts, err := s.repo.ListAccountSummariesByProxyID(s.ctx, p1.ID)
	s.Require().NoError(err)
	names := make([]string, 0, len(accounts))
	for _, account := range accounts {
		names = append(names, account.Name)
	}
	s.Require().ElementsMatch([]string{"traditional", "both-fields", "saved-disabled-bps"}, names)
	count, err := s.repo.CountAccountsByProxyID(s.ctx, p1.ID)
	s.Require().NoError(err)
	s.Require().Equal(int64(3), count)
	counts, err := s.repo.GetAccountCountsForProxies(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(int64(3), counts[p1.ID])
	s.Require().Equal(int64(1), counts[p2.ID])
	rows, _, err := s.repo.ListWithFiltersAndAccountCount(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10, SortBy: "account_count", SortOrder: "desc"}, "", "", "configured-")
	s.Require().NoError(err)
	s.Require().Len(rows, 2)
	s.Require().Equal(p1.ID, rows[0].ID)
	s.Require().Equal(int64(3), rows[0].AccountCount)
	s.Require().Equal(int64(1), rows[1].AccountCount)
}

func (s *ProxyRepoSuite) TestBPSConfiguredReferenceProtectsSingleAndBatchDeletion() {
	p := s.mustCreateProxy(&service.Proxy{Name: "saved-bps-group", Protocol: "http", Host: "192.0.2.31", Port: 8080, Status: service.StatusActive})
	extra, err := json.Marshal(map[string]any{"openai_excel_bps": false, service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: []int64{p.ID}})
	s.Require().NoError(err)
	_, err = s.tx.ExecContext(s.ctx, `INSERT INTO accounts (name, platform, type, extra) VALUES ($1,$2,$3,$4)`, "saved-group-only", service.PlatformOpenAI, service.AccountTypeOAuth, string(extra))
	s.Require().NoError(err)
	admin := service.NewAdminService(
		nil, nil, nil, nil, s.repo, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	s.Require().ErrorIs(admin.DeleteProxy(s.ctx, p.ID), service.ErrProxyInUse)
	batch, err := admin.BatchDeleteProxies(s.ctx, []int64{p.ID})
	s.Require().NoError(err)
	s.Require().Empty(batch.DeletedIDs)
	s.Require().Len(batch.Skipped, 1)
	s.Require().Equal(p.ID, batch.Skipped[0].ID)
	s.Require().Equal(service.ErrProxyInUse.Error(), batch.Skipped[0].Reason)
	_, err = s.repo.GetByID(s.ctx, p.ID)
	s.Require().NoError(err, "a saved group keeps its proxy configuration intact")
	_, err = s.tx.ExecContext(s.ctx, `UPDATE accounts SET extra = '{"openai_excel_bps_proxy_mode":"session","openai_excel_bps_proxy_ids":[]}'::jsonb WHERE name = $1`, "saved-group-only")
	s.Require().NoError(err)
	s.Require().NoError(admin.DeleteProxy(s.ctx, p.ID), "removing the saved group releases deletion protection")
}

func (s *ProxyRepoSuite) TestOpenAIConfiguredReferencesPreferExplicitMode() {
	newGroup := s.mustCreateProxy(&service.Proxy{Name: "new-group", Protocol: "http", Host: "192.0.2.61", Port: 8080, Status: service.StatusActive})
	legacyGroup := s.mustCreateProxy(&service.Proxy{Name: "legacy-group", Protocol: "http", Host: "192.0.2.62", Port: 8080, Status: service.StatusActive})
	for _, mode := range []string{service.OpenAIProxyModeSingle, service.OpenAIProxyModeRequest} {
		extra, err := json.Marshal(map[string]any{service.OpenAIProxyModeKey: mode, service.OpenAIProxyIDsKey: []int64{newGroup.ID, newGroup.ID}, service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: []int64{legacyGroup.ID}})
		s.Require().NoError(err)
		_, err = s.tx.ExecContext(s.ctx, `INSERT INTO accounts (name, platform, type, extra) VALUES ($1,$2,$3,$4)`, "explicit-"+mode, service.PlatformOpenAI, service.AccountTypeOAuth, string(extra))
		s.Require().NoError(err)
	}
	count, err := s.repo.CountAccountsByProxyID(s.ctx, newGroup.ID)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), count)
	count, err = s.repo.CountAccountsByProxyID(s.ctx, legacyGroup.ID)
	s.Require().NoError(err)
	s.Require().Zero(count, "explicit single/request both override retained legacy group")
	accounts, err := s.repo.ListAccountSummariesByProxyID(s.ctx, newGroup.ID)
	s.Require().NoError(err)
	s.Require().Len(accounts, 1)
	s.Require().Equal("explicit-request", accounts[0].Name)
	counts, err := s.repo.GetAccountCountsForProxies(s.ctx)
	s.Require().NoError(err)
	s.Require().Equal(int64(1), counts[newGroup.ID])
	s.Require().Zero(counts[legacyGroup.ID])
}

func (s *ProxyRepoSuite) TestNonBPSCredentialsIgnoreRetainedGroupsButKeepLegacyProxy() {
	groupOnly := s.mustCreateProxy(&service.Proxy{Name: "non-bps-group-only", Protocol: "http", Host: "192.0.2.41", Port: 8080, Status: service.StatusActive})
	batchOnly := s.mustCreateProxy(&service.Proxy{Name: "non-bps-batch-only", Protocol: "http", Host: "192.0.2.42", Port: 8080, Status: service.StatusActive})
	legacy := s.mustCreateProxy(&service.Proxy{Name: "non-bps-legacy", Protocol: "http", Host: "192.0.2.43", Port: 8080, Status: service.StatusActive})
	extra, err := json.Marshal(map[string]any{"openai_excel_bps": true, service.ExcelBPSProxyModeKey: service.ExcelBPSProxyModeRequest, service.ExcelBPSProxyIDsKey: []int64{groupOnly.ID, batchOnly.ID, legacy.ID, legacy.ID}})
	s.Require().NoError(err)
	credentials := []map[string]any{
		{"auth_mode": " \tAGENTidentity\n"},
		{"auth_mode": "\tPersonalAccessToken \r"},
		{"auth_mode": " PERSONAL_ACCESS_TOKEN "},
		{"auth_mode": "oauth", "openai_auth_mode": " PeRsOnAlAcCeSsToKeN "},
		{"openai_auth_mode": "\u00a0personal_access_token\u3000"},
	}
	for i, credential := range credentials {
		data, marshalErr := json.Marshal(credential)
		s.Require().NoError(marshalErr)
		_, err = s.tx.ExecContext(s.ctx, `INSERT INTO accounts (name, platform, type, proxy_id, credentials, extra) VALUES ($1,$2,$3,$4,$5,$6)`, fmt.Sprintf("non-bps-credentials-%d", i), service.PlatformOpenAI, service.AccountTypeOAuth, legacy.ID, string(data), string(extra))
		s.Require().NoError(err)
	}
	for _, id := range []int64{groupOnly.ID, batchOnly.ID} {
		accounts, listErr := s.repo.ListAccountSummariesByProxyID(s.ctx, id)
		s.Require().NoError(listErr)
		s.Require().Empty(accounts, "non-BPS retained groups must not be configured references")
		count, countErr := s.repo.CountAccountsByProxyID(s.ctx, id)
		s.Require().NoError(countErr)
		s.Require().Zero(count)
	}
	accounts, err := s.repo.ListAccountSummariesByProxyID(s.ctx, legacy.ID)
	s.Require().NoError(err)
	s.Require().Len(accounts, 5, "the traditional field remains one reference per account")
	counts, err := s.repo.GetAccountCountsForProxies(s.ctx)
	s.Require().NoError(err)
	s.Require().Zero(counts[groupOnly.ID])
	s.Require().Zero(counts[batchOnly.ID])
	s.Require().Equal(int64(5), counts[legacy.ID])
	admin := service.NewAdminService(
		nil, nil, nil, nil, s.repo, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	s.Require().NoError(admin.DeleteProxy(s.ctx, groupOnly.ID))
	batch, err := admin.BatchDeleteProxies(s.ctx, []int64{batchOnly.ID, legacy.ID})
	s.Require().NoError(err)
	s.Require().Equal([]int64{batchOnly.ID}, batch.DeletedIDs)
	s.Require().Len(batch.Skipped, 1)
	s.Require().Equal(legacy.ID, batch.Skipped[0].ID)
	s.Require().Equal(service.ErrProxyInUse.Error(), batch.Skipped[0].Reason)
	s.Require().ErrorIs(admin.DeleteProxy(s.ctx, legacy.ID), service.ErrProxyInUse)
}
