package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTiboSelectionOrderOmitsQualityIsolationEvenWithoutMonitoring(t *testing.T) {
	healthy := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	isolated := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{OpenAITiboQualityIsolatedExtraKey: true}}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{}}
	candidates := []openAIAccountCandidateScore{{account: isolated, score: 100}, {account: healthy, score: 1}}
	order := scheduler.buildOpenAISelectionOrder(OpenAIAccountScheduleRequest{}, openAIAccountLoadPlan{
		candidates: candidates, allCandidates: candidates, topK: 2,
	})
	require.Len(t, order, 1)
	require.Same(t, healthy, order[0].account)
}

func TestTiboQualityIsolationIsNotSchedulerFailure(t *testing.T) {
	err := openAITiboQualityAdmissionError(context.Background(), true, "")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, failover.ShouldReportAccountScheduleFailure())
	require.True(t, failover.ShouldRetryNextAccount())
}

func TestTiboQualityIsolationTransportHandlingPreservesBoundResponse(t *testing.T) {
	original := openAITiboQualityAdmissionError(context.Background(), true, "resp_owner")
	svc := &OpenAIGatewayService{}
	result := svc.handleOpenAIUpstreamTransportError(context.Background(), nil, nil, original, false)
	require.Same(t, original, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, result, &failover)
	require.False(t, failover.ShouldRetryNextAccount())
	require.False(t, failover.ShouldReportAccountScheduleFailure())
}
