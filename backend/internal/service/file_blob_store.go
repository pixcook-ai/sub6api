package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	ErrFileTooLarge       = errors.New("file exceeds storage byte limit")
	ErrFileStorageFull    = errors.New("file storage has insufficient free space")
	ErrInvalidFileBlobKey = errors.New("invalid private file storage key")
	ErrUnsafeFileBlobPath = errors.New("private file storage path is unsafe")
	ErrFileBlobExists     = errors.New("private file storage key already exists")
)

// PrivateFileBlobStore owns only private local bytes; the caller owns file
// authorization, metadata and the decision to publish a committed reference.
type PrivateFileBlobStore struct {
	root         string
	reserveBytes int64
	writeMu      sync.Mutex
	rootID       fileBlobDirectoryID
	stagingID    fileBlobDirectoryID
	blobsID      fileBlobDirectoryID
	ready        bool
}

type fileBlobDirectoryID struct{ device, inode uint64 }

func NewPrivateFileBlobStore(root string, reserveBytes int64) (*PrivateFileBlobStore, error) {
	if root == "" || reserveBytes < 0 {
		return nil, errors.New("invalid private file storage configuration")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if root == string(filepath.Separator) {
		return nil, errors.New("private file storage cannot use the filesystem root")
	}
	if info, err := os.Lstat(root); err == nil && !info.IsDir() {
		return nil, ErrUnsafeFileBlobPath
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fileBlobError(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fileBlobError(err)
	}
	// The operator-supplied ancestors are trusted only during construction.
	// Canonicalize aliases such as macOS /var; later opens reject every symlink.
	parent, err := filepath.EvalSymlinks(filepath.Dir(root))
	if err != nil {
		return nil, fileBlobError(err)
	}
	root = filepath.Join(parent, filepath.Base(root))
	store := &PrivateFileBlobStore{root: root, reserveBytes: reserveBytes}
	rootFD, err := store.openRoot()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	store.rootID, err = privateFileBlobDirectoryID(rootFD)
	if err != nil {
		return nil, err
	}
	if err := unix.Fchmod(rootFD, 0o700); err != nil {
		return nil, err
	}
	for _, dir := range []string{"staging", "blobs"} {
		if err := unix.Mkdirat(rootFD, dir, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, fileBlobError(err)
		}
		fd, err := store.openDirectory(rootFD, dir)
		if err != nil {
			return nil, err
		}
		id, identityErr := privateFileBlobDirectoryID(fd)
		err = unix.Fchmod(fd, 0o700)
		_ = unix.Close(fd)
		if identityErr != nil {
			return nil, identityErr
		}
		if err != nil {
			return nil, err
		}
		if dir == "staging" {
			store.stagingID = id
		} else {
			store.blobsID = id
		}
	}
	if err := unix.Fsync(rootFD); err != nil {
		return nil, fileBlobError(err)
	}
	store.ready = true
	return store, nil
}

func (s *PrivateFileBlobStore) Stage(ctx context.Context, key string, src io.Reader, maxBytes int64) (size int64, sha256Hex string, err error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	if err := validateFileBlobKey(key); err != nil {
		return 0, "", err
	}
	if src == nil || maxBytes <= 0 {
		return 0, "", ErrFileTooLarge
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return 0, "", err
	}
	defer unix.Close(rootFD)
	blobsFD, err := s.openDirectory(rootFD, "blobs")
	if err != nil {
		return 0, "", err
	}
	err = ensureFileBlobMissing(blobsFD, key)
	_ = unix.Close(blobsFD)
	if err != nil {
		return 0, "", err
	}
	stagingFD, err := s.openDirectory(rootFD, "staging")
	if err != nil {
		return 0, "", err
	}
	defer unix.Close(stagingFD)
	fd, err := unix.Openat(stagingFD, key, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			existingErr := ensureFileBlobMissing(stagingFD, key)
			if existingErr == nil {
				existingErr = ErrFileBlobExists
			}
			return 0, "", existingErr
		}
		return 0, "", fileBlobError(err)
	}
	file := os.NewFile(uintptr(fd), key)
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err, sha256Hex = fileBlobError(closeErr), ""
		}
		if err != nil {
			if cleanupErr := unix.Unlinkat(stagingFD, key, 0); cleanupErr != nil && !errors.Is(cleanupErr, unix.ENOENT) {
				err = errors.Join(err, fileBlobError(cleanupErr))
			}
			err = errors.Join(err, fileBlobError(unix.Fsync(stagingFD)))
		}
	}()
	hash := sha256.New()
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	size, err = io.Copy(io.MultiWriter(fileBlobReserveWriter{s, file, rootFD}, hash), io.LimitReader(fileBlobContextReader{ctx, src}, limit))
	if err != nil {
		return size, "", fileBlobError(err)
	}
	if size > maxBytes {
		return size, "", ErrFileTooLarge
	}
	if err = ctx.Err(); err != nil {
		return size, "", err
	}
	if err = file.Sync(); err != nil {
		return size, "", fileBlobError(err)
	}
	free, err := availableFileBlobBytes(rootFD)
	if err != nil {
		return size, "", err
	}
	if free < s.reserveBytes {
		return size, "", ErrFileStorageFull
	}
	if err = unix.Fsync(stagingFD); err != nil {
		return size, "", fileBlobError(err)
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *PrivateFileBlobStore) Commit(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFileBlobKey(key); err != nil {
		return err
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	stagingFD, err := s.openDirectory(rootFD, "staging")
	if err != nil {
		return err
	}
	defer unix.Close(stagingFD)
	blobsFD, err := s.openDirectory(rootFD, "blobs")
	if err != nil {
		return err
	}
	defer unix.Close(blobsFD)
	if err := ensureFileBlobMissing(blobsFD, key); err != nil {
		return err
	}
	file, err := openPrivateFileBlob(stagingFD, key)
	if err != nil {
		return err
	}
	_ = file.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Renameat(stagingFD, key, blobsFD, key); err != nil {
		return fileBlobError(err)
	}
	if err := unix.Fsync(blobsFD); err != nil {
		return fileBlobError(err)
	}
	return fileBlobError(unix.Fsync(stagingFD))
}

func (s *PrivateFileBlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateFileBlobKey(key); err != nil {
		return nil, err
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	dirFD, err := s.openDirectory(rootFD, "blobs")
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	return openPrivateFileBlob(dirFD, key)
}

func (s *PrivateFileBlobStore) Remove(ctx context.Context, key string) error {
	if err := s.CleanupStaging(ctx, key); err != nil {
		return err
	}
	return s.remove(ctx, "blobs", key)
}

func (s *PrivateFileBlobStore) CleanupStaging(ctx context.Context, key string) error {
	return s.remove(ctx, "staging", key)
}

// FreeBytes reports space available to this process before the configured reserve.
func (s *PrivateFileBlobStore) FreeBytes(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return 0, err
	}
	defer unix.Close(rootFD)
	return availableFileBlobBytes(rootFD)
}

func (s *PrivateFileBlobStore) remove(ctx context.Context, directory, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFileBlobKey(key); err != nil {
		return err
	}
	rootFD, err := s.openRoot()
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	dirFD, err := s.openDirectory(rootFD, directory)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	file, err := openPrivateFileBlob(dirFD, key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = file.Close()
	if err := unix.Unlinkat(dirFD, key, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fileBlobError(err)
	}
	return fileBlobError(unix.Fsync(dirFD))
}

func (s *PrivateFileBlobStore) openRoot() (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open(string(filepath.Separator), flags, 0)
	if err != nil {
		return -1, fileBlobError(err)
	}
	for _, part := range strings.Split(strings.TrimPrefix(s.root, string(filepath.Separator)), string(filepath.Separator)) {
		next, err := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, fileBlobError(err)
		}
		fd = next
	}
	if s.ready {
		id, err := privateFileBlobDirectoryID(fd)
		if err != nil || id != s.rootID {
			_ = unix.Close(fd)
			return -1, ErrUnsafeFileBlobPath
		}
	}
	return fd, nil
}

func (s *PrivateFileBlobStore) openDirectory(rootFD int, directory string) (int, error) {
	fd, err := unix.Openat(rootFD, directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fileBlobError(err)
	}
	id, err := privateFileBlobDirectoryID(fd)
	expected := s.stagingID
	if directory == "blobs" {
		expected = s.blobsID
	}
	if err != nil || id.device != s.rootID.device || s.ready && id != expected {
		_ = unix.Close(fd)
		return -1, ErrUnsafeFileBlobPath
	}
	return fd, nil
}

func privateFileBlobDirectoryID(fd int) (fileBlobDirectoryID, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fileBlobDirectoryID{}, fileBlobError(err)
	}
	return fileBlobDirectoryID{uint64(stat.Dev), uint64(stat.Ino)}, nil
}

