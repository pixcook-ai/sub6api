package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestManagedInlineStagingPreservesIndexesAndCleansActiveReaders(t *testing.T) {
	const body = `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	plan, err = plan.WithPublicFiles([]ManagedInlinePublicFile{{PublicID: "file-public", Bytes: 6}})
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	staged, err := StageManagedInlineFiles(context.Background(), plan, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	files := staged.Files()
	if len(files) != 2 || files[0].PublicID != "file-public" || files[0].SHA256 != "" || files[1].SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || files[1].Position.Part != 1 {
		t.Fatalf("staged files: %+v", files)
	}
	files[1].SHA256 = "mutated"
	if staged.Files()[1].SHA256 == "mutated" {
		t.Fatal("mutable stage snapshot")
	}
	dirs, err := os.ReadDir(parent)
	if err != nil || len(dirs) != 1 {
		t.Fatalf("staging dirs: %v %v", dirs, err)
	}
	dir := filepath.Join(parent, dirs[0].Name())
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("private directory: %v %v", info, err)
	}
	blobs, err := os.ReadDir(dir)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("staging blobs: %v %v", blobs, err)
	}
	info, err = os.Stat(filepath.Join(dir, blobs[0].Name()))
	if err != nil || info.Mode().Perm() != 0600 || blobs[0].Name() == "hello.txt" {
		t.Fatalf("private blob: %v %v", info, err)
	}
	if _, err := staged.Open(0); err == nil {
		t.Fatal("public index opened as inline")
	}
	reader, err := staged.Open(1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "hello" {
		t.Fatalf("staged content: %q %v", data, err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("active reader survives stage close")
	}
	if _, err := staged.Open(1); err == nil {
		t.Fatal("opened closed staging")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	dirs, err = os.ReadDir(parent)
	if err != nil || len(dirs) != 0 {
		t.Fatalf("left private staging: %v %v", dirs, err)
	}
}

func TestManagedInlineStagingRejectsUnbudgetedOrCanceledBeforeDisk(t *testing.T) {
	parent := t.TempDir()
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"}]}]}`), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StageManagedInlineFiles(context.Background(), plan, parent); err == nil {
		t.Fatal("unbudgeted plan staged")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StageManagedInlineFiles(ctx, nil, parent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("touched unapproved storage: %v %v", entries, err)
	}
}

func TestManagedInlineStagingCancellationClosesAndRemovesBlobs(t *testing.T) {
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	staged, err := StageManagedInlineFiles(ctx, plan, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	reader, err := staged.Open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled staging retained blobs")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("canceled reader remains open")
	}
	if _, err := staged.Open(0); err == nil {
		t.Fatal("canceled staging reopened")
	}
}

func TestManagedInlineStagingStreamsNISTMillionAAndConcurrentClose(t *testing.T) {
	payload := strings.Repeat("a", 1_000_000)
	body := fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"large.txt","file_data":%q}]}]}`, base64.StdEncoding.EncodeToString([]byte(payload)))
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	staged, err := StageManagedInlineFiles(context.Background(), plan, parent)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if staged.Files()[0].Bytes != 1_000_000 || staged.Files()[0].SHA256 != "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0" {
		t.Fatalf("large streamed digest: %+v", staged.Files())
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := staged.Open(0)
			if err == nil {
				_, _ = r.Read(make([]byte, 1))
				_ = r.Close()
			}
		}()
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("concurrent close retained blobs: %v %v", entries, err)
	}
}

func TestManagedInlineStagingStorageErrorsDoNotExposePrivatePaths(t *testing.T) {
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"sensitive.txt","file_data":"aGVsbG8="}]}]}`), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "private-owner-value")
	if err := os.WriteFile(parent, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if staged, err := StageManagedInlineFiles(context.Background(), plan, parent); err == nil || staged != nil || strings.Contains(err.Error(), parent) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe storage error: %v", err)
	}
}

func TestManagedInlineStagingENOSPCCleansEarlierAndPartialBlobs(t *testing.T) {
	const body = `{"input":[{"role":"user","content":[{"type":"input_file","filename":"first.txt","file_data":"aGVsbG8="},{"type":"input_file","filename":"second.txt","file_data":"aGVsbG8="}]}]}`
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	fs := defaultManagedInlineStagingFilesystem()
	original := fs.create
	created := 0
	fs.create = func(root *os.Root, name string) (io.WriteCloser, error) {
		file, err := original(root, name)
		if err != nil {
			return nil, err
		}
		created++
		if created == 2 {
			return &inlineWriteFailure{WriteCloser: file, err: syscall.ENOSPC}, nil
		}
		return file, nil
	}
	staged, err := stageManagedInlineFiles(context.Background(), plan, parent, fs)
	if staged != nil || !errors.Is(err, syscall.ENOSPC) || created != 2 {
		t.Fatalf("filesystem ENOSPC: stage=%v err=%v creates=%d", staged != nil, err, created)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed stage retained blobs: %v %v", entries, err)
	}
}

type inlineWriteFailure struct {
	io.WriteCloser
	err error
}

func (f *inlineWriteFailure) Write(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	n, err := f.WriteCloser.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.err
}

func TestManagedInlineStagingWriteCancellationAndCloseFailureCleanBlobs(t *testing.T) {
	body := fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"large.txt","file_data":%q}]}]}`, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 100_000))))
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"write cancellation", "close error", "open root error"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fs := defaultManagedInlineStagingFilesystem()
			create := fs.create
			fs.create = func(root *os.Root, name string) (io.WriteCloser, error) {
				file, err := create(root, name)
				if err != nil {
					return nil, err
				}
				if mode == "write cancellation" {
					return &inlineCancelWriter{WriteCloser: file, cancel: cancel}, nil
				}
				return &inlineCloseFailure{WriteCloser: file}, nil
			}
			if mode == "open root error" {
				fs.openRoot = func(string) (*os.Root, error) { return nil, syscall.EIO }
			}
			staged, err := stageManagedInlineFiles(ctx, plan, parent, fs)
			want := error(syscall.EIO)
			if mode == "write cancellation" {
				want = context.Canceled
			}
			if staged != nil || !errors.Is(err, want) {
				t.Fatalf("expected %v got %v", want, err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed stage retained blobs: %v %v", entries, err)
			}
		})
	}
}

func TestManagedInlineStagingCloseCanRetryFailedRemoval(t *testing.T) {
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	fs := defaultManagedInlineStagingFilesystem()
	removes := 0
	fs.removeAll = func(path string) error {
		removes++
		if removes == 1 {
			return os.ErrPermission
		}
		return os.RemoveAll(path)
	}
	staged, err := stageManagedInlineFiles(context.Background(), plan, parent, fs)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if err := staged.Close(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("removal error lost: %v", err)
	}
	if _, err := staged.Open(0); err == nil {
		t.Fatal("failed removal re-enabled readers")
	}
	if err := staged.Close(); err != nil {
		t.Fatalf("removal retry: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 || removes != 2 {
		t.Fatalf("cleanup retry: %v %v removes=%d", entries, err, removes)
	}
}

type inlineCancelWriter struct {
	io.WriteCloser
	cancel context.CancelFunc
}

func (w *inlineCancelWriter) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	w.cancel()
	return n, err
}

type inlineCloseFailure struct{ io.WriteCloser }

func (w *inlineCloseFailure) Close() error { return errors.Join(w.WriteCloser.Close(), syscall.EIO) }
