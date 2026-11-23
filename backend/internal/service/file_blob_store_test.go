package service

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const testFileBlobKey = "0123456789abcdef0123456789abcdef"

func TestPrivateFileBlobStorePersistsCommittedContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	store, err := NewPrivateFileBlobStore(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	size, digest, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("hello"), 5)
	if err != nil || size != 5 || digest != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("stage = (%d, %q, %v)", size, digest, err)
	}
	if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted content is visible: %v", err)
	}
	if err := store.Commit(context.Background(), testFileBlobKey); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPrivateFileBlobStore(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := reopened.Open(context.Background(), testFileBlobKey)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(content) != "hello" {
		t.Fatalf("persisted content = %q, %v", content, err)
	}
	for name, mode := range map[string]os.FileMode{
		root: 0o700, filepath.Join(root, "staging"): 0o700,
		filepath.Join(root, "blobs"): 0o700, filepath.Join(root, "blobs", testFileBlobKey): 0o600,
	} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("private permissions for %s: %v, %v", name, info, err)
		}
	}
	if err := reopened.Remove(context.Background(), testFileBlobKey); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Remove(context.Background(), testFileBlobKey); err != nil {
		t.Fatalf("remove is not idempotent: %v", err)
	}
	if _, err := reopened.Open(context.Background(), testFileBlobKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed content is still visible: %v", err)
	}
}

type fileBlobCancellingReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *fileBlobCancellingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, "partial")
	r.cancel()
	return n, nil
}

func TestPrivateFileBlobStoreCancelledStageRemovesPartialBytes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	store, err := NewPrivateFileBlobStore(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err = store.Stage(ctx, testFileBlobKey, &fileBlobCancellingReader{cancel: cancel}, 64)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stage returned %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "staging", testFileBlobKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled staging bytes remained: %v", err)
	}
}

func TestPrivateFileBlobStoreReservePreventsStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	store, err := NewPrivateFileBlobStore(root, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	free, err := store.FreeBytes(context.Background())
	if err != nil || free <= 0 {
		t.Fatalf("free bytes = %d, %v", free, err)
	}
	_, _, err = store.Stage(context.Background(), testFileBlobKey, strings.NewReader("hello"), 5)
	if !errors.Is(err, ErrFileStorageFull) {
		t.Fatalf("reserved disk was consumed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "staging", testFileBlobKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disk-full staging bytes remained: %v", err)
	}
}

func TestPrivateFileBlobStoreDoesNotOverwriteCommittedBlob(t *testing.T) {
	store, err := NewPrivateFileBlobStore(filepath.Join(t.TempDir(), "private"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("original"), 64); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), testFileBlobKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("replacement"), 64); !errors.Is(err, ErrFileBlobExists) {
		t.Fatalf("committed key was reusable: %v", err)
	}
	reader, err := store.Open(context.Background(), testFileBlobKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil || string(content) != "original" {
		t.Fatalf("committed bytes changed: %q, %v", content, err)
	}
}

func TestPrivateFileBlobStoreRejectsUnsafePaths(t *testing.T) {
	for _, directory := range []string{"root", "staging", "blobs"} {
		t.Run("directory-"+directory, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "private")
			outside := filepath.Join(base, "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if directory != "root" {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := root
			if directory != "root" {
				link = filepath.Join(root, directory)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			if _, err := NewPrivateFileBlobStore(root, 0); !errors.Is(err, ErrUnsafeFileBlobPath) {
				t.Fatalf("symlink directory accepted: %v", err)
			}
		})
	}
	for _, location := range []string{"staging", "blobs"} {
		t.Run("file-"+location, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private")
			store, err := NewPrivateFileBlobStore(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "untouched")
			if err := os.WriteFile(outside, []byte("private-other-content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, location, testFileBlobKey)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("replacement"), 64); !errors.Is(err, ErrUnsafeFileBlobPath) {
				t.Fatalf("stage followed symlink: %v", err)
			}
			if location == "blobs" {
				if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, ErrUnsafeFileBlobPath) {
					t.Fatalf("open followed symlink: %v", err)
				}
			}
			if err := store.Commit(context.Background(), testFileBlobKey); !errors.Is(err, ErrUnsafeFileBlobPath) {
				t.Fatalf("commit accepted symlink: %v", err)
			}
			if err := store.Remove(context.Background(), testFileBlobKey); !errors.Is(err, ErrUnsafeFileBlobPath) {
				t.Fatalf("remove accepted symlink: %v", err)
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "private-other-content" {
				t.Fatalf("symlink target was modified: %q, %v", data, err)
			}
		})
	}
}

