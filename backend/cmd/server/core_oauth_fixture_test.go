//go:build coreoauthfixture

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/handler"
	"github.com/MACOS-DO/sub4api/internal/service"
)

// TestCoreOAuthFixtureServe boots the real generated application and router.
// It is intentionally opt-in and only accepts a private, loopback fixture file.
// It is not a production server mode and provides no real OAuth evidence.
func TestCoreOAuthFixtureServe(t *testing.T) {
	if os.Getenv("SUB6_CORE_OAUTH_FIXTURE_RUN") != "1" {
		t.Skip("fixture server disabled")
	}
	configPath := strings.TrimSpace(os.Getenv("CONFIG_FILE"))
	readyPath := strings.TrimSpace(os.Getenv("SUB6_CORE_OAUTH_FIXTURE_READY"))
	if configPath == "" || readyPath == "" {
		t.Fatal("CONFIG_FILE and SUB6_CORE_OAUTH_FIXTURE_READY are required")
	}
	fixture, err := service.CoreOAuthFixtureLoadConfig()
	if err != nil {
		t.Fatal("invalid fixture configuration")
	}
	root := filepath.Dir(configPath)
	resolved, err := filepath.EvalSymlinks(root)
	info, statErr := os.Lstat(root)
	if err != nil || resolved != root || statErr != nil || !info.IsDir() || info.Mode().Perm() != 0700 ||
		root != filepath.Join("/private/tmp", "sub6-core-fixture-"+fixture.RunID) ||
		configPath != filepath.Join(root, "config.yaml") || readyPath != filepath.Join(root, "ready.json") ||
		os.Getenv("DATA_DIR") != root || os.Getenv("SUB6_CORE_OAUTH_FIXTURE_CONFIG") != filepath.Join(root, "oauth.json") {
		t.Fatal("owned fixture directory required")
	}
	var rawConfig map[string]any // Launcher writes JSON, a valid YAML document.
	if service.CoreOAuthFixtureReadPrivate(configPath, &rawConfig) != nil || len(rawConfig) == 0 {
		t.Fatal("private server configuration required")
	}
	cfg, err := config.LoadForBootstrap()
	if err != nil || cfg.Server.Host != "127.0.0.1" || cfg.Server.Port < 1024 || cfg.Server.Port > 65535 ||
		cfg.Database.Host != "127.0.0.1" || cfg.Database.Port < 1024 || cfg.Database.Port == 5432 || cfg.Database.DBName != "sub6_fixture_"+fixture.RunID || cfg.Database.User != "postgres" || cfg.Database.SSLMode != "disable" || len(cfg.Database.Password) != 48 ||
		cfg.Redis.Host != "127.0.0.1" || cfg.Redis.Port < 1024 || cfg.Redis.Port == 6379 || cfg.Redis.DB != 0 ||
		cfg.RunMode != config.RunModeStandard || cfg.Pricing.RemoteURL != "" || cfg.Pricing.HashURL != "" {
		t.Fatal("isolated fixture addresses and database required")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: cfg.Server.Port})
	if err != nil {
		t.Fatal("fixture listener unavailable")
	}
	defer listener.Close()
	app, err := initializeApplication(handler.BuildInfo{Version: "coreoauthfixture", BuildType: "source"})
	if err != nil {
		t.Fatal("fixture application initialization failed")
	}
	defer app.Cleanup()
	if err := seedCoreOAuthFixtureAdmin(cfg, root); err != nil {
		t.Fatal("fixture administrator bootstrap failed")
	}
	host, port, err := net.SplitHostPort(app.Server.Addr)
	if err != nil || host != "127.0.0.1" || app.Server.Addr != listener.Addr().String() {
		t.Fatal("fixture server address changed")
	}
	served := make(chan error, 1)
	go func() { served <- app.Server.Serve(listener) }()
	ready := map[string]any{"contract": "sub6-core-oauth-fixture-server/1", "synthetic": true, "base_url": "http://127.0.0.1:" + port + "/v1", "pid": os.Getpid(), "started_at": time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(ready)
	file, err := os.OpenFile(readyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("fixture readiness path already occupied")
	}
	_, writeErr := file.Write(append(raw, '\n'))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("fixture readiness write failed")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	select {
	case <-signals:
	case err = <-served:
		if err != http.ErrServerClosed {
			t.Error("fixture server exited unexpectedly")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if app.Server.Shutdown(ctx) != nil {
		t.Error("fixture server shutdown failed")
	}
}

func seedCoreOAuthFixtureAdmin(cfg *config.Config, root string) error {
	var input struct {
		Password string `json:"password"`
	}
	if err := service.CoreOAuthFixtureReadPrivate(filepath.Join(root, "admin.json"), &input); err != nil {
		return err
	}
	if !regexp.MustCompile(`^fixture-pass-[0-9a-f]{48}$`).MatchString(input.Password) {
		return os.ErrInvalid
	}
	user := &service.User{}
	if err := user.SetPassword(input.Password); err != nil {
		return err
	}
	db, err := sql.Open("postgres", cfg.Database.DSN())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, `INSERT INTO users (email, password_hash, role, balance, concurrency, status)
		VALUES ($1, $2, 'admin', 0, 3, 'active') ON CONFLICT DO NOTHING`,
		"admin-"+strings.TrimPrefix(cfg.Database.DBName, "sub6_fixture_")+"@example.invalid", user.PasswordHash)
	return err
}
