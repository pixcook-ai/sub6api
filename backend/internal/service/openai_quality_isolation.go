package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type openAITiboPreviousResponseKey struct{}

func withOpenAITiboQualityAdmission(ctx context.Context, body []byte) context.Context {
	previous := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previous == "" {
		return ctx
	}
	return context.WithValue(ctx, openAITiboPreviousResponseKey{}, previous)
}

// openAITiboQualityIsolationReader is deliberately narrower than GetByID: final
// admission reads only the durable quality bit and its credential parent's bit.
// Keeping this optional preserves custom repositories that only supply snapshots.
type openAITiboQualityIsolationReader interface {
	GetOpenAITiboQualityIsolation(context.Context, int64) (bool, error)
}

// checkOpenAITiboQualitySnapshotAdmission is the cheap entry gate. Authoritative
// state is checked again at the final HTTP or WS send boundary, after any waits.
func (s *OpenAIGatewayService) checkOpenAITiboQualitySnapshotAdmission(ctx context.Context, account *Account, body []byte) error {
	if account == nil || !account.IsOpenAI() || isOpenAITiboSyntheticProbe(ctx, account) {
		return nil
	}
	return openAITiboQualityAdmissionError(ctx, account.IsOpenAITiboQualityIsolated(), gjson.GetBytes(body, "previous_response_id").String())
}

// checkOpenAITiboQualityAdmission closes the scheduler-to-send gap. Recovery
// probes have private provenance, while ordinary requests always read the durable
// gate when the production repository supports it, independently of Enabled.
func (s *OpenAIGatewayService) checkOpenAITiboQualityAdmission(ctx context.Context, account *Account, body []byte) error {
	return s.checkOpenAITiboQualityAdmissionForPrevious(ctx, account, gjson.GetBytes(body, "previous_response_id").String())
}

func (s *OpenAIGatewayService) checkOpenAITiboQualityAdmissionForPrevious(ctx context.Context, account *Account, previousResponseID string) error {
	if account == nil || !account.IsOpenAI() || isOpenAITiboSyntheticProbe(ctx, account) {
		return nil
	}
	isolated := account.IsOpenAITiboQualityIsolated()
	if s != nil {
		if reader, ok := s.accountRepo.(openAITiboQualityIsolationReader); ok {
			current, err := reader.GetOpenAITiboQualityIsolation(ctx, account.ID)
			if err != nil {
				return fmt.Errorf("verify OpenAI quality isolation: %w", err)
			}
			isolated = current
		}
	}
	return openAITiboQualityAdmissionError(ctx, isolated, previousResponseID)
}

func openAITiboQualityAdmissionError(ctx context.Context, isolated bool, previousResponseID string) error {
	if !isolated {
		return nil
	}
	if previousResponseID == "" && ctx != nil {
		previousResponseID, _ = ctx.Value(openAITiboPreviousResponseKey{}).(string)
	}
	action := NextAccountRetry
	if strings.TrimSpace(previousResponseID) != "" {
		// Upstream response state belongs to this account. Never send an unsafe
		// continuation to a different account merely to escape isolation.
		action = NextAccountStop
	}
	return &UpstreamFailoverError{
		StatusCode: http.StatusServiceUnavailable, ClientStatusCode: http.StatusServiceUnavailable,
		ClientMessage: "账号已被 Tibo 质量隔离，停止调度 / Account is isolated by Tibo quality monitoring",
		Reason:        GatewayFailureReason("openai_tibo_quality_isolated"), NextAccountAction: action,
		RequestScopedTransient: true,
	}
}

// A bound response must be rejected before the ordinary resolver discards an
// unschedulable binding and falls back to another account.
func (s *OpenAIGatewayService) checkOpenAITiboPreviousResponseAdmission(ctx context.Context, groupID *int64, previousResponseID string) error {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if s == nil || s.accountRepo == nil || previousResponseID == "" {
		return nil
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return nil
	}
	id, err := store.GetResponseAccount(ctx, derefGroupID(groupID), previousResponseID)
	if err != nil || id <= 0 {
		return nil
	}
	if reader, ok := s.accountRepo.(openAITiboQualityIsolationReader); ok {
		isolated, err := reader.GetOpenAITiboQualityIsolation(ctx, id)
		if err != nil {
			return fmt.Errorf("verify OpenAI quality isolation: %w", err)
		}
		return openAITiboQualityAdmissionError(ctx, isolated, previousResponseID)
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil {
		// Keep the pre-existing behavior for an unavailable response owner;
		// the ordinary resolver performs its own availability checks.
		return nil
	}
	return s.checkOpenAITiboQualityAdmissionForPrevious(ctx, account, previousResponseID)
}