func TestPrivateFileBlobStoreRejectsReplacedDirectoriesAndNonregularFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	store, err := NewPrivateFileBlobStore(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "blobs", testFileBlobKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, ErrUnsafeFileBlobPath) {
		t.Fatalf("nonregular file accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "blobs", testFileBlobKey)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "blobs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "blobs")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, ErrUnsafeFileBlobPath) {
		t.Fatalf("replaced blob directory accepted: %v", err)
	}
	if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("x"), 4); !errors.Is(err, ErrUnsafeFileBlobPath) {
		t.Fatalf("stage accepted replaced directory: %v", err)
	}
}

type fileBlobErrorReader struct{ err error }

func (r fileBlobErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestPrivateFileBlobStoreFailedStagesLeaveNoBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader io.Reader
		limit  int64
		want   error
	}{
		{"too-large", strings.NewReader("over-limit"), 4, ErrFileTooLarge},
		{"no-space", io.MultiReader(strings.NewReader("partial"), fileBlobErrorReader{unix.ENOSPC}), 64, ErrFileStorageFull},
		{"quota", io.MultiReader(strings.NewReader("partial"), fileBlobErrorReader{unix.EDQUOT}), 64, ErrFileStorageFull},
		{"reader-failure", io.MultiReader(strings.NewReader("partial"), fileBlobErrorReader{io.ErrUnexpectedEOF}), 64, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private")
			store, err := NewPrivateFileBlobStore(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, digest, err := store.Stage(context.Background(), testFileBlobKey, tc.reader, tc.limit)
			if !errors.Is(err, tc.want) || digest != "" {
				t.Fatalf("failed stage = digest %q, err %v", digest, err)
			}
			if _, err := os.Lstat(filepath.Join(root, "staging", testFileBlobKey)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed staging bytes remained: %v", err)
			}
			if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed bytes published: %v", err)
			}
		})
	}
}

