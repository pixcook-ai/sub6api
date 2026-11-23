package service

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const claudeOAuthCompatibilitySourceKey = "claude_oauth_compatibility_source"
const claudeOAuthCompatibilityTTL = 15 * time.Minute
const claudeOAuthCompatibilityCapacity = 1024

// ClaudeOAuthCompatibilityDiagnostic contains no original identity or opaque metadata.
type ClaudeOAuthCompatibilityDiagnostic struct {
	ObservedAt                    string   `json:"observed_at"`
	Endpoint                      string   `json:"endpoint"`
	Mode                          string   `json:"mode"`
	ClientVersion                 string   `json:"client_version"`
	IncomingAttributionBlocks     int      `json:"incoming_attribution_blocks"`
	OutgoingAttributionBlocks     int      `json:"outgoing_attribution_blocks"`
	CCGatewayPresent              bool     `json:"cc_gateway_present"`
	CCHPresent                    bool     `json:"cch_present"`
	UnknownAttributionFields      int      `json:"unknown_attribution_fields"`
	WigglyPresent                 bool     `json:"wiggly_present"`
	WigglyForwarded               bool     `json:"wiggly_forwarded"`
	AdditionalProtectionPresent   bool     `json:"additional_protection_present"`
	AdditionalProtectionForwarded bool     `json:"additional_protection_forwarded"`
	BodyChanged                   bool     `json:"body_changed"`
	MetadataChanged               bool     `json:"metadata_changed"`
	BetaChanged                   bool     `json:"beta_changed"`
	IntegrityStatus               string   `json:"integrity_status"`
	Reasons                       []string `json:"reasons"`
}

type claudeAttributionSummary struct {
	blocks, unknown                       int
	gateway, cch, malformed, noncanonical bool
}

type claudeOAuthCompatibilitySource struct {
	attribution                      claudeAttributionSummary
	bodyHash, metadataHash, betaHash [32]byte
	clientVersion                    string
	wiggly, additionalProtection     bool
}

type claudeOAuthCompatibilityEntry struct {
	diagnostic ClaudeOAuthCompatibilityDiagnostic
	expiresAt  time.Time
}

var claudeOAuthCompatibilityObservations = struct {
	sync.Mutex
	entries map[int64]claudeOAuthCompatibilityEntry
}{entries: make(map[int64]claudeOAuthCompatibilityEntry)}

// GetClaudeOAuthCompatibilityDiagnostic returns a copy of a recent local observation.
func GetClaudeOAuthCompatibilityDiagnostic(accountID int64) *ClaudeOAuthCompatibilityDiagnostic {
	claudeOAuthCompatibilityObservations.Lock()
	defer claudeOAuthCompatibilityObservations.Unlock()
	entry, ok := claudeOAuthCompatibilityObservations.entries[accountID]
	if !ok {
		return nil
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(claudeOAuthCompatibilityObservations.entries, accountID)
		return nil
	}
	result := entry.diagnostic
	result.Reasons = append([]string{}, result.Reasons...)
	return &result
}

func storeClaudeOAuthCompatibilityDiagnostic(accountID int64, diagnostic ClaudeOAuthCompatibilityDiagnostic) {
	if accountID <= 0 {
		return
	}
	now := time.Now()
	claudeOAuthCompatibilityObservations.Lock()
	defer claudeOAuthCompatibilityObservations.Unlock()
	if _, exists := claudeOAuthCompatibilityObservations.entries[accountID]; !exists && len(claudeOAuthCompatibilityObservations.entries) >= claudeOAuthCompatibilityCapacity {
		var oldestID int64
		var oldest time.Time
		for id, entry := range claudeOAuthCompatibilityObservations.entries {
			if oldest.IsZero() || entry.expiresAt.Before(oldest) {
				oldestID, oldest = id, entry.expiresAt
			}
		}
		delete(claudeOAuthCompatibilityObservations.entries, oldestID)
	}
	diagnostic.Reasons = append([]string{}, diagnostic.Reasons...)
	claudeOAuthCompatibilityObservations.entries[accountID] = claudeOAuthCompatibilityEntry{diagnostic: diagnostic, expiresAt: now.Add(claudeOAuthCompatibilityTTL)}
}

func claudeCompatibilityHeaderPresent(headers http.Header, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func inspectClaudeAttribution(body []byte) claudeAttributionSummary {
	var summary claudeAttributionSummary
	system := gjson.GetBytes(body, "system")
	inspect := func(text string) {
		text = strings.TrimLeft(text, " \t")
		if !strings.HasPrefix(text, "x-anthropic-billing-header") {
			return
		}
		summary.blocks++
		first, rest, _ := strings.Cut(text, "\n")
		if rest != "" {
			summary.noncanonical = true
		}
		if !strings.HasPrefix(first, "x-anthropic-billing-header:") {
			summary.malformed = true
			return
		}
		for _, field := range strings.Split(strings.TrimPrefix(first, "x-anthropic-billing-header:"), ";") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			key, _, exists := strings.Cut(field, "=")
			if !exists {
				summary.malformed = true
				continue
			}
			switch strings.TrimSpace(key) {
			case "cc_version", "cc_entrypoint":
			case "cc_gateway":
				summary.gateway = true
			case "cch":
				summary.cch = true
			default:
				summary.unknown++
			}
		}
	}
	if system.Type == gjson.String {
		inspect(system.String())
		summary.noncanonical = summary.blocks > 0
	} else if system.IsArray() {
		for i, block := range system.Array() {
			before := summary.blocks
			if text := block.Get("text"); text.Type == gjson.String && (!block.Get("type").Exists() || block.Get("type").String() == "text") {
				inspect(text.String())
			}
			if i > 0 && summary.blocks > before {
				summary.noncanonical = true
			}
		}
	}
	return summary
}

