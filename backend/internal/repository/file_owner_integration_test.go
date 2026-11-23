//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func localFileFixture(t *testing.T) (*fileRepository, func(int64, int64) *service.StoredFile) {
	t.Helper()
	ctx := context.Background()
	uid := time.Now().UnixNano()
	sequence := 0
	r := NewFileRepository(integrationDB).(*fileRepository)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, `DELETE FROM gateway_files WHERE user_id=$1`, uid)
		require.NoError(t, err)
	})
	return r, func(key, group int64) *service.StoredFile {
		sequence++
		id := fmt.Sprintf("%016x%016x", uid, sequence)
		return &service.StoredFile{PublicID: "file-" + id, StorageKey: id, UserID: uid, APIKeyID: key, GroupID: group, Filename: "private.txt", MIME: "text/plain", Purpose: "user_data", UploadToken: "token-" + id, ReservedBytes: 6}
	}
}

func readyLocalFile(t *testing.T, r *fileRepository, f *service.StoredFile) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, r.ReserveUpload(ctx, f, math.MaxInt64, math.MaxInt64, time.Minute, time.Hour, 100, 10000))
	_, err := r.CommitUploadMetadata(ctx, f.ID, f.UploadToken, 6, fmt.Sprintf("%064x", 1), "user_data", f.Filename, f.MIME, nil)
	require.NoError(t, err)
}

func TestFileRepositoryFullKeyOwnerPaginationAndDelete(t *testing.T) {
	ctx := context.Background()
	r, next := localFileFixture(t)
	first, second, otherKey, otherGroup := next(11, 42), next(11, 42), next(12, 42), next(11, 43)
	for _, f := range []*service.StoredFile{first, otherKey, otherGroup, second} {
		readyLocalFile(t, r, f)
	}
	owner := service.FileOwner{UserID: first.UserID, APIKeyID: 11, GroupID: 42}
	page, err := r.ListVisibleFiles(ctx, owner, service.FileListQuery{Limit: 1, Order: "asc"})
	require.NoError(t, err)
	require.True(t, page.HasMore)
	require.Len(t, page.Files, 1)
	require.Equal(t, first.PublicID, page.Files[0].PublicID)
	page, err = r.ListVisibleFiles(ctx, owner, service.FileListQuery{Limit: 1, Order: "asc", After: first.PublicID})
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Len(t, page.Files, 1)
	require.Equal(t, second.PublicID, page.Files[0].PublicID)
	for _, foreign := range []*service.StoredFile{otherKey, otherGroup} {
		_, err = r.ListVisibleFiles(ctx, owner, service.FileListQuery{Limit: 1, Order: "asc", After: foreign.PublicID})
		require.ErrorIs(t, err, service.ErrFileInvalidCursor)
		require.ErrorIs(t, r.DeleteLocalFile(ctx, owner, foreign.PublicID, time.Minute), service.ErrFileNotFound)
	}
	wrongUser := owner
	wrongUser.UserID++
	page, err = r.ListVisibleFiles(ctx, wrongUser, service.FileListQuery{Limit: 10, Order: "asc"})
	require.NoError(t, err)
	require.Empty(t, page.Files)
	_, _, err = r.AcquireReadGrant(ctx, owner.UserID, owner.GroupID, first.PublicID, "reader", time.Minute, time.Hour)
	require.NoError(t, err)
	require.NoError(t, r.DeleteLocalFile(ctx, owner, first.PublicID, time.Hour))
	require.NoError(t, r.DeleteLocalFile(ctx, owner, first.PublicID, time.Hour))
	_, err = r.GetForOwner(ctx, owner.UserID, owner.GroupID, first.PublicID)
	require.ErrorIs(t, err, service.ErrFileNotFound)
	_, err = r.ClaimLocalPurge(ctx, first.ID, "cleanup", time.Minute)
	require.ErrorIs(t, err, service.ErrFileLeaseConflict)
	require.NoError(t, r.ReleaseRead(ctx, first.ID, "reader"))
	ticket, err := r.ClaimLocalPurge(ctx, first.ID, "cleanup", time.Minute)
	require.NoError(t, err)
	require.NoError(t, r.ConfirmLocalPurge(ctx, *ticket))
	var reserved int64
	var purged bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT reserved_bytes,local_purged_at IS NOT NULL FROM gateway_files WHERE id=$1`, first.ID).Scan(&reserved, &purged))
	require.Zero(t, reserved)
	require.True(t, purged)
}

func TestFileRepositoryConcurrentReservationsHaveSingleWinner(t *testing.T) {
	for _, quotaBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrency", true: "quota"}[quotaBound], func(t *testing.T) {
			ctx := context.Background()
			r, next := localFileFixture(t)
			files := []*service.StoredFile{next(11, 42), next(12, 42)}
			results := make(chan error, 2)
			var wg sync.WaitGroup
			quota := int64(12)
			limit := 1
			expected := service.ErrFileUploadConcurrency
			if quotaBound {
				quota = 6
				limit = 2
				expected = service.ErrFileQuotaExceeded
			}
			for _, f := range files {
				wg.Add(1)
				go func(f *service.StoredFile) {
					defer wg.Done()
					results <- r.ReserveUpload(ctx, f, quota, math.MaxInt64, time.Minute, time.Hour, limit, 10000)
				}(f)
			}
			wg.Wait()
			close(results)
			wins, rejected := 0, 0
			for err := range results {
				if err == nil {
					wins++
				} else if errors.Is(err, expected) {
					rejected++
				} else {
					require.NoError(t, err)
				}
			}
			require.Equal(t, 1, wins)
			require.Equal(t, 1, rejected)
		})
	}
}
