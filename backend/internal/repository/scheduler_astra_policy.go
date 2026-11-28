package repository

import (
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

func schedulerAstraVersion(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("20060102150405.000000000")
}

// Include manual clears as well as probe observations. Delayed snapshot rebuilds
// must not restore either an old isolation or a previously healthy projection.
func schedulerTiboVersion(account service.Account) string {
	_, at := service.LatestOpenAIAstraSample(account.Extra[service.OpenAIAstraPolicyExtraKey])
	if raw, ok := account.Extra[service.OpenAITiboQualityIsolationAtExtraKey].(string); ok {
		if changedAt, err := time.Parse(time.RFC3339Nano, raw); err == nil && changedAt.After(at) {
			at = changedAt
		}
	}
	return schedulerAstraVersion(at)
}

// Both projections change atomically. IDs and revisions from a delayed rebuild
// may still publish, but cannot replace newer probe/isolation state.
const schedulerWriteAstraAccountScript = `
local previous = redis.call('GET', KEYS[2])
local current = redis.call('GET', KEYS[1])
local function isNewer(raw)
  if not raw then return false end
  local ok, decoded = pcall(cjson.decode, raw)
  if ok and type(decoded) == 'table' then
    local version = decoded.astra_policy_version
    if type(version) == 'string' and version > ARGV[3] then return true end
  end
  return false
end
if isNewer(current) or isNewer(previous) then return 0 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[2])
return 1
`
