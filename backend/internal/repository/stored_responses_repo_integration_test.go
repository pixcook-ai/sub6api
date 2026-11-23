//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestStoredResponseRepositoryOwnerAndDurability(t *testing.T) {
	ctx := context.Background()
	uid := time.Now().UnixNano()
	repo := NewStoredResponseRepository(integrationDB)
	limits := service.DefaultStoredResponseConfig().Limits
	record := &service.StoredResponse{PublicID: fmt.Sprintf("resp_local_%032x", uid), UserID: uid, APIKeyID: 11, GroupID: 42, Model: "fixture", ResolvedModel: "fixture", AccountID: 1, Binding: "fixture-v1", Input: []byte(`"hello"`), Parameters: []byte(`{}`), LeaseToken: "fixture-lease-token"}
	require.NoError(t, repo.Reserve(ctx, record, limits, 30*24*time.Hour, time.Minute))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `UPDATE gateway_stored_responses SET created_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, uid)
		_, _ = repo.Sweep(ctx, 100)
	})
	got, err := NewStoredResponseRepository(integrationDB).GetForOwner(ctx, uid, 42, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, "in_progress", got.State)
	require.Equal(t, record.PublicID, got.PublicID)
	_, err = repo.GetForOwner(ctx, uid+1, 42, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
	_, err = repo.GetForOwner(ctx, uid, 43, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
	require.ErrorIs(t, repo.Delete(ctx, uid, 42, record.PublicID), service.ErrStoredResponseConflict)
	t.Run("known minimum capacity", func(t *testing.T) {
		large := *record
		large.PublicID = fmt.Sprintf("resp_local_%032x", uid+1)
		large.Parameters, err = json.Marshal(map[string]string{"instructions": strings.Repeat("i", 600<<10)})
		require.NoError(t, err)
		require.ErrorIs(t, repo.Reserve(ctx, &large, limits, 30*24*time.Hour, time.Minute), service.ErrStoredResponseTooLarge)
		_, err = repo.GetForOwner(ctx, uid, 42, large.PublicID)
		require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
		for _, budget := range []int64{347, 348} {
			bounded := *record
			bounded.PublicID = fmt.Sprintf("resp_local_%032x", uid+budget)
			bounded.Input, bounded.Parameters = []byte(`"x"`), []byte(`{"instructions":"client"}`)
			boundedLimits := limits
			boundedLimits.MaxResponseBytes = budget
			err = repo.Reserve(ctx, &bounded, boundedLimits, 30*24*time.Hour, time.Minute)
			if budget == 347 {
				require.ErrorIs(t, err, service.ErrStoredResponseTooLarge)
			} else {
				require.NoError(t, err)
				got, err := repo.GetForOwner(ctx, uid, 42, bounded.PublicID)
				require.NoError(t, err)
				var public map[string]any
				require.NoError(t, json.Unmarshal(got.PublicResponse, &public))
				require.Equal(t, "client", public["instructions"])
			}
		}
	})
}

func storedResponseFixture(t *testing.T) (service.StoredResponseRepository, func() *service.StoredResponse) {
	t.Helper()
	ctx := context.Background()
	uid := time.Now().UnixNano()
	seq := 0
	repo := NewStoredResponseRepository(integrationDB)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_stored_responses SET created_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE user_id=$1`, uid)
		require.NoError(t, err)
		_, err = repo.Sweep(ctx, 100)
		require.NoError(t, err)
	})
	return repo, func() *service.StoredResponse {
		seq++
		return &service.StoredResponse{PublicID: fmt.Sprintf("resp_local_%016x%016x", uid, seq), UserID: uid, APIKeyID: 11, GroupID: 42, Model: "fixture", ResolvedModel: "fixture", AccountID: 1, Binding: "fixture-v1", Input: []byte(`"hello"`), Parameters: []byte(`{}`), LeaseToken: fmt.Sprintf("fixture-token-%d-%d", uid, seq)}
	}
}

func storedTerminal(t *testing.T, record *service.StoredResponse, state string) service.StoredResponseTerminal {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"id": record.PublicID, "object": "response", "created_at": record.CreatedAt.Unix(), "status": state, "store": true, "model": record.Model, "output": []any{}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 4, "total_tokens": 15}})
	require.NoError(t, err)
	return service.StoredResponseTerminal{State: state, ExecutionState: "terminal", PublicResponse: payload}
}

