package admin

import (
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestUsageStatsCacheKey_RouteDegradedTriState(t *testing.T) {
	healthy, degraded := false, true
	base := usagestats.UsageLogFilters{}
	healthyFilter := base
	healthyFilter.RouteDegraded = &healthy
	degradedFilter := base
	degradedFilter.RouteDegraded = &degraded
	require.NotEqual(t, usageStatsCacheKey(base), usageStatsCacheKey(healthyFilter))
	require.NotEqual(t, usageStatsCacheKey(base), usageStatsCacheKey(degradedFilter))
	require.NotEqual(t, usageStatsCacheKey(healthyFilter), usageStatsCacheKey(degradedFilter))
}