func TestPrivateFileBlobStoreValidatesAllPublicKeysAndCancellation(t *testing.T) {
	store, err := NewPrivateFileBlobStore(filepath.Join(t.TempDir(), "private"), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "../escape", strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("0", 33)} {
		if _, _, err := store.Stage(context.Background(), key, strings.NewReader("x"), 4); !errors.Is(err, ErrInvalidFileBlobKey) {
			t.Fatalf("stage accepted key %q: %v", key, err)
		}
		if _, err := store.Open(context.Background(), key); !errors.Is(err, ErrInvalidFileBlobKey) {
			t.Fatalf("open accepted key %q: %v", key, err)
		}
		for _, call := range []func(context.Context, string) error{store.Commit, store.Remove, store.CleanupStaging} {
			if err := call(context.Background(), key); !errors.Is(err, ErrInvalidFileBlobKey) {
				t.Fatalf("operation accepted key %q: %v", key, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Stage(ctx, testFileBlobKey, strings.NewReader("x"), 4); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, testFileBlobKey); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := store.FreeBytes(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, call := range []func(context.Context, string) error{store.Commit, store.Remove, store.CleanupStaging} {
		if err := call(ctx, testFileBlobKey); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}

func TestPrivateFileBlobStoreRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		root    string
		reserve int64
	}{
		{"", 0}, {string(filepath.Separator), 0}, {filepath.Join(t.TempDir(), "private"), -1},
	} {
		if _, err := NewPrivateFileBlobStore(tc.root, tc.reserve); err == nil {
			t.Fatalf("invalid config accepted: %q, %d", tc.root, tc.reserve)
		}
	}
}

type fileBlobZeroReader struct{}

func (fileBlobZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// This is a deployment fault check, not a red-first unit slice: the existing
// storage-space checks must also hold on a real bounded filesystem.
func TestPrivateFileBlobStoreRealVolumeExhaustion(t *testing.T) {
	root := os.Getenv("FILES_TEST_SMALL_VOLUME")
	if root == "" {
		t.Skip("set FILES_TEST_SMALL_VOLUME to an isolated filesystem limited to 1 MiB")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("FILES_TEST_SMALL_VOLUME must be an absolute directory")
	}
	store, err := NewPrivateFileBlobStore(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := store.Stage(context.Background(), testFileBlobKey, io.LimitReader(fileBlobZeroReader{}, 8<<20), 8<<20)
	if !errors.Is(err, ErrFileStorageFull) || digest != "" {
		t.Fatalf("real bounded filesystem returned digest %q, err %v", digest, err)
	}
	for _, directory := range []string{"staging", "blobs"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil || len(entries) != 0 {
			t.Fatalf("real filesystem failure left %s entries: %v, %v", directory, entries, err)
		}
	}
	if _, err := store.Open(context.Background(), testFileBlobKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real filesystem failure published bytes: %v", err)
	}
}

func TestPrivateFileBlobStoreRejectsDirectoryReplacement(t *testing.T) {
	for _, directory := range []string{"root", "staging", "blobs"} {
		t.Run(directory, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "private")
			store, err := NewPrivateFileBlobStore(root, 0)
			if err != nil {
				t.Fatal(err)
			}
			target := root
			if directory != "root" {
				target = filepath.Join(root, directory)
			}
			if err := os.Rename(target, target+"-previous"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if directory == "root" {
				for _, subdir := range []string{"staging", "blobs"} {
					if err := os.Mkdir(filepath.Join(root, subdir), 0o700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if directory == "staging" {
				if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("must-not-write"), 64); !errors.Is(err, ErrUnsafeFileBlobPath) {
					t.Fatalf("replaced staging directory accepted: %v", err)
				}
				return
			}
			if err := os.WriteFile(filepath.Join(root, "blobs", testFileBlobKey), []byte("foreign bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			reader, err := store.Open(context.Background(), testFileBlobKey)
			if reader != nil {
				_ = reader.Close()
			}
			if !errors.Is(err, ErrUnsafeFileBlobPath) {
				t.Fatalf("replaced %s directory accepted: %v", directory, err)
			}
		})
	}
}

func TestPrivateFileBlobStoreRejectsAncestorSymlinkAfterConstruction(t *testing.T) {
	ancestor := filepath.Join(t.TempDir(), "parent")
	root := filepath.Join(ancestor, "private")
	store, err := NewPrivateFileBlobStore(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(ancestor, ancestor+"-previous"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ancestor+"-previous", ancestor); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("must-not-write"), 64); !errors.Is(err, ErrUnsafeFileBlobPath) {
		t.Fatalf("ancestor symlink accepted: %v", err)
	}
}

func TestPrivateFileBlobStoreCanonicalizesInitiallyTrustedAncestorAlias(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical")
	other := filepath.Join(base, "other")
	for _, dir := range []string{physical, other} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	store, err := NewPrivateFileBlobStore(filepath.Join(alias, "private"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stage(context.Background(), testFileBlobKey, strings.NewReader("original volume"), 64); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), testFileBlobKey); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(physical, "private", "blobs", testFileBlobKey))
	if err != nil || string(data) != "original volume" {
		t.Fatalf("initial canonical location lost: %q, %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(other, "private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retargeted alias was used: %v", err)
	}
}