func TestStoredResponseRepositoryFinalizeAndDeleteAreIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, next := storedResponseFixture(t)
	record := next()
	limits := service.DefaultStoredResponseConfig().Limits
	require.NoError(t, repo.Reserve(ctx, record, limits, 30*24*time.Hour, time.Minute))
	terminal := storedTerminal(t, record, "completed")
	got, err := repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, terminal)
	require.NoError(t, err)
	require.Equal(t, "completed", got.State)
	require.Empty(t, got.LeaseToken)
	repeated, err := NewStoredResponseRepository(integrationDB).Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, terminal)
	require.NoError(t, err)
	require.JSONEq(t, string(got.PublicResponse), string(repeated.PublicResponse))
	_, err = repo.Finalize(ctx, record.PublicID, "wrong-token", record.Version, terminal)
	require.ErrorIs(t, err, service.ErrStoredResponseConflict)
	require.ErrorIs(t, repo.Delete(ctx, record.UserID+1, record.GroupID, record.PublicID), service.ErrStoredResponseNotFound)
	require.NoError(t, repo.Delete(ctx, record.UserID, record.GroupID, record.PublicID))
	require.NoError(t, repo.Delete(ctx, record.UserID, record.GroupID, record.PublicID))
	_, err = repo.GetForOwner(ctx, record.UserID, record.GroupID, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
	var bytes, count int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT rows_count,reserved_bytes FROM gateway_stored_response_budgets WHERE owner_id=$1`, record.UserID).Scan(&count, &bytes))
	require.EqualValues(t, 1, count)
	require.Zero(t, bytes)
	var content []byte
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COALESCE(public_response::text,'') FROM gateway_stored_responses WHERE public_id=$1`, record.PublicID).Scan(&content))
	require.Empty(t, content)
}

func TestStoredResponseRepositoryConcurrentQuotaAndLeaseRecovery(t *testing.T) {
	ctx := context.Background()
	repo, next := storedResponseFixture(t)
	limits := service.DefaultStoredResponseConfig().Limits
	limits.UserMaxRows = 1
	records := []*service.StoredResponse{next(), next()}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, record := range records {
		wg.Add(1)
		go func(record *service.StoredResponse) {
			defer wg.Done()
			errs <- NewStoredResponseRepository(integrationDB).Reserve(ctx, record, limits, 30*24*time.Hour, time.Minute)
		}(record)
	}
	wg.Wait()
	close(errs)
	successful, rejected := 0, 0
	for err := range errs {
		if err == nil {
			successful++
		} else if errors.Is(err, service.ErrStoredResponseQuota) {
			rejected++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, successful)
	require.Equal(t, 1, rejected)
	var record *service.StoredResponse
	for _, candidate := range records {
		if candidate.Version > 0 {
			record = candidate
		}
	}
	require.NotNil(t, record)
	require.NoError(t, repo.Renew(ctx, record.PublicID, record.LeaseToken, record.Version, time.Minute))
	_, err := integrationDB.ExecContext(ctx, `UPDATE gateway_stored_responses SET lease_until=clock_timestamp()-interval '1 second' WHERE public_id=$1`, record.PublicID)
	require.NoError(t, err)
	require.ErrorIs(t, repo.Renew(ctx, record.PublicID, record.LeaseToken, record.Version, time.Minute), service.ErrStoredResponseLease)
	_, err = repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, storedTerminal(t, record, "completed"))
	require.ErrorIs(t, err, service.ErrStoredResponseLease)
	n, err := repo.Sweep(ctx, 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)
	got, err := repo.GetForOwner(ctx, record.UserID, record.GroupID, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, "failed", got.State)
	require.Equal(t, "unknown", got.ExecutionState)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(got.PublicResponse, &obj))
	require.Nil(t, obj["usage"])
	require.Equal(t, "response_interrupted", obj["error"].(map[string]any)["code"])
	require.NoError(t, repo.Delete(ctx, record.UserID, record.GroupID, record.PublicID))
	require.ErrorIs(t, repo.Reserve(ctx, next(), limits, 30*24*time.Hour, time.Minute), service.ErrStoredResponseQuota, "deleted tombstone retains finite row budget")
	_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_stored_responses SET created_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE public_id=$1`, record.PublicID)
	require.NoError(t, err)
	_, err = repo.Sweep(ctx, 100)
	require.NoError(t, err)
	require.NoError(t, repo.Reserve(ctx, next(), limits, 30*24*time.Hour, time.Minute))
}

func TestStoredResponseRepositoryFailedCommitRollsBackContentAndBudget(t *testing.T) {
	ctx := context.Background()
	repo, next := storedResponseFixture(t)
	record := next()
	limits := service.DefaultStoredResponseConfig().Limits
	require.NoError(t, repo.Reserve(ctx, record, limits, 30*24*time.Hour, time.Minute))
	require.NoError(t, ApplyMigrations(ctx, integrationDB), "migration replay preserves the response and reservation")
	function := fmt.Sprintf("stored_response_fail_%d", record.UserID)
	trigger := fmt.Sprintf("stored_response_trigger_%d", record.UserID)
	// Identifiers contain only a fixed prefix and the positive generated fixture ID.
	_, err := integrationDB.ExecContext(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.user_id=%d AND NEW.state='completed' THEN RAISE EXCEPTION 'fixture terminal write failure'; END IF; RETURN NEW; END $$`, function, record.UserID))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON gateway_stored_responses`, trigger))
		_, _ = integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, function))
	})
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON gateway_stored_responses FOR EACH ROW EXECUTE FUNCTION %s()`, trigger, function))
	require.NoError(t, err)
	terminal := storedTerminal(t, record, "completed")
	_, err = repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, terminal)
	require.Error(t, err)
	got, err := repo.GetForOwner(ctx, record.UserID, record.GroupID, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, "in_progress", got.State)
	require.Equal(t, limits.MaxResponseBytes, got.ReservedBytes)
	var reserved int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT reserved_bytes FROM gateway_stored_response_budgets WHERE owner_id=$1`, record.UserID).Scan(&reserved))
	require.Equal(t, limits.MaxResponseBytes, reserved)
	_, err = integrationDB.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER %s ON gateway_stored_responses`, trigger))
	require.NoError(t, err)
	_, err = repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, terminal)
	require.NoError(t, err)
}

