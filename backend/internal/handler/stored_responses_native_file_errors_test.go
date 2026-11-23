package handler

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestStoredNativeFileErrorsPreserveResourceClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		status int
		code   string
	}{
		{"expired or unowned file", service.ErrFileNotFound, 404, "file_not_found"},
		{"file authorization revoked", service.ErrFileAuthorizationRevoked, 403, "permission_denied"},
		{"lease retired", service.ErrFileLeaseConflict, 409, "file_lease_conflict"},
		{"busy", service.ErrManagedFileBusy, 409, "managed_file_busy"},
		{"binding changed", service.ErrManagedFileBindingConflict, 409, "managed_file_binding_conflict"},
		{"upload uncertain", service.ErrManagedFileUncertain, 409, "managed_file_uncertain"},
		{"budget", service.ErrManagedFileRecoveryBudget, 429, "managed_file_recovery_budget_exceeded"},
		{"unsupported", service.ErrManagedFileUnsupportedInput, 400, "managed_files_unsupported"},
		{"replay", service.ErrManagedFileGenerationReplay, 409, "managed_file_generation_replay"},
		{"metadata unavailable", errors.New("private-token private-url"), 503, "response_storage_unavailable"},
		{"file not ready", service.ErrManagedFileNotReady, 503, "response_storage_unavailable"},
		{"storage quota", service.ErrStoredResponseQuota, 429, "response_storage_quota_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &service.StoredResponseLocalError{Err: fmt.Errorf("private-token private-url: %w", tc.cause)}
			got := storedResponsePublicErrorFor(err)
			require.Equal(t, tc.status, got.status)
			require.Equal(t, tc.code, got.code)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			handleStoredResponseError(c, err)
			require.Equal(t, tc.status, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "private-token")
			require.NotContains(t, recorder.Body.String(), "private-url")
		})
	}
}
