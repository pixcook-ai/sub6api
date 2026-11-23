//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type createSchedulableRepoStub struct {
	AccountRepository
	created Account
}

func (r *createSchedulableRepoStub) Create(_ context.Context, account *Account) error {
	r.created = *account
	account.ID = 42
	return nil
}

func TestCreateAccountPersistsInitialSchedulable(t *testing.T) {
	paused, enabled := false, true
	for _, tc := range []struct {
		name        string
		schedulable *bool
		want        bool
	}{
		{name: "omitted keeps enabled default", want: true},
		{name: "explicit false is paused before persistence", schedulable: &paused, want: false},
		{name: "explicit true stays enabled", schedulable: &enabled, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &createSchedulableRepoStub{}
			svc := &adminServiceImpl{accountRepo: repo}
			account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
				Name:                 "imported-account",
				Platform:             PlatformAnthropic,
				Type:                 AccountTypeAPIKey,
				Credentials:          map[string]any{"api_key": "synthetic-test-key"},
				Schedulable:          tc.schedulable,
				SkipDefaultGroupBind: true,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, repo.created.Schedulable, "initial repository write must use requested scheduling state")
			require.Equal(t, tc.want, account.Schedulable)
			require.Equal(t, StatusActive, account.Status, "pausing scheduling does not disable the account")
		})
	}
}
