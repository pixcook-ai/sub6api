package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	bpsDefaultRetryAfter = time.Minute
	bpsMaxRetryAfter     = 7 * 24 * time.Hour
	bpsRetryAfterKey     = "bps_retry_after_seconds"
)

// Provider waits are accepted only as structured timings from this attempt.
func bpsRateLimitDelay(now time.Time, header http.Header, detail map[string]any) time.Duration {
	if seconds := bpsPositiveInt(header.Get("Retry-After")); seconds > 0 {
		return bpsBoundDelay(seconds, time.Second)
	}
	if reset, err := http.ParseTime(header.Get("Retry-After")); err == nil && reset.After(now) {
		return min(reset.Sub(now), bpsMaxRetryAfter)
	}
	if ms := bpsPositiveInt(header.Get("Retry-After-Ms")); ms > 0 {
		return bpsBoundDelay(ms, time.Millisecond)
	}
	if resetAt := bpsPositiveInt(detail["resets_at"]); resetAt > now.Unix() {
		return bpsBoundDelay(resetAt-now.Unix(), time.Second)
	}
	for _, key := range []string{"resets_in_seconds", "retry_after_seconds", "retry_after"} {
		if seconds := bpsPositiveInt(detail[key]); seconds > 0 {
			return bpsBoundDelay(seconds, time.Second)
		}
	}
	if ms := bpsPositiveInt(detail["retry_after_ms"]); ms > 0 {
		return bpsBoundDelay(ms, time.Millisecond)
	}
	return bpsDefaultRetryAfter
}

func bpsPositiveInt(raw any) int64 {
	var text string
	switch v := raw.(type) {
	case json.Number:
		text = v.String()
	case string:
		text = strings.TrimSpace(v)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func bpsBoundDelay(amount int64, unit time.Duration) time.Duration {
	return time.Duration(min(amount, int64(bpsMaxRetryAfter/unit))) * unit
}

func bpsSetClientRetryAfter(c *gin.Context, delay time.Duration) int {
	seconds := int((delay + time.Second - 1) / time.Second)
	c.Set(bpsRetryAfterKey, seconds)
	if !c.Writer.Written() {
		c.Header("Retry-After", strconv.Itoa(seconds))
	}
	return seconds
}

func bpsRateLimited(c *gin.Context, delay time.Duration) *bpsError {
	seconds := bpsSetClientRetryAfter(c, delay)
	return &bpsError{http.StatusTooManyRequests, "bps_rate_limited", fmt.Sprintf("BPS rate limit was reached. Retry after %d seconds.", seconds)}
}

func (s *OpenAIGatewayService) bpsRecordRateLimit(ctx context.Context, c *gin.Context, account *Account, header http.Header, detail map[string]any) *bpsError {
	now := time.Now()
	delay := bpsRateLimitDelay(now, header, detail)
	if account.RateLimitResetAt != nil {
		if remaining := account.RateLimitResetAt.Sub(now); remaining > delay {
			delay = min(remaining, bpsMaxRetryAfter)
		}
	}
	if s.accountRepo != nil {
		until := now.Add(delay)
		if repo, ok := s.accountRepo.(interface {
			SetRateLimitedIfLater(context.Context, int64, time.Time) error
		}); ok {
			_ = repo.SetRateLimitedIfLater(ctx, account.ID, until)
		} else {
			_ = s.accountRepo.SetRateLimited(ctx, account.ID, until)
		}
	}
	return bpsRateLimited(c, delay)
}

// Retried bound conversations must continue to report the cooldown instead
// of turning a temporary quota limit into a session/account error.
func (s *OpenAIGatewayService) WriteOpenAIBPSBoundAccountError(c *gin.Context) {
	ctx := c.Request.Context()
	routing, _ := ctx.Value(bpsRoutingContextKey{}).(bpsRoutingContext)
	if routing.Binding != nil && s.accountRepo != nil {
		account, err := s.accountRepo.GetByID(ctx, routing.Binding.AccountID)
		if err == nil && account != nil && account.IsOpenAIBPS() && bpsBoundAccountAllowed(ctx, account) && account.RateLimitResetAt != nil {
			withoutCooldown := *account
			withoutCooldown.RateLimitResetAt = nil
			if delay := time.Until(*account.RateLimitResetAt); delay > 0 && withoutCooldown.IsSchedulable() {
				WriteOpenAIBPSError(c, bpsRateLimited(c, min(delay, bpsMaxRetryAfter)))
				return
			}
		}
	}
	if ctx.Err() == nil {
		WriteOpenAIBPSAccountUnavailable(c)
	}
}

// Preserve protocol identifiers only. Never use provider messages as identifiers.
func bpsDiagnosticIdentifier(value, accessToken string) string {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if len(value) == 0 || len(value) > 128 ||
		(accessToken != "" && strings.Contains(value, accessToken)) ||
		strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "sess-") || strings.HasPrefix(lower, "eyj") {
		return ""
	}
	if strings.ContainsFunc(value, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			return false
		default:
			return true
		}
	}) {
		return ""
	}
	return value
}
