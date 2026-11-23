//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCoreOAuthRepositoryBulkReservedExtraCannotForgeOwnership(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "core-bulk-reserved-extra"})

	for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey} {
		for name, value := range map[string]any{"nil": nil, "malformed": "malformed"} {
			t.Run(key+"/"+name, func(t *testing.T) {
				changed, err := repo.BulkUpdate(ctx, []int64{account.ID}, service.AccountBulkUpdate{Extra: map[string]any{key: value}})
				require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
				require.Zero(t, changed)
				current, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				require.NotContains(t, current.Extra, key)
			})
		}
	}
}

func TestCoreOAuthRepositoryBulkResumePreservesMixedBatch(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	core := mustCreateAccount(t, client, &service.Account{
		Name:  "core-bulk-pause",
		Extra: map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract}},
	})
	ordinary := mustCreateAccount(t, client, &service.Account{Name: "ordinary-bulk-pause"})
	ids := []int64{ordinary.ID, core.ID}
	assertSchedulable := func(id int64, want bool) {
		t.Helper()
		account, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, want, account.Schedulable)
	}

	pause := false
	changed, err := repo.BulkUpdate(ctx, ids, service.AccountBulkUpdate{Schedulable: &pause})
	require.NoError(t, err)
	require.EqualValues(t, 2, changed)
	assertSchedulable(core.ID, false)
	assertSchedulable(ordinary.ID, false)

	resume := true
	name := "must-not-be-written"
	changed, err = repo.BulkUpdate(ctx, ids, service.AccountBulkUpdate{Schedulable: &resume, Name: &name})
	require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
	require.Zero(t, changed)
	assertSchedulable(core.ID, false)
	assertSchedulable(ordinary.ID, false)
	for _, original := range []*service.Account{core, ordinary} {
		current, err := repo.GetByID(ctx, original.ID)
		require.NoError(t, err)
		require.Equal(t, original.Name, current.Name)
	}

	changed, err = repo.BulkUpdate(ctx, []int64{ordinary.ID}, service.AccountBulkUpdate{Schedulable: &resume})
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	assertSchedulable(ordinary.ID, true)
	assertSchedulable(core.ID, false)
}

func TestCoreOAuthRepositoryEditPreservesCurrentManualPause(t *testing.T) {
	for _, billing := range []bool{false, true} {
		t.Run(map[bool]string{false: "Update", true: "UpdateWithAccountBillingSettings"}[billing], func(t *testing.T) {
			for _, core := range []bool{false, true} {
				t.Run(map[bool]string{false: "ordinary can resume", true: "core remains paused"}[core], func(t *testing.T) {
					ctx := context.Background()
					tx := testEntTx(t)
					repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
					account := &service.Account{Name: "stale-edit-before-pause", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
					if core {
						account.Extra = map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract}}
					}
					account = mustCreateAccount(t, tx.Client(), account)
					require.True(t, account.Schedulable)
					require.NoError(t, repo.SetSchedulable(ctx, account.ID, false))
					account.Name = "stale-edit-saved"
					var err error
					if billing {
						err = repo.UpdateWithAccountBillingSettings(ctx, account, nil, nil, nil)
					} else {
						err = repo.Update(ctx, account)
					}
					require.NoError(t, err)
					current, err := repo.GetByID(ctx, account.ID)
					require.NoError(t, err)
					require.Equal(t, "stale-edit-saved", current.Name)
					require.Equal(t, !core, current.Schedulable)
					require.Equal(t, !core, account.Schedulable)
				})
			}
		})
	}
}

func TestCoreOAuthRepositorySingleResumeRejectsCoreAndRuntimeShell(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey} {
		t.Run(key, func(t *testing.T) {
			account := mustCreateAccount(t, tx.Client(), &service.Account{
				Name: "single-resume-" + key, Extra: map[string]any{key: nil},
			})
			require.NoError(t, repo.SetSchedulable(ctx, account.ID, false))
			require.ErrorIs(t, repo.SetSchedulable(ctx, account.ID, true), service.ErrCoreOAuthConflict)
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.False(t, current.Schedulable)
		})
	}
	ordinary := mustCreateAccount(t, tx.Client(), &service.Account{Name: "single-resume-ordinary"})
	require.NoError(t, repo.SetSchedulable(ctx, ordinary.ID, false))
	require.NoError(t, repo.SetSchedulable(ctx, ordinary.ID, true))
	current, err := repo.GetByID(ctx, ordinary.ID)
	require.NoError(t, err)
	require.True(t, current.Schedulable)
}

