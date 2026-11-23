package handler

import (
	"net/http"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/tidwall/gjson"
)

// Scan only the top-level control without decoding large attachment payloads.
// Every occurrence participates, including escaped spellings of the same key.
func localStoredResponseStoreIntent(body []byte, defaultStore bool) (bool, error) {
	value, count, valid := defaultStore, 0, true
	gjson.ParseBytes(body).ForEach(func(key, result gjson.Result) bool {
		if key.String() != "store" {
			return true
		}
		count++
		if count > 1 || (result.Type != gjson.True && result.Type != gjson.False) {
			valid = false
			return false
		}
		value = result.Type == gjson.True
		return true
	})
	if !valid {
		return false, service.ErrStoredResponseInvalid
	}
	return value, nil
}

func unsupportedLocalStoredResponseCombination() error {
	return &managedfiles.ResponsesPreflightError{
		Status: http.StatusBadRequest,
		Type:   "invalid_request_error",
		Code:   "unsupported_storage_combination",
		Param:  "store",
		Message: "Local response storage currently supports text-only requests. " +
			"Set store:false to use supported tools or files.",
	}
}

func unsupportedLocalNativeContinuation() error {
	return &managedfiles.ResponsesPreflightError{
		Status:  http.StatusBadRequest,
		Type:    "invalid_request_error",
		Code:    "unsupported_storage_combination",
		Param:   "store",
		Message: "Local response history requires store:true. Stateless native continuation is not supported.",
	}
}
