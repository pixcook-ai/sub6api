package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

type ManagedInlineStagedFile struct {
	ManagedInlineFileDescriptor
	SHA256 string
}

// ManagedInlineStaging owns only request-local blobs, never Files API resources.
// Files and Open use the preparation's full stable index, including public IDs.
type ManagedInlineStaging struct {
	mu        sync.Mutex
	dir       string
	root      *os.Root
	files     []ManagedInlineStagedFile
	names     []string
	readers   map[*managedInlineReader]struct{}
	closed    bool
	stop      func() bool
	removeAll func(string) error
	children  []*ManagedInlineStaging
	delegates map[int]managedInlineStageDelegate
}

type managedInlineStageDelegate struct {
	stage *ManagedInlineStaging
	index int
}

// combineManagedInlineStages takes ownership of its children, including on
// failure. Each source remains addressed by the preparation's full position.
func combineManagedInlineStages(ctx context.Context, plan *ManagedInlinePreparation, children ...*ManagedInlineStaging) (_ *ManagedInlineStaging, resultErr error) {
	staged := &ManagedInlineStaging{children: children, delegates: make(map[int]managedInlineStageDelegate)}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, staged.Close())
		}
	}()
	if !plan.ReadyForStaging() {
		return nil, inlineReject("incomplete_inline_budget", "input")
	}
	staged.files = make([]ManagedInlineStagedFile, len(plan.entries))
	positions := make(map[managedfiles.Position]int, len(plan.entries))
	entries := append([]managedInlineEntry(nil), plan.entries...)
	for i, entry := range entries {
		position := entry.description.Position
		if _, exists := positions[position]; exists {
			return nil, inlineReject("invalid_inline_plan", "input")
		}
		positions[position] = i
		staged.files[i].ManagedInlineFileDescriptor = entry.description
	}
	for _, child := range children {
		for index, file := range child.Files() {
			if file.PublicID != "" || file.SHA256 == "" {
				continue
			}
			fullIndex, exists := positions[file.Position]
			if !exists {
				return nil, inlineReject("invalid_inline_plan", "input")
			}
			entry := entries[fullIndex]
			_, duplicate := staged.delegates[fullIndex]
			if duplicate || entry.description.PublicID != "" || file.Bytes <= 0 || file.Filename != entry.description.Filename || (entry.sourceURL == "" && file.ManagedInlineFileDescriptor != entry.description) {
				return nil, inlineReject("invalid_inline_plan", "input")
			}
			staged.files[fullIndex] = file
			staged.delegates[fullIndex] = managedInlineStageDelegate{stage: child, index: index}
			entries[fullIndex].description = file.ManagedInlineFileDescriptor
		}
	}
	for i, entry := range entries {
		if entry.description.PublicID == "" {
			if _, exists := staged.delegates[i]; !exists {
				return nil, inlineReject("invalid_inline_plan", "input")
			}
		}
	}
	if err := validateManagedInlineBudget(entries, plan.limits); err != nil {
		return nil, err
	}
	staged.mu.Lock()
	staged.stop = context.AfterFunc(ctx, func() { _ = staged.Close() })
	staged.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return staged, nil
}

func StageManagedInlineFiles(ctx context.Context, plan *ManagedInlinePreparation, privateParent string) (_ *ManagedInlineStaging, resultErr error) {
	return stageManagedInlineFiles(ctx, plan, privateParent, defaultManagedInlineStagingFilesystem())
}

// Filesystem substitutions stay at the private OS boundary; callers cannot
// bypass preflight or supply their own decoded payload through the public API.
type managedInlineStagingFilesystem struct {
	mkdirTemp func(string, string) (string, error)
	openRoot  func(string) (*os.Root, error)
	create    func(*os.Root, string) (io.WriteCloser, error)
	removeAll func(string) error
}

func defaultManagedInlineStagingFilesystem() managedInlineStagingFilesystem {
	return managedInlineStagingFilesystem{mkdirTemp: os.MkdirTemp, openRoot: os.OpenRoot, create: func(root *os.Root, name string) (io.WriteCloser, error) {
		return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}, removeAll: os.RemoveAll}
}