func TestCoreOAuthRepositoryResumeIgnoresDeletedOwnershipAndMissingTargets(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	deleted := mustCreateAccount(t, tx.Client(), &service.Account{
		Name: "deleted-core-resume", Extra: map[string]any{service.CoreOAuthAccountExtraKey: nil},
	})
	require.NoError(t, tx.Client().Account.UpdateOneID(deleted.ID).SetDeletedAt(time.Now()).Exec(ctx))
	for _, id := range []int64{-1, deleted.ID} {
		require.ErrorIs(t, repo.SetSchedulable(ctx, id, true), service.ErrAccountNotFound)
	}
	resume := true
	changed, err := repo.BulkUpdate(ctx, []int64{-1, deleted.ID}, service.AccountBulkUpdate{Schedulable: &resume})
	require.NoError(t, err)
	require.Zero(t, changed)
	ordinary := mustCreateAccount(t, tx.Client(), &service.Account{Name: "resume-with-missing-target"})
	require.NoError(t, repo.SetSchedulable(ctx, ordinary.ID, false))
	changed, err = repo.BulkUpdate(ctx, []int64{ordinary.ID, -1, deleted.ID}, service.AccountBulkUpdate{Schedulable: &resume})
	require.NoError(t, err)
	require.EqualValues(t, 1, changed)
	current, err := repo.GetByID(ctx, ordinary.ID)
	require.NoError(t, err)
	require.True(t, current.Schedulable)
}

// The install fixture changes the row between a caller's read and its repository
// write, reproducing the dangerous post-install ordering deterministically.
func TestCoreOAuthRepositoryStaleEditAfterInstallPreservesCurrentOwnership(t *testing.T) {
	for _, billing := range []bool{false, true} {
		for _, shell := range []bool{false, true} {
			t.Run(map[bool]string{false: "Update/", true: "UpdateWithBilling/"}[billing]+map[bool]string{false: "ordinary snapshot", true: "shell snapshot"}[shell], func(t *testing.T) {
				ctx := context.Background()
				tx := testEntTx(t)
				repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
				account := &service.Account{Name: "before-install", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic-worker-key"}}
				if shell {
					account.Extra = map[string]any{service.CoreOAuthRuntimeShellExtraKey: map[string]any{"provider": service.CoreOAuthProviderExcel, "group_id": 1}}
				}
				account = mustCreateAccount(t, tx.Client(), account)
				require.NoError(t, tx.Client().Account.UpdateOneID(account.ID).SetSchedulable(false).Exec(ctx))
				stale, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				installedExtra := map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract, "token_revision": 2}, service.ManagedDeploymentAccountExtraKey: map[string]any{"binding_ref": "installed"}}
				installedCredentials := map[string]any{"api_key": "synthetic-installed-key", "core_oauth_control_key": "synthetic-control-key"}
				require.NoError(t, tx.Client().Account.UpdateOneID(account.ID).SetExtra(installedExtra).SetCredentials(installedCredentials).SetSchedulable(true).Exec(ctx))
				installed, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				stale.Name = "stale-edit-must-not-win"
				if billing {
					err = repo.UpdateWithAccountBillingSettings(ctx, stale, nil, nil, nil)
				} else {
					err = repo.Update(ctx, stale)
				}
				require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
				current, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				require.Equal(t, installed.Credentials, current.Credentials)
				require.Equal(t, installed.Extra, current.Extra)
				require.Equal(t, installed.Name, current.Name)
				require.True(t, current.Schedulable)
			})
		}
	}
}

