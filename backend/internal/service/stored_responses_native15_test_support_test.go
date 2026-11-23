//go:build recovery15native

package service

import "context"

type storedNativeAccountAuthorizationGroups struct {
	GroupRepository
	group *Group
	err   error
	reads int
}

func (r *storedNativeAccountAuthorizationGroups) GetByID(context.Context, int64) (*Group, error) {
	r.reads++
	return r.group, r.err
}

func (r *storedNativeAccountAuthorizationGroups) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	return r.GetByID(ctx, id)
}
