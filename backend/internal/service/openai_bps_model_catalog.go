package service

import (
	"context"
	"sort"
	"strings"
)

// OpenAIBPSAvailableModels lists the client-facing models of a group by asking BPS
// what each schedulable account may use. An account's model_mapping narrows the
// upstream catalog (client name -> upstream name); an account without a mapping
// exposes the whole catalog. If all upstream lookups fail, apply account rules
// to the BPS defaults when the group has configured restrictions.
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
	hasRestrictions := false
	var unavailable []*Account
	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsOpenAIBPS() || acc.Type != AccountTypeOAuth {
			continue
		}
		ids, fetchErr := s.FetchOpenAIBPSModelIDs(ctx, acc)
		if fetchErr != nil {
			unavailable = append(unavailable, acc)
			hasRestrictions = hasRestrictions || len(acc.GetModelMapping()) > 0
			continue
		}
		queried = true
		for _, id := range acc.OpenAIBPSClientModelIDs(ids) {
			set[id] = struct{}{}
		}
	}
	if !queried {
		if !hasRestrictions {
			return nil, false
		}
		defaults := OpenAIBPSDefaultModels()
		for _, acc := range unavailable {
			for _, id := range acc.OpenAIBPSClientModelIDs(defaults) {
				set[id] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, true
}

// OpenAIBPSClientModelIDs resolves enumerable client names against the account
// rules and upstream catalog.
func (a *Account) OpenAIBPSClientModelIDs(upstreamIDs []string) []string {
	mapping := a.GetModelMapping()
	if len(mapping) == 0 {
		return upstreamIDs
	}
	allowed := make(map[string]struct{}, len(upstreamIDs))
	for _, id := range upstreamIDs {
		allowed[id] = struct{}{}
	}
	models := make(map[string]struct{}, len(mapping))
	for _, id := range upstreamIDs {
		if target, matched := resolveRequestedModelInMapping(mapping, id); matched {
			if _, ok := allowed[target]; ok {
				models[id] = struct{}{}
			}
		}
	}
	for client, target := range mapping {
		if strings.TrimSpace(client) == "" || strings.ContainsAny(client, "*?") {
			continue
		}
		if _, ok := allowed[target]; ok {
			models[client] = struct{}{}
		}
	}
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	return ids
}