func TestCoreOAuthRepositoryBulkInstalledOwnershipRejectsIdentityMutationAtomically(t *testing.T) {
	clearProxy := int64(0)
	for _, tc := range []struct {
		name    string
		updates service.AccountBulkUpdate
	}{
		{"credentials", service.AccountBulkUpdate{Credentials: map[string]any{"api_key": "synthetic-stale-key"}}},
		{"extra", service.AccountBulkUpdate{Extra: map[string]any{"untrusted": true}}},
		{"proxy", service.AccountBulkUpdate{ProxyID: &clearProxy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			core := mustCreateAccount(t, tx.Client(), &service.Account{Name: "installed-core", Extra: map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract}}, Credentials: map[string]any{"api_key": "synthetic-core-key"}})
			ordinary := mustCreateAccount(t, tx.Client(), &service.Account{Name: "ordinary", Credentials: map[string]any{"api_key": "synthetic-ordinary-key"}})
			before := make([]*service.Account, 0, 2)
			for _, id := range []int64{ordinary.ID, core.ID} {
				account, err := repo.GetByID(ctx, id)
				require.NoError(t, err)
				before = append(before, account)
			}
			name := "must-not-be-written"
			tc.updates.Name = &name
			changed, err := repo.BulkUpdate(ctx, []int64{ordinary.ID, core.ID}, tc.updates)
			require.ErrorIs(t, err, service.ErrCoreOAuthConflict)
			require.Zero(t, changed)
			for _, original := range before {
				current, err := repo.GetByID(ctx, original.ID)
				require.NoError(t, err)
				require.Equal(t, original.Name, current.Name)
				require.Equal(t, original.Credentials, current.Credentials)
				require.Equal(t, original.Extra, current.Extra)
				require.Equal(t, original.ProxyID, current.ProxyID)
			}
			// Core ownership must not block independent operator tuning.
			safeName := "safe-tuning"
			concurrency, priority := 3, 7
			changed, err = repo.BulkUpdate(ctx, []int64{ordinary.ID, core.ID}, service.AccountBulkUpdate{Name: &safeName, Concurrency: &concurrency, Priority: &priority})
			require.NoError(t, err)
			require.EqualValues(t, 2, changed)
			for _, original := range before {
				current, err := repo.GetByID(ctx, original.ID)
				require.NoError(t, err)
				require.Equal(t, safeName, current.Name)
				require.Equal(t, concurrency, current.Concurrency)
				require.Equal(t, priority, current.Priority)
				require.Equal(t, original.Credentials, current.Credentials)
				require.Equal(t, original.Extra, current.Extra)
			}
		})
	}
}

func TestCoreOAuthRepositoryDirectCredentialsPreserveInstalledOwnership(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for _, tc := range []struct {
		key  string
		want error
	}{
		{service.CoreOAuthAccountExtraKey, service.ErrCoreOAuthConflict},
		{service.CoreOAuthRuntimeShellExtraKey, service.ErrCoreOAuthConflict},
		{service.ManagedOAuthExtraKey, service.ErrManagedOAuthConflict},
	} {
		t.Run(tc.key, func(t *testing.T) {
			account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "direct-credentials-" + tc.key, Extra: map[string]any{tc.key: nil}, Credentials: map[string]any{"access_token": "synthetic-installed-at"}})
			require.NoError(t, repo.SetSchedulable(ctx, account.ID, false))
			before, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.ErrorIs(t, repo.UpdateCredentials(ctx, account.ID, map[string]any{"access_token": "synthetic-stale-at"}), tc.want)
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, before.Credentials, current.Credentials)
			require.Equal(t, before.Extra, current.Extra)
			require.False(t, current.Schedulable)
		})
	}
	ordinary := mustCreateAccount(t, tx.Client(), &service.Account{Name: "direct-credentials-ordinary"})
	credentials := map[string]any{"access_token": "synthetic-new-at"}
	require.NoError(t, repo.UpdateCredentials(ctx, ordinary.ID, credentials))
	current, err := repo.GetByID(ctx, ordinary.ID)
	require.NoError(t, err)
	require.Equal(t, credentials, current.Credentials)
	deleted := mustCreateAccount(t, tx.Client(), &service.Account{Name: "direct-credentials-deleted", Extra: map[string]any{service.CoreOAuthAccountExtraKey: nil}})
	require.NoError(t, tx.Client().Account.UpdateOneID(deleted.ID).SetDeletedAt(time.Now()).Exec(ctx))
	for _, id := range []int64{-1, deleted.ID} {
		require.ErrorIs(t, repo.UpdateCredentials(ctx, id, credentials), service.ErrAccountNotFound)
	}
}

