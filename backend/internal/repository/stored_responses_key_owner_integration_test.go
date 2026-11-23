//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

var _ service.StoredResponseKeyOwnerRepository = (*storedResponseRepository)(nil)

func TestStoredResponseRepositoryKeyOwnerScopeAndTombstone(t *testing.T) {
	ctx := context.Background()
	repository, next := storedResponseFixture(t)
	repo := repository.(service.StoredResponseKeyOwnerRepository)
	record := next()
	require.NoError(t, repository.Reserve(ctx, record, service.DefaultStoredResponseConfig().Limits, 30*24*time.Hour, time.Minute))
	for _, scope := range []struct{ user, key, group int64 }{
		{record.UserID + 1, record.APIKeyID, record.GroupID},
		{record.UserID, record.APIKeyID + 1, record.GroupID},
		{record.UserID, 0, record.GroupID},
		{record.UserID, record.APIKeyID, record.GroupID + 1},
	} {
		_, err := repo.GetForKeyOwner(ctx, scope.user, scope.key, scope.group, record.PublicID)
		require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
		_, err = repo.GetManagementModelForKeyOwner(ctx, scope.user, scope.key, scope.group, record.PublicID)
		require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
		require.ErrorIs(t, repo.DeleteForKeyOwner(ctx, scope.user, scope.key, scope.group, record.PublicID), service.ErrStoredResponseNotFound)
	}
	got, err := repo.GetForKeyOwner(ctx, record.UserID, record.APIKeyID, record.GroupID, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, record.PublicID, got.PublicID)
	_, err = repository.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, storedTerminal(t, record, "completed"))
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, repo.DeleteForKeyOwner(ctx, record.UserID, record.APIKeyID, record.GroupID, record.PublicID))
		model, err := repo.GetManagementModelForKeyOwner(ctx, record.UserID, record.APIKeyID, record.GroupID, record.PublicID)
		require.NoError(t, err)
		require.Equal(t, record.Model, model)
		_, err = repo.GetForKeyOwner(ctx, record.UserID, record.APIKeyID, record.GroupID, record.PublicID)
		require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
		require.ErrorIs(t, repo.DeleteForKeyOwner(ctx, record.UserID, record.APIKeyID+1, record.GroupID, record.PublicID), service.ErrStoredResponseNotFound)
	}
}

func TestStoredResponseRepositoryDifferentTurnAdmissionsKeepKeyOwner(t *testing.T) {
	ctx := context.Background()
	repo, next := storedResponseFixture(t)
	limits := service.DefaultStoredResponseConfig().Limits
	parent := next()
	parent.Binding, parent.RecoveryVersion = "admission-turn-one", 1
	require.NoError(t, repo.Reserve(ctx, parent, limits, 30*24*time.Hour, time.Minute))
	terminal := storedTerminal(t, parent, "completed")
	terminal.Recovery = []byte(`{"version":1,"kind":"text","input":"hello","output":[]}`)
	_, err := repo.Finalize(ctx, parent.PublicID, parent.LeaseToken, parent.Version, terminal)
	require.NoError(t, err)
	child := next()
	child.Binding, child.RecoveryVersion, child.PreviousResponseID = "admission-turn-two", 1, parent.PublicID
	require.NoError(t, repo.Reserve(ctx, child, limits, 30*24*time.Hour, time.Minute))
	terminal = storedTerminal(t, child, "completed")
	terminal.Recovery = []byte(`{"version":1,"kind":"text","input":"hello","output":[]}`)
	_, err = repo.Finalize(ctx, child.PublicID, child.LeaseToken, child.Version, terminal)
	require.NoError(t, err)
	crossKey := next()
	crossKey.APIKeyID++
	crossKey.Binding, crossKey.RecoveryVersion, crossKey.PreviousResponseID = "admission-turn-three", 1, parent.PublicID
	require.ErrorIs(t, repo.Reserve(ctx, crossKey, limits, 30*24*time.Hour, time.Minute), service.ErrStoredResponseNotFound)
}
