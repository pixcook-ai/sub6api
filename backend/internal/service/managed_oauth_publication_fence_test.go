//go:build unit

package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type managedPublicationFenceRepo struct {
	*managedControllerTestRepo
	fences int
}

func (r *managedPublicationFenceRepo) FenceManagedOAuthPublicationIfMatch(_ context.Context, id int64, expected ManagedOAuthState, op string) (bool, error) {
	current, err := managedOAuthStateFromAccount(r.account)
	if err != nil || id != r.account.ID || !reflect.DeepEqual(*current, expected) {
		return false, ErrManagedOAuthConflict
	}
	next := *current
	next.PublicationFenceOperationID = op
	r.account.Extra[ManagedOAuthExtraKey] = next
	r.account.Schedulable = false
	for _, key := range r.keys {
		key.state.Active = false
	}
	r.fences++
	return true, nil
}

func TestManagedOAuthPublicationFencePermanentlyClosesAllPublishers(t *testing.T) {
	a, base, _, identity := managedControllerFixture()
	r := &managedPublicationFenceRepo{managedControllerTestRepo: base}
	s := &adminServiceImpl{accountRepo: r}
	ctx := context.Background()
	installed := managedOAuthRequest(identity, 0, 1)
	_, err := s.InstallManagedOAuthCredentials(ctx, a.ID, installed)
	require.NoError(t, err)
	a.Schedulable = true
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey)
	require.NoError(t, err)
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey+"other")
	require.NoError(t, err)
	op := uuid.NewString()
	input := &ManagedOAuthPublicationFenceInput{OperationID: op, Identity: identity, TokenRevision: 1}
	first, err := s.FenceManagedOAuthPublication(ctx, a.ID, input)
	require.NoError(t, err)
	require.True(t, first.Fenced)
	require.Equal(t, identity, first.Identity)
	require.Equal(t, op, first.OperationID)
	require.False(t, a.Schedulable)
	require.Equal(t, "admin note", a.ErrorMessage)
	for _, key := range r.keys {
		require.False(t, key.state.Active)
	}
	_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey+"new")
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, installed)
	require.ErrorIs(t, err, ErrManagedOAuthConflict, "even the last successful install cannot replay after a fence")
	_, err = s.InstallManagedOAuthCredentials(ctx, a.ID, managedOAuthRequest(identity, 1, 2))
	require.ErrorIs(t, err, ErrManagedOAuthConflict, "administrator/static controller writes must also stay fenced")
	require.False(t, ManagedOAuthSchedulingReady(a))
	_, err = readManagedOAuthCredentials(ctx, r, a)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)

	// A new service instance sees the retained receipt after a lost response.
	s = &adminServiceImpl{accountRepo: r}
	second, err := s.FenceManagedOAuthPublication(ctx, a.ID, input)
	require.NoError(t, err)
	require.Equal(t, first, second)
	readback, err := s.GetManagedOAuthPublicationFence(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, first, readback)
	require.Equal(t, 1, r.fences)
	changed := *input
	changed.OperationID = uuid.NewString()
	_, err = s.FenceManagedOAuthPublication(ctx, a.ID, &changed)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	changed = *input
	changed.TokenRevision++
	_, err = s.FenceManagedOAuthPublication(ctx, a.ID, &changed)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
}
