package service

import (
	"context"
	"io"
	"sync"
	"time"
)

type grantedFileReader struct {
	reader     io.ReadCloser
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	changed    chan struct{}
	mu         sync.Mutex
	deadline   time.Time
	err        error
	closeOnce  sync.Once
	finishOnce sync.Once
	closeErr   error
	release    func()
}

func newGrantedFileReader(parent context.Context, reader io.ReadCloser, started time.Time, grant FileReadGrant, lease time.Duration, renew func(context.Context) (FileReadGrant, error), release func()) *grantedFileReader {
	ctx, cancel := context.WithCancel(parent)
	r := &grantedFileReader{reader: reader, ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}, 1), deadline: started.Add(grant.Until.Sub(grant.DBNow)), release: release}
	go func() {
		for {
			r.mu.Lock()
			remaining := time.Until(r.deadline)
			r.mu.Unlock()
			if remaining <= 0 {
				r.fail(ErrFileLeaseConflict)
				r.finish()
				return
			}
			timer := time.NewTimer(remaining)
			select {
			case <-ctx.Done():
				timer.Stop()
				r.finish()
				return
			case <-r.changed:
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
	go func() {
		defer close(r.done)
		defer r.finish()
		for {
			r.mu.Lock()
			remaining := time.Until(r.deadline)
			r.mu.Unlock()
			if remaining <= 0 {
				r.fail(ErrFileLeaseConflict)
				return
			}
			wait := lease / 3
			if remaining/3 < wait {
				wait = remaining / 3
			}
			if wait < time.Millisecond {
				wait = time.Millisecond
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				r.fail(ctx.Err())
				return
			case <-timer.C:
			}
			began := time.Now()
			r.mu.Lock()
			deadline := r.deadline
			r.mu.Unlock()
			renewCtx, stop := context.WithDeadline(ctx, deadline)
			g, err := renew(renewCtx)
			stop()
			if err != nil {
				r.fail(err)
				return
			}
			r.mu.Lock()
			if !time.Now().Before(r.deadline) {
				r.err = ErrFileLeaseConflict
				r.mu.Unlock()
				r.cancel()
				return
			}
			r.deadline = began.Add(g.Until.Sub(g.DBNow))
			r.mu.Unlock()
			select {
			case r.changed <- struct{}{}:
			default:
			}
		}
	}()
	return r
}

func (r *grantedFileReader) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.cancel()
}
func (r *grantedFileReader) finish() {
	r.finishOnce.Do(func() { r.closeErr = r.reader.Close(); r.release() })
}
func (r *grantedFileReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	err := r.err
	if err == nil && !time.Now().Before(r.deadline) {
		err = ErrFileLeaseConflict
		r.err = err
	}
	r.mu.Unlock()
	if err != nil {
		r.cancel()
		r.finish()
		return 0, err
	}
	if err = r.ctx.Err(); err != nil {
		r.finish()
		return 0, err
	}
	return r.reader.Read(p)
}
func (r *grantedFileReader) Close() error {
	r.closeOnce.Do(func() { r.cancel(); <-r.done; r.finish() })
	return r.closeErr
}
