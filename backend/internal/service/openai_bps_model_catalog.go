package service

import (
	"context"
	"sort"
)

// OpenAIBPSAvailableModels lists the client-facing models of a group by asking BPS
// what each schedulable account may use. An account's model_mapping narrows the
// upstream catalog (client name -> upstream name); an account without a mapping
// exposes the whole catalog. ok is false when no account could be queried, so the
// caller can fall back to the configured list instead of advertising nothing.
func (s *OpenAIGatewayService) OpenAIBPSAvailableModels(ctx context.Context, groupID *int64) ([]string, bool) {
	if s == nil || s.accountRepo == nil {
		return nil, false
	}
	var accounts []Account
	var err error
	if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupID(ctx, *groupID)
	} else {
		accounts, err = s.accountRepo.ListSchedulable(ctx)
	}
	if err != nil {
		return nil, false
	}
	set := map[string]struct{}{}
	queried := false
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAIBPS() || acc.Type != AccountTypeOAuth {
			continue
		}
		ids, fetchErr := s.FetchOpenAIBPSModelIDs(ctx, acc)
		if fetchErr != nil {
			continue
		}
		queried = true
		// The upstream catalog is authoritative. Mapping entries only add aliases
		// whose target the upstream allows; they never narrow the catalog.
		allowed := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			set[id] = struct{}{}
			allowed[id] = struct{}{}
		}
		for client, upstream := range acc.GetModelMapping() {
			if _, ok := allowed[upstream]; ok {
				set[client] = struct{}{}
			}
		}
	}
	if !queried {
		return nil, false
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, true
}
