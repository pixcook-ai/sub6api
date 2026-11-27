package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
)

var batchLoginCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,96}$`)

// batchLoginRecord holds only the whitelisted fields emitted by run-login.mjs.
type batchLoginRecord struct {
	Provider     string `json:"provider"`
	Email        string `json:"email"`
	Subject      string `json:"subject"`
	WorkspaceID  string `json:"workspaceId"`
	ClientID     string `json:"clientId"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken"`
	ExpiresAt    string `json:"expiresAt"`
}

// batchLoginRunFunc performs one login. On failure it returns a nil record and a
// fixed error code; credentials and provider output are never returned.
type batchLoginRunFunc func(ctx context.Context, provider string, entry BatchLoginEntry, proxyURL string, onProgress func(string)) (*batchLoginRecord, string)

func batchLoginScript(cfg config.BatchLoginConfig) string {
	return filepath.Join(cfg.ToolsDir, "run-login.mjs")
}

func newBatchLoginExecRunner(cfg config.BatchLoginConfig) batchLoginRunFunc {
	return func(ctx context.Context, provider string, entry BatchLoginEntry, proxyURL string, onProgress func(string)) (*batchLoginRecord, string) {
		node := strings.TrimSpace(cfg.NodePath)
		if node == "" {
			found, err := exec.LookPath("node")
			if err != nil {
				return nil, "runtime_unavailable"
			}
			node = found
		}
		timeout := time.Duration(cfg.TaskTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Secrets travel only through the child's environment, never argv.
		cmd := exec.CommandContext(runCtx, node, batchLoginScript(cfg), "--email", entry.Email, "--provider", provider)
		cmd.Dir = cfg.ToolsDir
		cmd.Env = batchLoginEnv(entry, proxyURL)
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 5 * time.Second
		cmd.Stderr = &batchLoginProgressWriter{onProgress: onProgress}

		out, runErr := cmd.Output()
		if ctx.Err() != nil {
			return nil, "canceled"
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, "login_timeout"
		}
		return parseBatchLoginOutput(out, runErr)
	}
}

// Only fixed progress labels cross the subprocess boundary. Provider stderr is
// otherwise discarded and never included in job snapshots or process errors.
type batchLoginProgressWriter struct {
	line       []byte
	discarding bool
	onProgress func(string)
}

func (w *batchLoginProgressWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			if !w.discarding {
				w.report()
			}
			w.line = w.line[:0]
			w.discarding = false
			continue
		}
		if w.discarding {
			continue
		}
		if len(w.line) >= 256 {
			w.line = w.line[:0]
			w.discarding = true
			continue
		}
		w.line = append(w.line, value)
	}
	return len(data), nil
}

func (w *batchLoginProgressWriter) report() {
	var event struct {
		Type  string `json:"type"`
		Stage string `json:"stage"`
	}
	if json.Unmarshal(w.line, &event) != nil || event.Type != "progress" || !isBatchLoginStage(event.Stage) {
		return
	}
	if w.onProgress != nil {
		w.onProgress(event.Stage)
	}
}

func isBatchLoginStage(stage string) bool {
	switch stage {
	case "proxy_check", "timezone_check", "authorize", "email", "password", "totp", "workspace", "token_exchange", "create_account":
		return true
	default:
		return false
	}
}

func batchLoginEnv(entry BatchLoginEntry, proxyURL string) []string {
	skip := []string{"CHATGPT_LOGIN_PASSWORD=", "CHATGPT_TOTP_SECRET=", "CHATGPT_PROXY_URL="}
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		drop := false
		for _, prefix := range skip {
			drop = drop || strings.HasPrefix(kv, prefix)
		}
		if !drop {
			env = append(env, kv)
		}
	}
	env = append(env, "CHATGPT_LOGIN_PASSWORD="+entry.Password, "CHATGPT_TOTP_SECRET="+entry.TOTPSecret)
	if proxyURL != "" {
		env = append(env, "CHATGPT_PROXY_URL="+proxyURL)
	}
	return env
}

func parseBatchLoginOutput(out []byte, runErr error) (*batchLoginRecord, string) {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	last := bytes.TrimSpace(lines[len(lines)-1])
	if len(last) == 0 {
		return nil, "login_failed"
	}
	var payload struct {
		OK     bool              `json:"ok"`
		Code   string            `json:"code"`
		Record *batchLoginRecord `json:"record"`
	}
	if err := json.Unmarshal(last, &payload); err != nil {
		return nil, "login_failed"
	}
	if payload.OK && payload.Record != nil && runErr == nil {
		return payload.Record, ""
	}
	if batchLoginCodePattern.MatchString(payload.Code) {
		return nil, payload.Code
	}
	return nil, "login_failed"
}
