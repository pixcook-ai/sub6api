//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type coreOAuthAdminGuardRepo struct {
	AccountRepository
	account                                              *Account
	creates, updates, extraUpdates, bulkUpdates, deletes int
}

func (r *coreOAuthAdminGuardRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}
func (r *coreOAuthAdminGuardRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	return []*Account{r.account}, nil
}
func (r *coreOAuthAdminGuardRepo) Create(_ context.Context, a *Account) error {
	r.creates++
	r.account = a
	a.ID = 41
	return nil
}
func (r *coreOAuthAdminGuardRepo) Update(_ context.Context, a *Account) error {
	r.updates++
	r.account = a
	return nil
}
func (r *coreOAuthAdminGuardRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.extraUpdates++
	return nil
}
func (r *coreOAuthAdminGuardRepo) BulkUpdate(context.Context, []int64, AccountBulkUpdate) (int64, error) {
	r.bulkUpdates++
	return 1, nil
}
func (r *coreOAuthAdminGuardRepo) Delete(context.Context, int64) error {
	r.deletes++
	return nil
}
func coreOAuthAdminGuardAccount() *Account {
	return &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: false, Credentials: map[string]any{"access_token": "synthetic-at"}, Extra: map[string]any{CoreOAuthAccountExtraKey: map[string]any{"contract": CoreOAuthContract}}}
}
func TestCoreOAuthAdminGuardCreateRejectsDeclaredMarkerBeforeWrite(t *testing.T) {
	for _, marker := range []any{nil, "malformed", map[string]any{"contract": CoreOAuthContract}} {
		input := &CreateAccountInput{Name: "ordinary", Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic"}, Extra: map[string]any{CoreOAuthAccountExtraKey: marker}, SkipDefaultGroupBind: true}
		repo := &coreOAuthAdminGuardRepo{}
		_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), input)
		require.ErrorIs(t, err, ErrCoreOAuthConflict)
		require.Zero(t, repo.creates)
		_, err = buildAccountForCreate(input, map[string]any{CoreOAuthAccountExtraKey: marker})
		require.ErrorIs(t, err, ErrCoreOAuthConflict)
	}
}
func TestCoreOAuthAdminGuardUpdateRejectsIdentityMutationBeforeWrite(t *testing.T) {
	proxy := int64(9)
	for name, input := range map[string]*UpdateAccountInput{
		"marker replacement": {Extra: map[string]any{CoreOAuthAccountExtraKey: nil}},
		"marker deletion":    {Extra: map[string]any{}},
		"extra replacement":  {Extra: map[string]any{"quota_daily_limit": 20}},
		"access credential":  {Credentials: map[string]any{"access_token": "synthetic-replacement"}},
		"refresh credential": {Credentials: map[string]any{"refresh_token": "synthetic-never-store"}},
		"empty credentials":  {Credentials: map[string]any{}},
		"account type":       {Type: AccountTypeAPIKey},
		"proxy":              {ProxyID: &proxy},
	} {
		t.Run(name, func(t *testing.T) {
			account := coreOAuthAdminGuardAccount()
			repo := &coreOAuthAdminGuardRepo{account: account}
			_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, input)
			require.ErrorIs(t, err, ErrCoreOAuthConflict)
			require.Zero(t, repo.updates)
			require.Equal(t, AccountTypeOAuth, account.Type)
			require.Nil(t, account.ProxyID)
			require.False(t, account.Schedulable)
		})
	}
}
func TestCoreOAuthAdminGuardUpdateAllowsTuningAndPreservesManualPause(t *testing.T) {
	priority := 7
	account := coreOAuthAdminGuardAccount()
	repo := &coreOAuthAdminGuardRepo{account: account}
	got, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{Name: "renamed", Priority: &priority})
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
	require.Equal(t, priority, got.Priority)
	require.False(t, got.Schedulable)
	require.Equal(t, 1, repo.updates)
	require.True(t, CoreOAuthDeclared(got))
}
func TestCoreOAuthAdminGuardUpdateRejectsForgedMarkerOnOrdinaryAccount(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{}, Extra: map[string]any{}}
	repo := &coreOAuthAdminGuardRepo{account: account}
	_, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 41, &UpdateAccountInput{Extra: map[string]any{CoreOAuthAccountExtraKey: nil}})
	require.ErrorIs(t, err, ErrCoreOAuthConflict)
	require.Zero(t, repo.updates)
}
func TestCoreOAuthAdminGuardExtraRejectsCoreEditsAndForgery(t *testing.T) {
	for _, core := range []bool{false, true} {
		for _, updates := range []map[string]any{{CoreOAuthAccountExtraKey: nil}, {"quota_daily_limit": 20}} {
			account := coreOAuthAdminGuardAccount()
			if !core {
				account.Extra = map[string]any{}
			}
			repo := &coreOAuthAdminGuardRepo{account: account}
			_, forged := updates[CoreOAuthAccountExtraKey]
			err := (&adminServiceImpl{accountRepo: repo}).UpdateAccountExtra(context.Background(), 41, updates)
			if core || forged {
				require.ErrorIs(t, err, ErrCoreOAuthConflict)
				require.Zero(t, repo.extraUpdates)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, repo.extraUpdates)
			}
		}
	}
}
func TestCoreOAuthAdminGuardBulkRejectsIdentityMutationBeforeAnyWrite(t *testing.T) {
	proxy := int64(8)
	for name, input := range map[string]*BulkUpdateAccountsInput{
		"marker":            {Extra: map[string]any{CoreOAuthAccountExtraKey: nil}},
		"extra":             {Extra: map[string]any{"quota_daily_limit": 20}},
		"empty extra":       {Extra: map[string]any{}},
		"credentials":       {Credentials: map[string]any{"refresh_token": "synthetic-never-store"}},
		"empty credentials": {Credentials: map[string]any{}},
		"proxy":             {ProxyID: &proxy},
	} {
		t.Run(name, func(t *testing.T) {
			account := coreOAuthAdminGuardAccount()
			repo := &coreOAuthAdminGuardRepo{account: account}
			input.AccountIDs = []int64{41}
			_, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), input)
			require.ErrorIs(t, err, ErrCoreOAuthConflict)
			require.Zero(t, repo.bulkUpdates)
		})
	}
}
func TestCoreOAuthAdminGuardBulkAllowsExplicitManualPause(t *testing.T) {
	paused := false
	priority := 8
	account := coreOAuthAdminGuardAccount()
	repo := &coreOAuthAdminGuardRepo{account: account}
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{41}, Schedulable: &paused, Priority: &priority})
	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Equal(t, 1, repo.bulkUpdates)
	require.False(t, account.Schedulable)
}

func TestCoreOAuthAdminGuardRejectsSparkShadow(t *testing.T) {
	repo := &coreOAuthAdminGuardRepo{account: coreOAuthAdminGuardAccount()}
	_, err := (&adminServiceImpl{accountRepo: repo}).CreateShadow(context.Background(), 41, ShadowOptions{})
	require.ErrorIs(t, err, ErrCoreOAuthConflict)
	require.Zero(t, repo.creates)
}

func TestCoreOAuthAdminGuardDeleteRejectsCoreRuntimeBeforeWrite(t *testing.T) {
	account := coreOAuthAdminGuardAccount()
	repo := &coreOAuthAdminGuardRepo{account: account}
	err := (&adminServiceImpl{accountRepo: repo}).DeleteAccount(context.Background(), account.ID)
	require.ErrorIs(t, err, ErrCoreOAuthConflict)
	require.Zero(t, repo.deletes)
}