func stageManagedInlineFiles(ctx context.Context, plan *ManagedInlinePreparation, privateParent string, fs managedInlineStagingFilesystem) (_ *ManagedInlineStaging, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !plan.ReadyForStaging() {
		return nil, inlineReject("incomplete_inline_budget", "input")
	}
	if err := validateManagedInlineBudget(plan.entries, plan.limits); err != nil {
		return nil, err
	}
	staged := &ManagedInlineStaging{files: make([]ManagedInlineStagedFile, len(plan.entries)), names: make([]string, len(plan.entries)), readers: make(map[*managedInlineReader]struct{}), removeAll: fs.removeAll}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, staged.Close())
		}
	}()
	for i, entry := range plan.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		staged.files[i].ManagedInlineFileDescriptor = entry.description
		if entry.description.PublicID != "" || entry.sourceURL != "" {
			continue
		}
		if staged.root == nil {
			dir, err := fs.mkdirTemp(privateParent, "bps-inline-*")
			if err != nil {
				return nil, inlineStorageError(err)
			}
			staged.dir = dir
			staged.root, err = fs.openRoot(dir)
			if err != nil {
				return nil, inlineStorageError(err)
			}
		}
		name := "blob-" + strconv.Itoa(i)
		file, err := fs.create(staged.root, name)
		if err != nil {
			return nil, inlineStorageError(err)
		}
		digest := sha256.New()
		reader := base64.NewDecoder(base64.StdEncoding.Strict(), managedInlineContextReader{ctx: ctx, reader: strings.NewReader(entry.encoded)})
		size, copyErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(reader, entry.description.Bytes+1))
		closeErr := file.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if copyErr != nil || closeErr != nil {
			return nil, inlineStorageError(errors.Join(copyErr, closeErr))
		}
		if size != entry.description.Bytes {
			return nil, inlineReject("invalid_inline_size", "input")
		}
		staged.files[i].SHA256 = hex.EncodeToString(digest.Sum(nil))
		staged.names[i] = name
	}
	staged.mu.Lock()
	staged.stop = context.AfterFunc(ctx, func() { _ = staged.Close() })
	staged.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return staged, nil
}

func (s *ManagedInlineStaging) Files() []ManagedInlineStagedFile {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ManagedInlineStagedFile(nil), s.files...)
}

func (s *ManagedInlineStaging) Open(index int) (io.ReadCloser, error) {
	if s == nil {
		return nil, inlineReject("closed_inline_staging", "input")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || index < 0 || index >= len(s.files) {
		return nil, inlineReject("unavailable_inline_file", "input")
	}
	if delegate, ok := s.delegates[index]; ok {
		return delegate.stage.Open(delegate.index)
	}
	if index >= len(s.names) || s.names[index] == "" {
		return nil, inlineReject("unavailable_inline_file", "input")
	}
	file, err := s.root.Open(s.names[index])
	if err != nil {
		return nil, inlineStorageError(err)
	}
	reader := &managedInlineReader{file: file, owner: s}
	s.readers[reader] = struct{}{}
	return reader, nil
}

// Close blocks new opens, closes live readers, and retries a failed removal on
// the next call. Storage errors retain errors.Is while exposing no path text.
func (s *ManagedInlineStaging) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.stop != nil {
		s.stop()
		s.stop = nil
	}
	var result error
	for _, child := range s.children {
		result = errors.Join(result, child.Close())
	}
	for reader := range s.readers {
		result = errors.Join(result, reader.file.Close())
		reader.closed = true
		delete(s.readers, reader)
	}
	if s.root != nil {
		result = errors.Join(result, s.root.Close())
		s.root = nil
	}
	if s.dir != "" {
		if err := s.removeAll(s.dir); err != nil {
			result = errors.Join(result, err)
		} else {
			s.dir = ""
		}
	}
	if result != nil {
		return inlineStorageError(result)
	}
	return nil
}

type managedInlineReader struct {
	file   *os.File
	owner  *ManagedInlineStaging
	closed bool
}

func (r *managedInlineReader) Read(p []byte) (int, error) { return r.file.Read(p) }

func (r *managedInlineReader) Close() error {
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	delete(r.owner.readers, r)
	if err := r.file.Close(); err != nil {
		return inlineStorageError(err)
	}
	return nil
}

type managedInlineStorageError struct{ cause error }

func (e *managedInlineStorageError) Error() string { return "inline file staging failed" }
func (e *managedInlineStorageError) Unwrap() error { return e.cause }
func inlineStorageError(err error) error           { return &managedInlineStorageError{cause: err} }
