package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type bpsAuthorizationTestKey struct{}

func TestBPSRequestAuthorizationRequiresServerOwnedCallback(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), WithBPSRequestReauthorize(context.Background(), nil)} {
		require.Nil(t, BPSRequestReauthorizeFromContext(ctx))
		require.ErrorIs(t, CheckBPSRequestAuthorization(ctx, "original-client-model"), ErrBPSRequestAuthorizationUnavailable)
	}
}

func TestBPSRequestAuthorizationPassesOriginalModelAndCurrentContext(t *testing.T) {
	for _, model := range []string{"client-alias/gpt-model", "原客户模型", strings.Repeat("model", 80)} {
		t.Run(model, func(t *testing.T) {
			calls := 0
			ctx := WithBPSRequestReauthorize(context.Background(), func(ctx context.Context, requested string) error {
				calls++
				require.Equal(t, model, requested)
				require.Equal(t, "request-context", ctx.Value(bpsAuthorizationTestKey{}))
				return nil
			})
			ctx = context.WithValue(ctx, bpsAuthorizationTestKey{}, "request-context")
			require.NotNil(t, BPSRequestReauthorizeFromContext(ctx))
			require.NoError(t, CheckBPSRequestAuthorization(ctx, model))
			require.Equal(t, 1, calls)
		})
	}
}

func TestBPSRequestAuthorizationScopesCallbacksToContext(t *testing.T) {
	calls := 0
	parent := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error {
		calls++
		return ErrBPSRequestAuthorizationRevoked
	})
	child := WithBPSRequestReauthorize(parent, func(context.Context, string) error { return nil })
	require.NoError(t, CheckBPSRequestAuthorization(child, "client-model"))
	require.Equal(t, ErrBPSRequestAuthorizationRevoked, CheckBPSRequestAuthorization(parent, "client-model"))
	require.Equal(t, 1, calls)
	nilChild := WithBPSRequestReauthorize(parent, nil)
	require.Nil(t, BPSRequestReauthorizeFromContext(nilChild))
	require.Equal(t, ErrBPSRequestAuthorizationUnavailable, CheckBPSRequestAuthorization(nilChild, "client-model"))
	require.Equal(t, 1, calls, "nil replacement must not invoke an inherited check")
	require.Nil(t, BPSRequestReauthorizeFromContext(context.Background()))
}

func TestBPSRequestAuthorizationNormalizesCallbackErrors(t *testing.T) {
	for _, tc := range []struct {
		name               string
		returned, expected error
	}{
		{"revoked", ErrBPSRequestAuthorizationRevoked, ErrBPSRequestAuthorizationRevoked},
		{"unavailable", ErrBPSRequestAuthorizationUnavailable, ErrBPSRequestAuthorizationUnavailable},
		{"wrapped revoked", fmt.Errorf("secret-token https://private.invalid: %w", ErrBPSRequestAuthorizationRevoked), ErrBPSRequestAuthorizationRevoked},
		{"wrapped unavailable", fmt.Errorf("secret-token: %w", ErrBPSRequestAuthorizationUnavailable), ErrBPSRequestAuthorizationUnavailable},
		{"raw failure", errors.New("secret-token https://private.invalid"), ErrBPSRequestAuthorizationUnavailable},
		{"joined raw failure", errors.Join(errors.New("secret-token"), ErrBPSRequestAuthorizationRevoked), ErrBPSRequestAuthorizationRevoked},
		{"cancelled", fmt.Errorf("secret-token: %w", context.Canceled), context.Canceled},
		{"deadline", fmt.Errorf("secret-token: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error { return tc.returned })
			err := CheckBPSRequestAuthorization(ctx, "client-model")
			require.Equal(t, tc.expected, err)
			require.NotContains(t, err.Error(), "secret-token")
			require.NotContains(t, err.Error(), "private.invalid")
		})
	}
}

func TestBPSRequestAuthorizationRejectsInvalidModelBeforeCallback(t *testing.T) {
	calls := 0
	ctx := WithBPSRequestReauthorize(context.Background(), func(context.Context, string) error { calls++; return nil })
	for _, model := range []string{"", " ", " model", "model ", "model\n", "mo\x00del", "model\u007f", string([]byte{'m', 0xff})} {
		require.Equal(t, ErrBPSRequestAuthorizationUnavailable, CheckBPSRequestAuthorization(ctx, model))
	}
	require.Zero(t, calls)
}

func TestBPSRequestAuthorizationPreservesCancellationBeforeAndAfterCallback(t *testing.T) {
	for _, withCallback := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if withCallback {
			ctx = WithBPSRequestReauthorize(ctx, func(context.Context, string) error { t.Fatal("cancelled check executed"); return nil })
		}
		cancel()
		require.Equal(t, context.Canceled, CheckBPSRequestAuthorization(ctx, "client-model"))
	}
	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	require.Equal(t, context.DeadlineExceeded, CheckBPSRequestAuthorization(deadline, "client-model"))
	for _, returned := range []error{nil, ErrBPSRequestAuthorizationRevoked, errors.New("secret-token")} {
		ctx, cancel := context.WithCancel(context.Background())
		ctx = WithBPSRequestReauthorize(ctx, func(context.Context, string) error { cancel(); return returned })
		require.Equal(t, context.Canceled, CheckBPSRequestAuthorization(ctx, "client-model"))
		cancel()
	}
}
