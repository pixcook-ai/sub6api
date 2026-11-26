package dto

import (
	"encoding/json"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogFromServiceAdmin_RouteDegradedTriStateAdminOnly(t *testing.T) {
	t.Parallel()

	healthy, degraded := false, true
	for _, tc := range []struct {
		name     string
		value    *bool
		wantJSON string
	}{
		{name: "nil_tibo_not_applied", value: nil},
		{name: "false_healthy_route", value: &healthy, wantJSON: `"route_degraded":false`},
		{name: "true_http_fallback", value: &degraded, wantJSON: `"route_degraded":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := &service.UsageLog{
				RequestID:     "req_tibo_route",
				Model:         "gpt-5.4",
				RouteDegraded: tc.value,
			}

			adminDTO := UsageLogFromServiceAdmin(log)
			require.Equal(t, tc.value, adminDTO.RouteDegraded)

			adminJSON, err := json.Marshal(adminDTO)
			require.NoError(t, err)
			if tc.wantJSON == "" {
				require.NotContains(t, string(adminJSON), `"route_degraded"`)
			} else {
				require.Contains(t, string(adminJSON), tc.wantJSON)
			}

			userJSON, err := json.Marshal(UsageLogFromService(log))
			require.NoError(t, err)
			require.NotContains(t, string(userJSON), `"route_degraded"`)
		})
	}
}

func TestAccountListItemKeepsTiboObservationsSeparateFromCodexTickets(t *testing.T) {
	t.Parallel()
	account := &Account{
		ID:                  42,
		CodexTurnTickets:    []service.OpenAICodexTicketStatus{{Model: "gpt-6-astra", Ready: true, Length: 128, TurnStatePresent: true}},
		TiboAccountStatuses: []service.OpenAITiboAccountStatus{{Model: "gpt-6-astra", TiboRoutes: []service.OpenAITiboRouteStatus{{Route: "http", Verdict: "healthy"}}}},
	}
	list := AccountListItemFromAccount(account)
	require.Equal(t, account.CodexTurnTickets, list.CodexTurnTickets)
	require.Equal(t, account.TiboAccountStatuses, list.TiboAccountStatuses)
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Contains(t, string(payload["codex_turn_tickets"]), `"ready":true`)
	require.NotContains(t, string(payload["codex_turn_tickets"]), `"tibo_routes"`)
	require.Contains(t, string(payload["tibo_account_statuses"]), `"verdict":"healthy"`)
}

func TestAccountManagedExtraRedactsTiboHistory(t *testing.T) {
	t.Parallel()
	extra := map[string]any{
		"codex_tibo_verdict:http":                 map[string]any{"probe_identity": "private-account-identity"},
		service.OpenAITiboQualityIsolatedExtraKey: true,
		"ordinary": "kept",
	}
	redacted := redactAccountManagedExtra(extra)
	require.NotContains(t, redacted, "codex_tibo_verdict:http")
	require.Equal(t, true, redacted[service.OpenAITiboQualityIsolatedExtraKey])
	require.Equal(t, "kept", redacted["ordinary"])
	require.Contains(t, extra, "codex_tibo_verdict:http")
}
