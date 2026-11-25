package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSAccessModelIDs_FiltersUnavailableRestrictedAndDuplicates(t *testing.T) {
	raw := []byte(`{"allowed":true,"model_catalog":{"restricted_models":["gpt-6-luna"],"models":[{"id":"gpt-6-astra"},{"id":"gpt-5.6-sol","available":true},{"id":"gpt-6-luna"},{"id":"gpt-5.6-terra","enabled":false},{"id":"gpt-6-sol","model_picker_enabled":false},{"id":"gpt-5.6-luna","policy":{"state":"disabled"}},{"id":"not-a-gpt-model"},{"id":"gpt-6-astra"}]}}`)

	ids, err := bpsAccessModelIDs(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-6-astra", "gpt-5.6-sol"}, ids)
}

func TestBPSAccessModelIDs_ExplicitDenialIsEmptyNotError(t *testing.T) {
	ids, err := bpsAccessModelIDs([]byte(`{"allowed":false}`))
	require.NoError(t, err)
	require.Empty(t, ids)
	require.NotNil(t, ids)
}

func TestBPSAccessModelIDs_MalformedEnvelopeIsError(t *testing.T) {
	for _, raw := range []string{`not json`, `{}`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{}}`} {
		_, err := bpsAccessModelIDs([]byte(raw))
		require.Error(t, err, raw)
	}
}