func TestStoredResponseRepositoryManagementModelScopesLiveRecordsAndDeletedTombstones(t *testing.T) {
	ctx := context.Background()
	repo, next := storedResponseFixture(t)
	limits := service.DefaultStoredResponseConfig().Limits
	record := next()
	record.Model, record.ResolvedModel = "public-client-model", "private-resolved-model"
	require.NoError(t, repo.Reserve(ctx, record, limits, 30*24*time.Hour, time.Minute))

	model, err := repo.GetManagementModel(ctx, record.UserID, record.GroupID, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, record.Model, model, "authorization must use the original client model")
	for _, tc := range []struct {
		name            string
		userID, groupID int64
		id              string
	}{
		{name: "other user", userID: record.UserID + 1, groupID: record.GroupID, id: record.PublicID},
		{name: "other group", userID: record.UserID, groupID: record.GroupID + 1, id: record.PublicID},
		{name: "invalid user", groupID: record.GroupID, id: record.PublicID},
		{name: "invalid group", userID: record.UserID, id: record.PublicID},
		{name: "unknown response", userID: record.UserID, groupID: record.GroupID, id: next().PublicID},
		{name: "invalid response", userID: record.UserID, groupID: record.GroupID, id: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, err := repo.GetManagementModel(ctx, tc.userID, tc.groupID, tc.id)
			require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
			require.Empty(t, model)
		})
	}

	_, err = repo.Finalize(ctx, record.PublicID, record.LeaseToken, record.Version, storedTerminal(t, record, "completed"))
	require.NoError(t, err)
	require.NoError(t, repo.Delete(ctx, record.UserID, record.GroupID, record.PublicID))
	model, err = NewStoredResponseRepository(integrationDB).GetManagementModel(ctx, record.UserID, record.GroupID, record.PublicID)
	require.NoError(t, err)
	require.Equal(t, record.Model, model, "deleted tombstone retains only the model needed for repeated DELETE authorization")
	_, err = repo.GetForOwner(ctx, record.UserID, record.GroupID, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound, "metadata must not make deleted public content readable")
	require.NoError(t, repo.Delete(ctx, record.UserID, record.GroupID, record.PublicID))
	_, err = repo.GetManagementModel(ctx, record.UserID+1, record.GroupID, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
	_, err = repo.GetManagementModel(ctx, record.UserID, record.GroupID+1, record.PublicID)
	require.ErrorIs(t, err, service.ErrStoredResponseNotFound)

	active := next()
	require.NoError(t, repo.Reserve(ctx, active, limits, 30*24*time.Hour, time.Minute))
	for _, expired := range []*service.StoredResponse{record, active} {
		_, err = integrationDB.ExecContext(ctx, `UPDATE gateway_stored_responses SET created_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE public_id=$1`, expired.PublicID)
		require.NoError(t, err)
		model, err = repo.GetManagementModel(ctx, expired.UserID, expired.GroupID, expired.PublicID)
		require.ErrorIs(t, err, service.ErrStoredResponseNotFound)
		require.Empty(t, model, "expired active records and tombstones both stop authorizing management")
	}
}
