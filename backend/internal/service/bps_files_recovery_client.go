package service

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func (b *BPSFilesBackend) AdmitRecoverable(ctx context.Context, m managedfiles.AdmitManifest, readers map[string]io.Reader) (managedfiles.AdmitResult, *managedfiles.RecoveryMissing, error) {
	upstream := &bpsFilesRecoveryUpstream{HTTPUpstream: b.upstream, manifest: m}
	backend := *b
	backend.upstream = upstream
	result, err := backend.Admit(ctx, m, readers)
	if upstream.missing != nil && ctx.Err() == nil {
		return managedfiles.AdmitResult{}, upstream.missing, nil
	}
	return result, nil, err
}

// Capture only this Admit response; retain the existing multipart and success
// validation and restore the body for its ordinary rejection/close path.
type bpsFilesRecoveryUpstream struct {
	HTTPUpstream
	manifest managedfiles.AdmitManifest
	missing  *managedfiles.RecoveryMissing
}

func (u *bpsFilesRecoveryUpstream) Do(req *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	resp, err := u.HTTPUpstream.Do(req, proxy, accountID, concurrency)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusConflict {
		return resp, err
	}
	body := resp.Body
	raw, readErr := io.ReadAll(io.LimitReader(body, bpsFilesResponseLimit+1))
	resp.Body = &bpsFilesRecoveryBody{Reader: io.MultiReader(bytes.NewReader(raw), body), Closer: body}
	if readErr == nil && int64(len(raw)) <= bpsFilesResponseLimit && req.Context().Err() == nil {
		u.missing, _ = managedfiles.ParseRecoveryMissing(resp.StatusCode, raw, u.manifest)
	}
	return resp, nil
}

type bpsFilesRecoveryBody struct {
	io.Reader
	io.Closer
}
