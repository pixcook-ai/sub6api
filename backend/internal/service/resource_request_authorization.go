package service

import "context"

// GetForFilePublication bypasses authentication caches. The repository loads
// the key together with its current user, allowed groups and group from SQL.
func (s *APIKeyService) GetForFilePublication(ctx context.Context, credential string) (*APIKey, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.apiKeyRepo == nil {
		return nil, ErrFileAuthorizationUnavailable
	}
	if credential == "" || len(credential) > MaxAPIKeyCredentialBytes {
		return nil, ErrAPIKeyNotFound
	}
	key, err := s.apiKeyRepo.GetByKey(ctx, credential)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, ErrFileAuthorizationUnavailable
	}
	s.compileAPIKeyIPRules(key)
	return key, nil
}

// GetForFilePublication bypasses subscription and billing caches.
func (s *SubscriptionService) GetForFilePublication(ctx context.Context, userID, groupID int64) (*UserSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.userSubRepo == nil {
		return nil, ErrFileAuthorizationUnavailable
	}
	sub, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, userID, groupID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, ErrFileAuthorizationUnavailable
	}
	return sub, nil
}
