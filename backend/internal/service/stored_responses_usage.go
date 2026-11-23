package service

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type storedResponseUsageError struct {
	err   error
	usage OpenAIUsage
}

func (e *storedResponseUsageError) Error() string { return e.err.Error() }
func (e *storedResponseUsageError) Unwrap() error { return e.err }

// Preserve observed counters across storage failures without exposing the
// upstream terminal, private snapshot, or retrying an inference send.
func withStoredResponseFailureUsage(ctx context.Context, err error) error {
	var existing *storedResponseUsageError
	if err == nil || errors.As(err, &existing) {
		return err
	}
	var usage OpenAIUsage
	reported := false
	if attempt := storedResponseAttemptFrom(ctx); attempt != nil {
		if values, ok := attempt.capture.observedUsage.(map[string]any); ok && len(values) > 0 {
			usage = *storedResponseOpenAIUsage(values)
			reported = true
		}
	}
	if observed, ok := nativeTerminalUsage15(ctx); ok {
		mergeOpenAIUsageNonZero(&usage, *observed)
		reported = true
	}
	if reported {
		return &storedResponseUsageError{err: err, usage: usage}
	}
	return err
}

func storedResponseOpenAIUsage(value map[string]any) *OpenAIUsage {
	usage := &OpenAIUsage{}
	if value == nil {
		return usage
	}
	raw, _ := json.Marshal(map[string]any{"usage": value})
	if parsed, ok := extractOpenAIUsageFromJSONBytes(raw); ok {
		*usage = parsed
	}
	return usage
}

func safeOpenAIFailureUsage(value gjson.Result) map[string]any {
	if !value.IsObject() {
		return nil
	}
	result := make(map[string]any)
	copyCount := func(source gjson.Result, field string, dest map[string]any) {
		count := source.Get(field)
		if validOpenAIFailureCount(count) {
			dest[field] = json.RawMessage(count.Raw)
		}
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		copyCount(value, field, result)
	}
	for field, child := range map[string]string{"input_tokens_details": "cached_tokens", "output_tokens_details": "reasoning_tokens"} {
		details := make(map[string]any)
		copyCount(value.Get(field), child, details)
		if len(details) > 0 {
			result[field] = details
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// Exact arithmetic prevents fractional large numbers from rounding into counts.
func validOpenAIFailureCount(count gjson.Result) bool {
	if count.Type != gjson.Number || len(count.Raw) > 64 {
		return false
	}
	if index := strings.IndexAny(count.Raw, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(count.Raw[index+1:])
		if err != nil || exponent < -324 || exponent > 308 {
			return false
		}
	}
	exact, ok := new(big.Rat).SetString(count.Raw)
	return ok && exact.IsInt() && exact.Sign() >= 0 && exact.Num().Cmp(big.NewInt(9007199254740991)) <= 0
}
