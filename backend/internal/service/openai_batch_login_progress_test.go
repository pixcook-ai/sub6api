package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
)

func TestBatchLoginProgressWriterFiltersAndFramesEvents(t *testing.T) {
	var stages []string
	w := &batchLoginProgressWriter{onProgress: func(stage string) { stages = append(stages, stage) }}
	chunks := []string{
		"private provider message\n{\"type\":\"progress\",\"sta",
		"ge\":\"password\"}\n{\"type\":\"progress\",\"stage\":\"private-token\"}\n",
		"{\"type\":\"oauth_tokens\",\"stage\":\"totp\"}\n",
		strings.Repeat("x", 300),
		"{\"type\":\"progress\",\"stage\":\"email\"}\n",
		"{\"type\":\"progress\",\"stage\":\"totp\"}\n",
	}
	for _, chunk := range chunks {
		n, err := w.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("write returned %d, %v", n, err)
		}
	}
	if !reflect.DeepEqual(stages, []string{"password", "totp"}) {
		t.Fatalf("unexpected progress %v", stages)
	}
}

func TestBatchLoginExecReportsProgressBeforeExit(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is unavailable")
	}
	dir := t.TempDir()
	gate := filepath.Join(dir, "finish")
	script := `import fs from "node:fs";
process.stderr.write('{"type":"progress","sta');
process.stderr.write('ge":"password"}\n');
const timer = setInterval(() => {
  if (!fs.existsSync(` + strconv.Quote(gate) + `)) return;
  clearInterval(timer);
  process.stdout.write('{"ok":true,"record":{"email":"fixture@example.invalid"}}\n');
}, 10);
`
	if err := os.WriteFile(filepath.Join(dir, "run-login.mjs"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := newBatchLoginExecRunner(config.BatchLoginConfig{ToolsDir: dir, NodePath: node, TaskTimeoutSeconds: 5})
	progress := make(chan string, 1)
	type result struct {
		record *batchLoginRecord
		code   string
	}
	done := make(chan result, 1)
	go func() {
		record, code := runner(ctx, "codex", entries("fixture@example.invalid")[0], "", func(stage string) {
			progress <- stage
		})
		done <- result{record: record, code: code}
	}()
	select {
	case stage := <-progress:
		if stage != "password" {
			t.Fatalf("stage %q", stage)
		}
	case <-done:
		t.Fatal("runner exited before progress was observed")
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not report progress before exit")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.code != "" || got.record == nil || got.record.Email != "fixture@example.invalid" {
			t.Fatalf("unexpected result %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not finish")
	}
}

type progressBatchAdmin struct {
	fakeBatchAdmin
	creating chan struct{}
	release  chan struct{}
}

func (a *progressBatchAdmin) CreateAccount(ctx context.Context, in *CreateAccountInput) (*Account, error) {
	close(a.creating)
	<-a.release
	return a.fakeBatchAdmin.CreateAccount(ctx, in)
}

func TestBatchLoginSnapshotTracksProgressAndClearsItAfterCreation(t *testing.T) {
	loginReady := make(chan struct{})
	releaseLogin := make(chan struct{})
	admin := &progressBatchAdmin{creating: make(chan struct{}), release: make(chan struct{})}
	var lateProgress func(string)
	runner := func(ctx context.Context, provider string, entry BatchLoginEntry, proxy string, onProgress func(string)) (*batchLoginRecord, string) {
		lateProgress = onProgress
		onProgress("password")
		onProgress("invalid-private-text")
		close(loginReady)
		<-releaseLogin
		return bpsRecord(entry.Email), ""
	}
	svc := newBatchLoginTestService(t, admin, runner, 1)
	job, err := svc.Start(context.Background(), &BatchLoginInput{Entries: entries("fixture@example.invalid"), Targets: []string{"bps"}})
	if err != nil {
		t.Fatal(err)
	}
	<-loginReady
	current, _ := svc.Get(job.ID)
	if current.Items[0].Stage != "password" || current.Items[0].Status != BatchLoginItemRunning {
		t.Fatalf("unexpected login progress %+v", current.Items[0])
	}
	close(releaseLogin)
	<-admin.creating
	current, _ = svc.Get(job.ID)
	if current.Items[0].Stage != "create_account" {
		t.Fatalf("unexpected creation progress %+v", current.Items[0])
	}
	close(admin.release)
	done := waitBatchLoginJob(t, svc, job.ID)
	if done.Items[0].Status != BatchLoginItemCreated || done.Items[0].Stage != "" {
		t.Fatalf("unexpected terminal item %+v", done.Items[0])
	}
	lateProgress("totp")
	done, _ = svc.Get(job.ID)
	if done.Items[0].Stage != "" {
		t.Fatal("late progress overwrote terminal state")
	}
}
