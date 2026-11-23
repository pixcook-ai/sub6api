//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const managedControllerTestKey = "managed-controller-key-0123456789-abcdefghijklmnop"

type managedControllerTestKeyRecord struct {
	hash  string
	state ManagedOAuthControllerKeyState
}

// This fake exercises the service contract; repository CAS has separate SQL and PostgreSQL coverage.
type managedControllerTestRepo struct {
	*managedOAuthTestRepo
	keys                 map[string]*managedControllerTestKeyRecord
	puts, revokes, finds int
	lastHash             string
}

func (r *managedControllerTestRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, ErrManagedOAuthInvalid
	}
	return r.managedOAuthTestRepo.GetByID(ctx, id)
}
func (r *managedControllerTestRepo) PutManagedOAuthControllerKey(_ context.Context, id int64, expected ManagedOAuthState, op, hash string) (*ManagedOAuthControllerKeyState, error) {
	r.puts++
	r.lastHash = hash
	current, err := managedOAuthStateFromAccount(r.account)
	if err != nil || id != r.account.ID || !reflect.DeepEqual(*current, expected) {
		return nil, ErrManagedOAuthConflict
	}
	if existing := r.keys[op]; existing != nil {
		if existing.hash != hash || !existing.state.Active || existing.state.AccountID != id || existing.state.ManagedOAuthIdentity != expected.ManagedOAuthIdentity {
			return nil, ErrManagedOAuthConflict
		}
		copy := existing.state
		return &copy, nil
	}
	for _, existing := range r.keys {
		if existing.hash == hash {
			return nil, ErrManagedOAuthConflict
		}
	}
	record := &managedControllerTestKeyRecord{hash: hash, state: ManagedOAuthControllerKeyState{ManagedOAuthIdentity: expected.ManagedOAuthIdentity, AccountID: id, OperationID: op, Active: true}}
	r.keys[op] = record
	copy := record.state
	return &copy, nil
}
func (r *managedControllerTestRepo) RevokeManagedOAuthControllerKey(_ context.Context, id int64, op string) (*ManagedOAuthControllerKeyState, error) {
	r.revokes++
	record := r.keys[op]
	if record == nil || record.state.AccountID != id {
		return nil, ErrManagedOAuthConflict
	}
	record.state.Active = false
	copy := record.state
	return &copy, nil
}
func (r *managedControllerTestRepo) FindManagedOAuthControllerKey(_ context.Context, hash string) (*ManagedOAuthControllerKeyState, error) {
	r.finds++
	for _, record := range r.keys {
		if record.hash == hash {
			copy := record.state
			return &copy, nil
		}
	}
	return nil, ErrManagedOAuthControllerKeyUnknown
}
func managedControllerFixture() (*Account, *managedControllerTestRepo, *adminServiceImpl, ManagedOAuthIdentity) {
	a, base, _, identity := managedOAuthFixture()
	r := &managedControllerTestRepo{managedOAuthTestRepo: base, keys: map[string]*managedControllerTestKeyRecord{}}
	return a, r, &adminServiceImpl{accountRepo: r}, identity
}

func TestManagedOAuthControllerRevisionZeroRegistrationIsHashedAndIdempotent(t *testing.T) {
	a, r, s, identity := managedControllerFixture()
	a.Credentials["model_mapping"] = map[string]any{"model": "upstream-model"}
	before, err := json.Marshal(a)
	require.NoError(t, err)
	ctx := context.Background()
	op := uuid.NewString()
	first, err := s.RegisterManagedOAuthControllerKey(ctx, a.ID, op, managedControllerTestKey)
	require.NoError(t, err)
	require.Equal(t, identity, first.ManagedOAuthIdentity)
	require.Equal(t, a.ID, first.AccountID)
	require.Equal(t, op, first.OperationID)
	require.True(t, first.Active)
	public, err := s.GetManagedOAuthState(ctx, a.ID)
	require.NoError(t, err)
	require.Zero(t, public.TokenRevision)
	require.False(t, public.Initialized)
	require.False(t, public.Ready)
	second, err := s.RegisterManagedOAuthControllerKey(ctx, a.ID, op, managedControllerTestKey)
	require.NoError(t, err)
	require.Equal(t, first, second, "lost-response retry returns the same registration")
	require.Len(t, r.keys, 1)
	sum := sha256.Sum256([]byte(managedControllerTestKey))
	require.Equal(t, hex.EncodeToString(sum[:]), r.lastHash)
	require.Len(t, r.lastHash, 64)
	require.NotEqual(t, managedControllerTestKey, r.lastHash)
	wire, err := json.Marshal(first)
	require.NoError(t, err)
	require.NotContains(t, string(wire), managedControllerTestKey)
	require.NotContains(t, string(wire), r.lastHash)
	require.NotContains(t, string(wire), "key_hash")
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, op, managedControllerTestKey+"different")
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthConflict, "a hash cannot be reused under another operation")
	_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey)
	require.NoError(t, err, "registered revision zero can authenticate before first AT delivery")
	after, err := json.Marshal(a)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "key operations must preserve admin state, credentials and managed state")
	require.Zero(t, r.installs)
	require.Zero(t, r.setErrors)
}