func TestCoreOAuthRepositoryReservedExtraPreservesOwnershipAndObservationUpdates(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	for _, core := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "core"}[core], func(t *testing.T) {
			input := &service.Account{Name: "extra-ownership-observations"}
			if core {
				input.Extra = map[string]any{service.CoreOAuthAccountExtraKey: map[string]any{"contract": service.CoreOAuthContract}, service.ManagedDeploymentAccountExtraKey: map[string]any{"binding_ref": "installed"}}
			}
			account := mustCreateAccount(t, tx.Client(), input)
			before, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey, service.ManagedDeploymentAccountExtraKey} {
				require.ErrorIs(t, repo.UpdateExtra(ctx, account.ID, map[string]any{key: nil}), service.ErrCoreOAuthConflict)
				current, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				require.Equal(t, before.Extra, current.Extra)
				require.Equal(t, before.Credentials, current.Credentials)
			}
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, map[string]any{"passive_usage_observed": true}))
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, true, current.Extra["passive_usage_observed"])
			require.Equal(t, before.Credentials, current.Credentials)
			for _, key := range []string{service.CoreOAuthAccountExtraKey, service.CoreOAuthRuntimeShellExtraKey, service.ManagedDeploymentAccountExtraKey} {
				require.Equal(t, before.Extra[key], current.Extra[key])
			}
		})
	}
}

func TestCoreOAuthRepositorySchedulingDoesNotMutateDeletedOrdinaryRows(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "resume"}[enabled], func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			account := mustCreateAccount(t, tx.Client(), &service.Account{Name: "deleted-ordinary-scheduling"})
			require.NoError(t, tx.Client().Account.UpdateOneID(account.ID).SetSchedulable(!enabled).SetDeletedAt(time.Now()).Exec(ctx))
			err := repo.SetSchedulable(ctx, account.ID, enabled)
			if enabled {
				require.ErrorIs(t, err, service.ErrAccountNotFound)
			} else {
				require.NoError(t, err)
			}
			// Restore only this disposable fixture to observe that the deleted row did
			// not change; the repository must never resurrect or alter a deleted row.
			require.NoError(t, tx.Client().Account.UpdateOneID(account.ID).ClearDeletedAt().Exec(ctx))
			current, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, !enabled, current.Schedulable)
		})
	}
}

