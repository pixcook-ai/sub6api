package repository_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/repository"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

const coreOAuthTestRef = "20e0c286-9847-49af-8de1-b7e95af0c4b9"

func coreOAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "core-oauth.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("CREATE TABLE settings (id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, value TEXT NOT NULL, updated_at TIMESTAMP NOT NULL)")
	require.NoError(t, err)
	return db
}

func TestCoreOAuthRepositoryPersistsSealedProfileAcrossInstances(t *testing.T) {
	db := coreOAuthTestDB(t)
	ctx := context.Background()
	first := repository.NewCoreOAuthRepository(db)
	profile := service.CoreOAuthSealedProfile{ProfileRef: coreOAuthTestRef, Version: 1, Ciphertext: "sealed-first-grant"}
	require.NoError(t, first.CreateProfile(ctx, profile))

	restarted := repository.NewCoreOAuthRepository(db)
	actual, err := restarted.ReadProfile(ctx, profile.ProfileRef)
	require.NoError(t, err)
	require.Equal(t, &profile, actual)
	listed, err := restarted.ListProfiles(ctx)
	require.NoError(t, err)
	require.Equal(t, []service.CoreOAuthSealedProfile{profile}, listed)
}

func TestCoreOAuthRepositoryCASAllowsOneWriterAndRejectsLateVersion(t *testing.T) {
	db := coreOAuthTestDB(t)
	ctx := context.Background()
	repo := repository.NewCoreOAuthRepository(db)
	require.NoError(t, repo.CreateProfile(ctx, service.CoreOAuthSealedProfile{ProfileRef: coreOAuthTestRef, Version: 1, Ciphertext: "sealed-first-grant"}))

	type outcome struct {
		won        bool
		err        error
		ciphertext string
	}
	results := make(chan outcome, 16)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			peer := repository.NewCoreOAuthRepository(db)
			next := service.CoreOAuthSealedProfile{ProfileRef: coreOAuthTestRef, Version: 2, Ciphertext: fmt.Sprintf("sealed-winner-%02d", index)}
			<-start
			won, err := peer.CompareAndSwapProfile(ctx, coreOAuthTestRef, 1, next)
			results <- outcome{won: won, err: err, ciphertext: next.Ciphertext}
		}(i)
	}
	close(start)
	wait.Wait()
	close(results)
	count, winner := 0, ""
	for result := range results {
		require.NoError(t, result.err)
		if result.won {
			count++
			winner = result.ciphertext
		}
	}
	require.Equal(t, 1, count)
	actual, err := repo.ReadProfile(ctx, coreOAuthTestRef)
	require.NoError(t, err)
	require.Equal(t, int64(2), actual.Version)
	require.Equal(t, winner, actual.Ciphertext)

	late := service.CoreOAuthSealedProfile{ProfileRef: coreOAuthTestRef, Version: 2, Ciphertext: "sealed-late-grant"}
	won, err := repo.CompareAndSwapProfile(ctx, coreOAuthTestRef, 1, late)
	require.NoError(t, err)
	require.False(t, won)
	after, err := repo.ReadProfile(ctx, coreOAuthTestRef)
	require.NoError(t, err)
	require.Equal(t, actual, after)
}

func TestCoreOAuthRepositoryRejectsDuplicateAndMalformedEnvelope(t *testing.T) {
	db := coreOAuthTestDB(t)
	ctx := context.Background()
	repo := repository.NewCoreOAuthRepository(db)
	profile := service.CoreOAuthSealedProfile{ProfileRef: coreOAuthTestRef, Version: 1, Ciphertext: "sealed-first-grant"}
	require.NoError(t, repo.CreateProfile(ctx, profile))
	require.ErrorIs(t, repo.CreateProfile(ctx, profile), service.ErrCoreOAuthConflict)
	_, err := db.ExecContext(ctx, "INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, CURRENT_TIMESTAMP)", "core_oauth_profile:6efc2d20-04e9-44ea-bf85-a2a02e8fb4b9", `{"version":1,"ciphertext":"valid","extra":true}`)
	require.NoError(t, err)
	_, err = repo.ReadProfile(ctx, "6efc2d20-04e9-44ea-bf85-a2a02e8fb4b9")
	require.ErrorIs(t, err, service.ErrCoreOAuthStorage)
}