func newClaudeOAuthCompatibilitySource(body []byte, headers http.Header) *claudeOAuthCompatibilitySource {
	version := ExtractCLIVersion(getHeaderRaw(headers, "User-Agent"))
	if len(version) > 32 {
		version = ""
	}
	return &claudeOAuthCompatibilitySource{
		attribution: inspectClaudeAttribution(body), bodyHash: sha256.Sum256(body),
		metadataHash: sha256.Sum256([]byte(gjson.GetBytes(body, "metadata").Raw)),
		betaHash:     sha256.Sum256([]byte(getHeaderRaw(headers, "anthropic-beta"))), clientVersion: version,
		wiggly:               claudeCompatibilityHeaderPresent(headers, "x-claude-code-wiggly-dove"),
		additionalProtection: claudeCompatibilityHeaderPresent(headers, "x-anthropic-additional-protection"),
	}
}

func captureClaudeOAuthCompatibilitySource(c *gin.Context, account *Account, body []byte) {
	if c == nil || !isClaudeOAuthCompatibilityAccount(account) {
		return
	}
	if _, exists := c.Get(claudeOAuthCompatibilitySourceKey); exists {
		return
	}
	headers := http.Header{}
	if c.Request != nil {
		headers = c.Request.Header
	}
	c.Set(claudeOAuthCompatibilitySourceKey, newClaudeOAuthCompatibilitySource(body, headers))
}

func claudeOAuthCompatibilityInput(c *gin.Context, account *Account, body []byte, headers http.Header) *claudeOAuthCompatibilitySource {
	if !isClaudeOAuthCompatibilityAccount(account) {
		return nil
	}
	if c != nil {
		if value, exists := c.Get(claudeOAuthCompatibilitySourceKey); exists {
			if source, ok := value.(*claudeOAuthCompatibilitySource); ok {
				return source
			}
		}
	}
	source := newClaudeOAuthCompatibilitySource(body, headers)
	if c != nil {
		c.Set(claudeOAuthCompatibilitySourceKey, source)
	}
	return source
}

func finalizeClaudeOAuthCompatibility(account *Account, source *claudeOAuthCompatibilitySource, request *http.Request, body []byte, endpoint string) {
	if source == nil || !isClaudeOAuthCompatibilityAccount(account) {
		return
	}
	outgoing := inspectClaudeAttribution(body)
	diagnostic := ClaudeOAuthCompatibilityDiagnostic{
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Endpoint: endpoint, Mode: "transparent", ClientVersion: source.clientVersion,
		IncomingAttributionBlocks: source.attribution.blocks, OutgoingAttributionBlocks: outgoing.blocks,
		CCGatewayPresent: source.attribution.gateway, CCHPresent: source.attribution.cch, UnknownAttributionFields: source.attribution.unknown,
		WigglyPresent: source.wiggly, WigglyForwarded: claudeCompatibilityHeaderPresent(request.Header, "x-claude-code-wiggly-dove"),
		AdditionalProtectionPresent: source.additionalProtection, AdditionalProtectionForwarded: claudeCompatibilityHeaderPresent(request.Header, "x-anthropic-additional-protection"),
		BodyChanged: source.bodyHash != sha256.Sum256(body), MetadataChanged: source.metadataHash != sha256.Sum256([]byte(gjson.GetBytes(body, "metadata").Raw)),
		BetaChanged:     source.betaHash != sha256.Sum256([]byte(getHeaderRaw(request.Header, "anthropic-beta"))),
		IntegrityStatus: "unverified", Reasons: []string{},
	}
	if source.attribution.blocks == 0 && outgoing.blocks > 0 {
		diagnostic.Reasons = append(diagnostic.Reasons, "attribution_synthesized")
	}
	if source.attribution.malformed || source.attribution.noncanonical || source.attribution.blocks > 1 {
		diagnostic.Reasons = append(diagnostic.Reasons, "noncanonical_attribution")
	}
	if source.attribution.unknown > 0 {
		diagnostic.Reasons = append(diagnostic.Reasons, "unknown_attribution_fields")
	}
	if source.wiggly || source.additionalProtection {
		diagnostic.Reasons = append(diagnostic.Reasons, "opaque_metadata_unverified")
	}
	if source.wiggly && !diagnostic.WigglyForwarded {
		diagnostic.Reasons = append(diagnostic.Reasons, "wiggly_dropped_by_allowlist")
	}
	storeClaudeOAuthCompatibilityDiagnostic(account.ID, diagnostic)
}

func isClaudeOAuthCompatibilityAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformAnthropic && account.Type == AccountTypeOAuth
}
