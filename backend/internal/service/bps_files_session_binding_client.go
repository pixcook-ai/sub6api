package service

import (
	"context"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// DescribeSessionBinding authenticates the current backend and requires explicit
// support before any new session-bound inference or manifest can be sent.
func (b *BPSFilesBackend) DescribeSessionBinding(ctx context.Context) (managedfiles.DescribeResult, error) {
	if b == nil || b.upstream == nil || len(b.config.ServiceKey) < 32 {
		return managedfiles.DescribeResult{}, bpsFilesError("session_binding_unavailable")
	}
	upstream := &bpsSessionBindingDescribeUpstream{HTTPUpstream: b.upstream}
	backend := *b
	backend.upstream = upstream
	result, err := backend.Describe(ctx)
	if err != nil {
		return managedfiles.DescribeResult{}, err
	}
	if !upstream.negotiated {
		return managedfiles.DescribeResult{}, bpsFilesError("session_binding_unavailable")
	}
	return result, nil
}

// Only this single authenticated Describe receives the feature request. The
// existing decoder retains identity, shape, limit and body-close validation.
type bpsSessionBindingDescribeUpstream struct {
	HTTPUpstream
	negotiated bool
}

func (u *bpsSessionBindingDescribeUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	req.Header.Set(managedfiles.SessionBindingFeatureHeader, managedfiles.SessionBindingContract)
	resp, err := u.HTTPUpstream.Do(req, proxy, accountID, concurrency)
	if err == nil && resp != nil {
		var values []string
		for name, candidates := range resp.Header {
			if strings.EqualFold(name, managedfiles.SessionBindingFeatureHeader) {
				values = append(values, candidates...)
			}
		}
		u.negotiated = len(values) == 1 && values[0] == managedfiles.SessionBindingContract
	}
	return resp, err
}