func TestCoreOAuthRepositoryConcurrentInstallFencesStaleWriters(t *testing.T) {
	for _, mode := range []string{"Update", "Bulk"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			client := testEntClient(t)
			repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
			create := func(name string) int64 {
				t.Helper()
				account, err := client.Account.Create().
					SetName(name).SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeAPIKey).
					SetStatus(service.StatusActive).SetConcurrency(1).SetPriority(1).SetSchedulable(true).
					SetCredentials(map[string]any{"api_key": "synthetic-old-key"}).
					SetExtra(map[string]any{"fixture": "before-install"}).Save(ctx)
				require.NoError(t, err)
				return account.ID
			}
			var targetID, ordinaryID int64
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cleanupCancel()
				_, err := integrationDB.ExecContext(cleanupCtx, "DELETE FROM scheduler_outbox WHERE account_id IN ($1,$2) OR payload @> jsonb_build_object('account_ids',jsonb_build_array($1::bigint,$2::bigint))", targetID, ordinaryID)
				if err != nil {
					t.Errorf("clean scheduler fixture: %v", err)
				}
				_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM account_groups WHERE account_id IN ($1,$2)", targetID, ordinaryID)
				if err != nil {
					t.Errorf("clean group fixture: %v", err)
				}
				_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM accounts WHERE id IN ($1,$2)", targetID, ordinaryID)
				if err != nil {
					t.Errorf("clean account fixture: %v", err)
				}
			})
			targetID = create("core-concurrent-install-target")
			ordinaryID = create("core-concurrent-install-ordinary")
			stale, err := repo.GetByID(ctx, targetID)
			require.NoError(t, err)
			targetName := stale.Name
			ordinaryBefore, err := repo.GetByID(ctx, ordinaryID)
			require.NoError(t, err)

			// Connection A keeps the new runtime identity and manual pause uncommitted.
			// The ordinary writer has already loaded its old snapshot on the same pool.
			installer, err := integrationDB.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = installer.Rollback() }()
			var installerPID int
			require.NoError(t, installer.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&installerPID))
			var lockedID int64
			require.NoError(t, installer.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE", targetID).Scan(&lockedID))
			require.Equal(t, targetID, lockedID)
			installedExtra := map[string]any{
				"openai_long_context_billing_enabled":    false,
				service.CoreOAuthAccountExtraKey:         map[string]any{"contract": service.CoreOAuthContract, "token_revision": 2},
				service.ManagedDeploymentAccountExtraKey: map[string]any{"binding_ref": "synthetic-installed-binding"},
			}
			installedCredentials := map[string]any{"api_key": "synthetic-installed-key", "core_oauth_control_key": "synthetic-installed-control"}
			extraJSON, err := json.Marshal(installedExtra)
			require.NoError(t, err)
			credentialsJSON, err := json.Marshal(installedCredentials)
			require.NoError(t, err)
			_, err = installer.ExecContext(ctx, "UPDATE accounts SET extra=$1::jsonb,credentials=$2::jsonb,schedulable=FALSE WHERE id=$3", string(extraJSON), string(credentialsJSON), targetID)
			require.NoError(t, err)

			type writeResult struct {
				changed int64
				err     error
			}
			results := make(chan writeResult, 1)
			exited := make(chan struct{})
			// Register cleanup before starting B. On every failure path release A before
			// waiting for B, and cancel the writer even if the lock observation fails.
			defer func() {
				_ = installer.Rollback()
				cancel()
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					t.Error("stale writer did not exit after releasing installer and cancellation")
				}
			}()
			go func() {
				defer close(exited)
				if mode == "Update" {
					stale.Name = "stale-update-must-not-commit"
					results <- writeResult{err: repo.Update(ctx, stale)}
					return
				}
				name := "stale-bulk-must-not-commit"
				changed, err := repo.BulkUpdate(ctx, []int64{targetID, ordinaryID}, service.AccountBulkUpdate{
					Name: &name, Credentials: map[string]any{"api_key": "synthetic-stale-bulk-key"},
				})
				results <- writeResult{changed: changed, err: err}
			}()

			// An observer connection proves that B is actually blocked by A, rather
			// than inferring the ordering from a sleep or a goroutine-start signal.
			waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
			defer waitCancel()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			writerPID := 0
			for writerPID == 0 {
				select {
				case result := <-results:
					t.Fatalf("stale writer completed before waiting on installer: changed=%d err=%v", result.changed, result.err)
				case <-waitCtx.Done():
					t.Fatalf("stale writer did not enter an observable PostgreSQL lock wait: %v", waitCtx.Err())
				case <-ticker.C:
					err := integrationDB.QueryRowContext(waitCtx, `
      SELECT COALESCE(MIN(pid),0) FROM pg_stat_activity
      WHERE pid <> $1 AND wait_event_type='Lock'
        AND $1::integer = ANY(pg_blocking_pids(pid))
     `, installerPID).Scan(&writerPID)
					require.NoError(t, err)
				}
			}
			t.Logf("observed PostgreSQL writer pid=%d blocked by installer pid=%d", writerPID, installerPID)
			require.NoError(t, installer.Commit())
			select {
			case result := <-results:
				require.ErrorIs(t, result.err, service.ErrCoreOAuthConflict)
				require.Zero(t, result.changed)
			case <-ctx.Done():
				t.Fatalf("stale writer did not return after installer committed: %v", ctx.Err())
			}

			current, err := repo.GetByID(ctx, targetID)
			require.NoError(t, err)
			require.Equal(t, targetName, current.Name)
			require.Equal(t, installedCredentials, current.Credentials)
			currentExtraJSON, err := json.Marshal(current.Extra)
			require.NoError(t, err)
			require.JSONEq(t, string(extraJSON), string(currentExtraJSON))
			require.False(t, current.Schedulable, "stale writer must preserve the installed manual pause")
			ordinaryAfter, err := repo.GetByID(ctx, ordinaryID)
			require.NoError(t, err)
			require.Equal(t, ordinaryBefore, ordinaryAfter, "a rejected mixed batch must leave the ordinary account unchanged")
		})
	}
}
