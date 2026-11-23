package service

import (
	"context"
	"errors"
	"log"
	"time"
)

// RunLifecycleOnce advances a bounded ID page. A blocked remote ledger row
// cannot starve later files; each row owns an independent transaction.
func (s *FileService) RunLifecycleOnce(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	s.lifecycleRunMu.Lock()
	defer s.lifecycleRunMu.Unlock()
	ids, err := s.repo.LifecycleCandidates(ctx, s.lifecycleCursor, s.cfg.LifecycleWorkerBatchSize)
	if err != nil {
		return err
	}
	var failures []error
	// Periodically revisit crashed uploads too, even if no new upload arrives.
	// Keep each pass bounded; the startup Recover path can still drain fully.
	recoveryToken, recoveryErr := fileToken()
	if recoveryErr != nil {
		return recoveryErr
	}
	stale, recoveryErr := s.repo.ClaimStaleUploads(ctx, recoveryToken, s.lease(), s.cfg.LifecycleWorkerBatchSize)
	if recoveryErr != nil {
		failures = append(failures, recoveryErr)
	} else {
		for _, f := range stale {
			if e := s.store.Remove(ctx, f.StorageKey); e != nil {
				failures = append(failures, e)
				continue
			}
			if e := s.repo.FinishCleanup(ctx, f.ID, recoveryToken); e != nil {
				failures = append(failures, e)
			}
		}
	}
	for _, id := range ids {
		s.lifecycleCursor = id
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = s.repo.RetireExpired(ctx, id, time.Duration(s.cfg.ReadDrainSeconds)*time.Second); err != nil {
			failures = append(failures, err)
			continue
		}
		token, e := fileToken()
		if e != nil {
			return e
		}
		ticket, e := s.repo.ClaimLocalPurge(ctx, id, token, time.Duration(s.cfg.CleanupLeaseSeconds)*time.Second)
		if errors.Is(e, ErrFileLeaseConflict) || errors.Is(e, ErrFileNotFound) {
			continue
		}
		if e != nil {
			failures = append(failures, e)
			continue
		}
		if e = s.store.Remove(ctx, ticket.StorageKey); e != nil {
			failures = append(failures, e)
			continue
		}
		if e = s.repo.ConfirmLocalPurge(ctx, *ticket); e != nil {
			failures = append(failures, e)
		}
	}
	if len(ids) < s.cfg.LifecycleWorkerBatchSize {
		s.lifecycleCursor = 0
	}
	return errors.Join(failures...)
}

func (s *FileService) StartLifecycle() {
	if !s.Enabled() {
		return
	}
	s.lifecycleMu.Lock()
	if s.lifecycleCancel != nil {
		s.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.lifecycleCancel, s.lifecycleDone = cancel, done
	s.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(s.cfg.LifecycleWorkerIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			work, stop := context.WithTimeout(ctx, time.Duration(s.cfg.CleanupLeaseSeconds)*time.Second)
			if err := s.RunLifecycleOnce(work); err != nil && ctx.Err() == nil {
				log.Printf("[FilesLifecycle] local cleanup pending: %v", err)
			}
			stop()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (s *FileService) StopLifecycle() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	cancel, done := s.lifecycleCancel, s.lifecycleDone
	s.lifecycleMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	s.lifecycleMu.Lock()
	if s.lifecycleDone == done {
		s.lifecycleCancel = nil
		s.lifecycleDone = nil
	}
	s.lifecycleMu.Unlock()
}
