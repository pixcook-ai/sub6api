package admin

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

const oauthControllersEnv = "SUB6_OAUTH_CONTROLLERS"

type oauthControllerConfig struct {
	KeyEnv      string          `json:"key_env"`
	ProfileRefs []string        `json:"profile_refs"`
	BackendIDs  []string        `json:"backend_ids"`
	AccountIDs  json.RawMessage `json:"account_ids"`
}

type oauthControllerScope struct {
	keyHash    [sha256.Size]byte
	profiles   map[string]bool
	backends   map[string]bool
	accountIDs map[int64]bool
}

var errOAuthControllerConfig = errors.New("invalid OAuth controller configuration")

func oauthControllerStrings(values []string) (map[string]bool, error) {
	if len(values) == 0 {
		return nil, errOAuthControllerConfig
	}
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || value == "*" || strings.TrimSpace(value) != value || set[value] {
			return nil, errOAuthControllerConfig
		}
		set[value] = true
	}
	return set, nil
}

func parseOAuthControllers(raw string, lookup func(string) (string, bool)) ([]oauthControllerScope, error) {
	var entries []json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(raw))
	if decoder.Decode(&entries) != nil || len(entries) == 0 || len(entries) > 128 {
		return nil, errOAuthControllerConfig
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, errOAuthControllerConfig
	}
	seenEnv, seenHash := map[string]bool{}, map[[sha256.Size]byte]bool{}
	scopes := make([]oauthControllerScope, 0, len(entries))
	for _, entry := range entries {
		if !managedOAuthUniqueJSONFields(entry) {
			return nil, errOAuthControllerConfig
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil {
			return nil, errOAuthControllerConfig
		}
		for name := range fields {
			if name != "key_env" && name != "profile_refs" && name != "backend_ids" && name != "account_ids" {
				return nil, errOAuthControllerConfig
			}
		}
		var input oauthControllerConfig
		decoder := json.NewDecoder(bytes.NewReader(entry))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !oauthControllerEnvName(input.KeyEnv) || seenEnv[input.KeyEnv] {
			return nil, errOAuthControllerConfig
		}
		secret, ok := lookup(input.KeyEnv)
		if !ok || len(secret) < 32 || len(secret) > 4096 {
			return nil, errOAuthControllerConfig
		}
		for _, b := range []byte(secret) {
			if b <= 32 || b > 126 {
				return nil, errOAuthControllerConfig
			}
		}
		scope := oauthControllerScope{keyHash: sha256.Sum256([]byte(secret))}
		if seenHash[scope.keyHash] {
			return nil, errOAuthControllerConfig
		}
		var err error
		if scope.profiles, err = oauthControllerStrings(input.ProfileRefs); err != nil {
			return nil, err
		}
		if scope.backends, err = oauthControllerStrings(input.BackendIDs); err != nil {
			return nil, err
		}
		if len(input.AccountIDs) > 0 {
			var ids []int64
			if json.Unmarshal(input.AccountIDs, &ids) != nil || len(ids) == 0 {
				return nil, errOAuthControllerConfig
			}
			scope.accountIDs = make(map[int64]bool, len(ids))
			for _, id := range ids {
				if id <= 0 || scope.accountIDs[id] {
					return nil, errOAuthControllerConfig
				}
				scope.accountIDs[id] = true
			}
		}
		seenEnv[input.KeyEnv], seenHash[scope.keyHash] = true, true
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

func oauthControllerEnvName(name string) bool {
	if name == "" || name == oauthControllersEnv {
		return false
	}
	for i, b := range []byte(name) {
		if b != '_' && (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (i == 0 || b < '0' || b > '9') {
			return false
		}
	}
	return true
}

// NewOAuthControllerAuth loads only hashed keys once; malformed configuration disables all controller access.
func NewOAuthControllerAuth(h *AccountHandler) gin.HandlerFunc {
	raw := os.Getenv(oauthControllersEnv)
	var scopes []oauthControllerScope
	var configErr error
	if raw != "" {
		scopes, configErr = parseOAuthControllers(raw, os.LookupEnv)
	}
	var persistent service.ManagedOAuthControllerService
	if h != nil {
		persistent, _ = h.adminService.(service.ManagedOAuthControllerService)
	}
	return func(c *gin.Context) {
		if configErr != nil || h == nil || (len(scopes) == 0 && persistent == nil) {
			response.Error(c, http.StatusServiceUnavailable, "OAuth controller unavailable")
			c.Abort()
			return
		}
		values := c.Request.Header.Values("Authorization")
		var token string
		if len(values) == 1 {
			parts := strings.Split(values[0], " ")
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				token = parts[1]
			}
		}
		match := -1
		if token != "" && len(token) <= 4096 {
			hash := sha256.Sum256([]byte(token))
			for i := range scopes {
				if subtle.ConstantTimeCompare(hash[:], scopes[i].keyHash[:]) == 1 {
					match = i
				}
			}
		}
		var binding *service.ManagedOAuthControllerKeyState
		if token != "" && len(token) <= 4096 && persistent != nil {
			var err error
			binding, err = persistent.AuthenticateManagedOAuthControllerKey(c.Request.Context(), token)
			if err != nil && !errors.Is(err, service.ErrManagedOAuthControllerKeyUnknown) {
				response.Unauthorized(c, "OAuth controller authorization required")
				c.Abort()
				return
			}
			if err != nil {
				binding = nil
			}
		}
		if binding == nil && match < 0 {
			response.Unauthorized(c, "OAuth controller authorization required")
			c.Abort()
			return
		}
		admin, ok := h.adminService.(service.ManagedOAuthAdminService)
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		var scope oauthControllerScope
		if binding == nil {
			scope = scopes[match]
		}
		if !ok || err != nil || id <= 0 || (binding != nil && (!binding.Active || binding.AccountID != id || binding.ManagedOAuthIdentity.Validate() != nil)) || (binding == nil && scope.accountIDs != nil && !scope.accountIDs[id]) {
			response.Forbidden(c, "OAuth controller account access denied")
			c.Abort()
			return
		}
		// Metadata is immutable after registration; neither an AT nor administrator status grants scope.
		state, err := admin.GetManagedOAuthState(c.Request.Context(), id)
		if err != nil || state == nil || state.PublicationFenceOperationID != "" || state.AccountID != id || state.ManagedOAuthIdentity.Validate() != nil || (binding != nil && state.ManagedOAuthIdentity != binding.ManagedOAuthIdentity) || (binding == nil && (!scope.profiles[state.ProfileRef] || !scope.backends[state.BackendID])) {
			response.Forbidden(c, "OAuth controller account access denied")
			c.Abort()
			return
		}
		c.Next()
	}
}