func openPrivateFileBlob(dirFD int, key string) (*os.File, error) {
	fd, err := unix.Openat(dirFD, key, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileBlobError(err)
	}
	file := os.NewFile(uintptr(fd), key)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrUnsafeFileBlobPath
	}
	return file, nil
}

func ensureFileBlobMissing(dirFD int, key string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(dirFD, key, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fileBlobError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafeFileBlobPath
	}
	return ErrFileBlobExists
}

func validateFileBlobKey(key string) error {
	if len(key) != 32 {
		return ErrInvalidFileBlobKey
	}
	for _, ch := range key {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return ErrInvalidFileBlobKey
		}
	}
	return nil
}

type fileBlobContextReader struct {
	ctx context.Context
	src io.Reader
}

type fileBlobReserveWriter struct {
	store  *PrivateFileBlobStore
	file   *os.File
	rootFD int
}

func (w fileBlobReserveWriter) Write(p []byte) (int, error) {
	w.store.writeMu.Lock()
	defer w.store.writeMu.Unlock()
	free, err := availableFileBlobBytes(w.rootFD)
	if err != nil {
		return 0, err
	}
	if free < w.store.reserveBytes || int64(len(p)) > free-w.store.reserveBytes {
		return 0, ErrFileStorageFull
	}
	n, err := w.file.Write(p)
	return n, fileBlobError(err)
}

func availableFileBlobBytes(fd int) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return 0, fileBlobError(err)
	}
	blocks, blockSize := uint64(stat.Bavail), uint64(stat.Bsize)
	if blockSize == 0 {
		return 0, errors.New("private file storage reported an invalid block size")
	}
	if blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64, nil
	}
	return int64(blocks * blockSize), nil
}

func (r fileBlobContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.src.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

func fileBlobError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) {
		return fmt.Errorf("%w: %w", ErrFileStorageFull, err)
	}
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("%w: %w", ErrUnsafeFileBlobPath, err)
	}
	return err
}
