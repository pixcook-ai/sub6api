package service

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrBPSRequestAuthorizationRevoked     = errors.New("bps request authorization revoked")
	ErrBPSRequestAuthorizationUnavailable = errors.New("bps request authorization unavailable")
)

type BPSRequestReauthorize func(context.Context, string) error

type bpsRequestReauthorizeKey struct{}

func WithBPSRequestReauthorize(ctx context.Context, check BPSRequestReauthorize) context.Context {
	return context.WithValue(ctx, bpsRequestReauthorizeKey{}, check)
}

func BPSRequestReauthorizeFromContext(ctx context.Context) BPSRequestReauthorize {
	check, _ := ctx.Value(bpsRequestReauthorizeKey{}).(BPSRequestReauthorize)
	return check
}

func CheckBPSRequestAuthorization(ctx context.Context, requestedModel string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	check := BPSRequestReauthorizeFromContext(ctx)
	if check == nil || requestedModel == "" || !utf8.ValidString(requestedModel) || strings.TrimSpace(requestedModel) != requestedModel || strings.IndexFunc(requestedModel, unicode.IsControl) >= 0 {
		return ErrBPSRequestAuthorizationUnavailable
	}
	err := check(ctx, requestedModel)
	if cancelled := ctx.Err(); cancelled != nil {
		return cancelled
	}
	// Return fixed errors, even when a callback wraps a sentinel with secrets.
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrBPSRequestAuthorizationRevoked):
		return ErrBPSRequestAuthorizationRevoked
	default:
		return ErrBPSRequestAuthorizationUnavailable
	}
}
