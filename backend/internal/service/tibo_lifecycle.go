package service

import (
	"context"
	"fmt"
	"time"
)

// OpenAITiboAccountStatus retains the established admin sparkline envelope.
type OpenAITiboAccountStatus struct {
	Model      string                  `json:"model"`
	TiboRoutes []OpenAITiboRouteStatus `json:"tibo_routes,omitempty"`
}

func (s *AccountTestService) OpenAITiboAccountStatuses(account *Account, now time.Time) []OpenAITiboAccountStatus {
	if s == nil || s.openaiGatewayService == nil {
		return nil
	}
	routes := s.openaiGatewayService.openAITiboRouteStatuses(account, now)
	if len(routes) == 0 {
		return nil
	}
	return []OpenAITiboAccountStatus{{Model: openAITiboProbeModel, TiboRoutes: routes}}
}

// StartOpenAITiboProber belongs to application startup, never service construction.
func (s *OpenAIGatewayService) StartOpenAITiboProber() {
	if s == nil || s.accountRepo == nil {
		return
	}
	s.openaiTiboLifecycleMu.Lock()
	defer s.openaiTiboLifecycleMu.Unlock()
	if s.openaiTiboStopped || s.openaiTiboDone != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.openaiTiboContext, s.openaiTiboCancel = ctx, cancel
	done := make(chan struct{})
	s.openaiTiboDone = done
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.probeOpenAITiboRoutes(ctx)
				timer.Reset(6 * time.Second)
			}
		}
	}()
}

func (s *OpenAIGatewayService) StopOpenAITiboProber() {
	if s == nil {
		return
	}
	s.openaiTiboLifecycleMu.Lock()
	s.openaiTiboStopped = true
	cancel, done := s.openaiTiboCancel, s.openaiTiboDone
	s.openaiTiboLifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	s.openaiTiboProbeWG.Wait()
}

func (s *AccountTestService) ClearOpenAITiboQualityIsolation(ctx context.Context, id int64) error {
	if s == nil || s.openaiGatewayService == nil {
		return fmt.Errorf("tibo account service is unavailable")
	}
	repo, ok := s.openaiGatewayService.accountRepo.(interface {
		ClearOpenAITiboQualityIsolation(context.Context, int64) error
	})
	if !ok {
		return fmt.Errorf("tibo quality isolation persistence is unavailable")
	}
	return repo.ClearOpenAITiboQualityIsolation(ctx, id)
}
