//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type resourcePublicationKeyRepo struct {
	APIKeyRepository
	key              *APIKey
	err              error
	reads, authReads int
	afterRead        func()
}

func (r *resourcePublicationKeyRepo) GetByKey(context.Context, string) (*APIKey, error) {
	r.reads++
	if r.afterRead != nil {
		r.afterRead()
	}
	return r.key, r.err
}

func (r *resourcePublicationKeyRepo) GetByKeyForAuth(context.Context, string) (*APIKey, error) {
	r.authReads++
	return r.key, r.err
}

type resourcePublicationSubscriptionRepo struct {
	UserSubscriptionRepository
	sub       *UserSubscription
	err       error
	reads     int
	afterRead func()
}

func (r *resourcePublicationSubscriptionRepo) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*UserSubscription, error) {
	r.reads++
	if r.afterRead != nil {
		r.afterRead()
	}
	return r.sub, r.err
}

func TestResourcePublicationKeyBypassesStaleAuthCache(t *testing.T) {
	groupID := int64(9)
	repo := &resourcePublicationKeyRepo{key: &APIKey{
		ID: 1, UserID: 2, Key: "resource-key", GroupID: &groupID, Status: StatusActive,
		User:  &User{ID: 2, Status: StatusActive, Balance: 10},
		Group: &Group{ID: groupID, Status: StatusActive, Platform: PlatformOpenAI, Hydrated: true},
	}}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L1Size: 100, L1TTLSeconds: 60}})
	t.Cleanup(svc.authCacheL1.Close)
	initial, err := svc.GetByKey(context.Background(), "resource-key")
	require.NoError(t, err)
	require.Equal(t, StatusActive, initial.Status)
	svc.authCacheL1.Wait()
	fresh := *repo.key
	fresh.Status = StatusDisabled
	fresh.IPWhitelist = []string{"192.0.2.0/24"}
	repo.key = &fresh
	got, err := svc.GetForFilePublication(context.Background(), "resource-key")
	require.NoError(t, err)
	require.Equal(t, StatusDisabled, got.Status)
	require.NotNil(t, got.CompiledIPWhitelist)
	require.Equal(t, 1, repo.authReads)
	require.Equal(t, 1, repo.reads)
}

func TestResourcePublicationSubscriptionBypassesStaleCache(t *testing.T) {
	now := time.Now()
	repo := &resourcePublicationSubscriptionRepo{sub: &UserSubscription{ID: 3, UserID: 2, GroupID: 9, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)}}
	svc := NewSubscriptionService(nil, repo, nil, nil, &config.Config{SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 100, L1TTLSeconds: 60}})
	t.Cleanup(svc.Stop)
	t.Cleanup(svc.subCacheL1.Close)
	initial, err := svc.GetActiveSubscription(context.Background(), 2, 9)
	require.NoError(t, err)
	require.Equal(t, SubscriptionStatusActive, initial.Status)
	svc.subCacheL1.Wait()
	fresh := *repo.sub
	fresh.Status = SubscriptionStatusSuspended
	repo.sub = &fresh
	got, err := svc.GetForFilePublication(context.Background(), 2, 9)
	require.NoError(t, err)
	require.Equal(t, SubscriptionStatusSuspended, got.Status)
	require.Equal(t, 2, repo.reads)
}

func TestResourcePublicationReadFailsClosedAndPreservesCancellation(t *testing.T) {
	ctx := context.Background()
	var nilKeys *APIKeyService
	var nilSubs *SubscriptionService
	_, err := nilKeys.GetForFilePublication(ctx, "credential")
	require.ErrorIs(t, err, ErrFileAuthorizationUnavailable)
	_, err = nilSubs.GetForFilePublication(ctx, 1, 2)
	require.ErrorIs(t, err, ErrFileAuthorizationUnavailable)
	keys := &resourcePublicationKeyRepo{}
	subs := &resourcePublicationSubscriptionRepo{}
	keySvc := &APIKeyService{apiKeyRepo: keys}
	subSvc := &SubscriptionService{userSubRepo: subs}
	_, err = keySvc.GetForFilePublication(ctx, "credential")
	require.ErrorIs(t, err, ErrFileAuthorizationUnavailable)
	_, err = subSvc.GetForFilePublication(ctx, 1, 2)
	require.ErrorIs(t, err, ErrFileAuthorizationUnavailable)
	keys.err, subs.err = errors.New("database failure"), errors.New("database failure")
	_, err = keySvc.GetForFilePublication(ctx, "credential")
	require.Equal(t, keys.err, err)
	_, err = subSvc.GetForFilePublication(ctx, 1, 2)
	require.Equal(t, subs.err, err)
	before := keys.reads
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = keySvc.GetForFilePublication(cancelled, "credential")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, keys.reads)
	during, cancelDuring := context.WithCancel(ctx)
	defer cancelDuring()
	keys.afterRead = cancelDuring
	_, err = keySvc.GetForFilePublication(during, "credential")
	require.ErrorIs(t, err, context.Canceled)
	before = subs.reads
	_, err = subSvc.GetForFilePublication(cancelled, 1, 2)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, subs.reads)
	duringSub, cancelSub := context.WithCancel(ctx)
	defer cancelSub()
	subs.afterRead = cancelSub
	_, err = subSvc.GetForFilePublication(duringSub, 1, 2)
	require.ErrorIs(t, err, context.Canceled)
}