func TestManagedOAuthControllerRevocationIsIdempotentAndCannotResurrectKey(t *testing.T) {
	a, r, s, _ := managedControllerFixture()
	before, err := json.Marshal(a)
	require.NoError(t, err)
	ctx := context.Background()
	op := uuid.NewString()
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, op, managedControllerTestKey)
	require.NoError(t, err)
	_, err = s.RevokeManagedOAuthControllerKey(ctx, a.ID, uuid.NewString())
	require.ErrorIs(t, err, ErrManagedOAuthConflict, "unknown operation cannot claim revocation")
	for i := 0; i < 2; i++ {
		revoked, err := s.RevokeManagedOAuthControllerKey(ctx, a.ID, op)
		require.NoError(t, err)
		require.False(t, revoked.Active)
	}
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, op, managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthInvalid, "revoked must remain distinct from unknown to prevent env fallback")
	_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey+"unknown")
	require.ErrorIs(t, err, ErrManagedOAuthControllerKeyUnknown)
	_, err = s.RevokeManagedOAuthControllerKey(ctx, a.ID+1, op)
	require.ErrorIs(t, err, ErrManagedOAuthConflict)
	after, err := json.Marshal(a)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	require.Len(t, r.keys, 1, "revocation retains its tombstone")
}

func TestManagedOAuthControllerAuthenticationBindsFullCurrentIdentity(t *testing.T) {
	for _, field := range []string{"epoch", "binding", "profile", "backend", "subject", "workspace", "provider", "contract"} {
		t.Run(field, func(t *testing.T) {
			a, _, s, _ := managedControllerFixture()
			ctx := context.Background()
			_, err := s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey)
			require.NoError(t, err)
			state := a.Extra[ManagedOAuthExtraKey].(ManagedOAuthState)
			switch field {
			case "epoch":
				state.IdentityEpoch = "new-epoch"
			case "binding":
				state.BindingRef = "new-binding"
			case "profile":
				state.ProfileRef = "new-profile"
			case "backend":
				state.BackendID = "new-backend"
			case "subject":
				state.Subject = "new-subject"
			case "workspace":
				state.WorkspaceID = "new-workspace"
			case "provider":
				state.Provider = "bps"
			case "contract":
				state.Contract = "other-contract"
			}
			a.Extra[ManagedOAuthExtraKey] = state
			_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey)
			require.ErrorIs(t, err, ErrManagedOAuthInvalid)
		})
	}
}

func TestManagedOAuthControllerNullOrMalformedMarkerFailsClosed(t *testing.T) {
	for _, marker := range []any{nil, "invalid", map[string]any{"contract": ManagedOAuthContract}} {
		a, r, s, _ := managedControllerFixture()
		ctx := context.Background()
		_, err := s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey)
		require.NoError(t, err)
		puts := r.puts
		a.Extra[ManagedOAuthExtraKey] = marker
		_, err = s.RegisterManagedOAuthControllerKey(ctx, a.ID, uuid.NewString(), managedControllerTestKey+"next")
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
		require.Equal(t, puts, r.puts, "invalid marker rejects before key persistence")
		_, err = s.AuthenticateManagedOAuthControllerKey(ctx, managedControllerTestKey)
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	}
}

func TestManagedOAuthControllerAuthenticationRejectsInactiveOrInvalidStoredIdentity(t *testing.T) {
	for _, name := range []string{"inactive", "invalid-account", "invalid-identity"} {
		t.Run(name, func(t *testing.T) {
			a, r, s, _ := managedControllerFixture()
			op := uuid.NewString()
			_, err := s.RegisterManagedOAuthControllerKey(context.Background(), a.ID, op, managedControllerTestKey)
			require.NoError(t, err)
			switch name {
			case "inactive":
				r.keys[op].state.Active = false
			case "invalid-account":
				r.keys[op].state.AccountID = 0
			case "invalid-identity":
				r.keys[op].state.ManagedOAuthIdentity = ManagedOAuthIdentity{}
			}
			reads := r.reads
			_, err = s.AuthenticateManagedOAuthControllerKey(context.Background(), managedControllerTestKey)
			require.ErrorIs(t, err, ErrManagedOAuthInvalid)
			require.Equal(t, reads, r.reads, "untrusted key record cannot trigger an account lookup")
		})
	}
}

func TestManagedOAuthControllerInputValidationAndMissingRepositoryFailClosed(t *testing.T) {
	a, r, s, _ := managedControllerFixture()
	for _, op := range []string{"", "bad", uuid.Nil.String(), "C45CE361-C1D8-44B2-89B3-D0955E17CC88"} {
		_, err := s.RegisterManagedOAuthControllerKey(context.Background(), a.ID, op, managedControllerTestKey)
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
		_, err = s.RevokeManagedOAuthControllerKey(context.Background(), a.ID, op)
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	}
	for _, key := range []string{"", strings.Repeat("x", 31), strings.Repeat("x", 4097), managedControllerTestKey + " ", managedControllerTestKey + "\n", managedControllerTestKey + "中"} {
		_, err := s.RegisterManagedOAuthControllerKey(context.Background(), a.ID, uuid.NewString(), key)
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
		_, err = s.AuthenticateManagedOAuthControllerKey(context.Background(), key)
		require.ErrorIs(t, err, ErrManagedOAuthInvalid)
	}
	require.Zero(t, r.puts)
	require.Zero(t, r.revokes)
	require.Zero(t, r.finds)
	_, _, unavailable, _ := managedOAuthFixture()
	_, err := unavailable.RegisterManagedOAuthControllerKey(context.Background(), a.ID, uuid.NewString(), managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
	_, err = unavailable.RevokeManagedOAuthControllerKey(context.Background(), a.ID, uuid.NewString())
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
	_, err = unavailable.AuthenticateManagedOAuthControllerKey(context.Background(), managedControllerTestKey)
	require.ErrorIs(t, err, ErrManagedOAuthUnavailable)
}
