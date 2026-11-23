package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// StoredResponsesWorker only expires display records and recovers stale leases;
// recovery never starts or retries upstream generation.
type StoredResponsesWorker struct {
	service *StoredResponseService
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewStoredResponsesWorker(service *StoredResponseService) *StoredResponsesWorker {
	return &StoredResponsesWorker{service: service}
}

func (w *StoredResponsesWorker) Start() {
	if w == nil || !w.service.Enabled() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.cancel, w.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for ctx.Err() == nil {
			work, stop := context.WithTimeout(ctx, 10*time.Second)
			_, err := w.service.Sweep(work, 100)
			stop()
			if err != nil && ctx.Err() == nil {
				log.Printf("[StoredResponses] cleanup pending: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *StoredResponsesWorker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	w.mu.Lock()
	if w.done == done {
		w.cancel = nil
		w.done = nil
	}
	w.mu.Unlock()
}
