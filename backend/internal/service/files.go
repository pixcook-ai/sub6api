package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/config"
)

var (
	ErrFileNotFound                 = errors.New("file not found")
	ErrFileQuotaExceeded            = errors.New("file storage quota exceeded")
	ErrFileLeaseConflict            = errors.New("file storage lease lost")
	ErrFileAuthorizationRevoked     = errors.New("file upload authorization revoked")
	ErrFileAuthorizationUnavailable = errors.New("file upload authorization unavailable")
	ErrInvalidFile                  = errors.New("invalid file upload")
	ErrFilesDisabled                = errors.New("files storage is disabled")
)

// StoredFile is a local durable resource. It carries no BPS upload or processing status.
type StoredFile struct {
	ID                                                              int64
	PublicID                                                        string
	UserID, APIKeyID, GroupID                                       int64
	StorageKey, Filename, Purpose, MIME, State, UploadToken, SHA256 string
	Bytes, ReservedBytes                                            int64
	CreatedAt                                                       time.Time
	ExpiresAt, RetiredAt, ReadDrainUntil, LocalPurgedAt             *time.Time
	Version                                                         int64
	RemoteActivityPossible                                          bool
}

type FileOwner struct{ UserID, APIKeyID, GroupID int64 }

// FileRepository coordinates budgets and leases across all processes sharing the volume.
type FileRepository interface {
	ListVisibleFiles(context.Context, FileOwner, FileListQuery) (FileListPage, error)
	Reserve(context.Context, *StoredFile, int64, int64, time.Duration) error
	ReserveUpload(context.Context, *StoredFile, int64, int64, time.Duration, time.Duration, int, int) error
	CommitUpload(context.Context, int64, string, int64, string, string) error
	CommitUploadWithRetention(context.Context, int64, string, int64, string, string, *int64) (*StoredFile, error)
	CommitUploadMetadata(context.Context, int64, string, int64, string, string, string, string, *int64) (*StoredFile, error)
	AbortUpload(context.Context, int64, string) (bool, error)
	GetForOwner(context.Context, int64, int64, string) (*StoredFile, error)
	AcquireRead(context.Context, int64, int64, string, string, time.Duration) (*StoredFile, error)
	AcquireReadGrant(context.Context, int64, int64, string, string, time.Duration, time.Duration) (*StoredFile, FileReadGrant, error)
	RenewUpload(context.Context, int64, string, time.Duration) error
	RenewRead(context.Context, int64, string, time.Duration) error
	RenewReadGrant(context.Context, int64, string, time.Duration, time.Duration) (FileReadGrant, error)
	ReleaseRead(context.Context, int64, string) error
	ClaimStaleUploads(context.Context, string, time.Duration, int) ([]*StoredFile, error)
	FinishCleanup(context.Context, int64, string) error
	LifecycleCandidates(context.Context, int64, int) ([]int64, error)
	RetireExpired(context.Context, int64, time.Duration) (bool, error)
	ClaimLocalPurge(context.Context, int64, string, time.Duration) (*FilePurgeTicket, error)
	ConfirmLocalPurge(context.Context, FilePurgeTicket) error
}

type FileBlobStore interface {
	Stage(context.Context, string, io.Reader, int64) (int64, string, error)
	Commit(context.Context, string) error
	Open(context.Context, string) (io.ReadCloser, error)
	Remove(context.Context, string) error
	CleanupStaging(context.Context, string) error
	FreeBytes(context.Context) (int64, error)
}

type FileService struct {
	repo            FileRepository
	store           FileBlobStore
	cfg             config.FilesConfig
	lifecycleMu     sync.Mutex
	lifecycleRunMu  sync.Mutex
	lifecycleCursor int64
	lifecycleCancel context.CancelFunc
	lifecycleDone   chan struct{}
}

func NewFileService(repo FileRepository, store FileBlobStore, cfg config.FilesConfig) (*FileService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Enabled && (repo == nil || store == nil) {
		return nil, errors.New("enabled files require repository and private storage")
	}
	return &FileService{repo: repo, store: store, cfg: cfg}, nil
}

// ProvideFileService leaves the feature inert unless an explicit volume is configured.
func ProvideFileService(repo FileRepository, cfg *config.Config) (*FileService, error) {
	if !cfg.Gateway.Files.Enabled {
		return NewFileService(repo, nil, cfg.Gateway.Files)
	}
	store, err := NewPrivateFileBlobStore(cfg.Gateway.Files.Root, cfg.Gateway.Files.FreeSpaceReserveBytes)
	if err != nil {
		return nil, err
	}
	svc, err := NewFileService(repo, store, cfg.Gateway.Files)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = svc.Recover(ctx); err != nil {
		return nil, fmt.Errorf("recover private file staging: %w", err)
	}
	svc.StartLifecycle()
	return svc, nil
}

func (s *FileService) Enabled() bool             { return s != nil && s.cfg.Enabled }
func (s *FileService) MaxUploadBodyBytes() int64 { return s.cfg.MaxFileBytes + 64*1024 }
func (s *FileService) UploadTimeout() time.Duration {
	return time.Duration(s.cfg.UploadTimeoutSeconds) * time.Second
}
func (s *FileService) lease() time.Duration { return time.Duration(s.cfg.LeaseSeconds) * time.Second }

func fileToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// UploadMultipart streams exactly one file; fields may appear before or after it.
// The full max-file reservation is made before disk IO, including unknown-length uploads.
func (s *FileService) UploadMultipart(ctx context.Context, owner FileOwner, reader *multipart.Reader) (_ *StoredFile, retErr error) {
	return s.UploadMultipartWithHooks(ctx, owner, reader, FileUploadHooks{})
}

type FileUploadHooks struct {
	InterruptRead func()
	Reauthorize   func(context.Context) error
}

func (s *FileService) UploadMultipartWithHooks(ctx context.Context, owner FileOwner, reader *multipart.Reader, hooks FileUploadHooks) (_ *StoredFile, retErr error) {
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 {
		return nil, ErrInvalidFile
	}
	ctx, cancel := context.WithTimeout(ctx, s.UploadTimeout())
	defer cancel()
	// Revisit uploads whose old process lease was still live during startup.
	// This is staging recovery only; it does not expire completed Files.
	if err := s.Recover(ctx); err != nil {
		return nil, err
	}
	key, err := fileToken()
	if err != nil {
		return nil, err
	}
	token, err := fileToken()
	if err != nil {
		return nil, err
	}
	record := &StoredFile{PublicID: "file-" + key, StorageKey: key, UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID, UploadToken: token, ReservedBytes: s.cfg.MaxFileBytes, State: "staging"}
	var fileSeen bool
	var purpose string
	var purposeSeen bool
	var expiry FileExpiryInput
	var size int64
	var hash string
	var stop func() error
	defer func() {
		if stop != nil {
			if err := stop(); err != nil {
				retErr = err
			}
		}
		if retErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			retErr = ErrFileUploadDeadline
		}
		if retErr != nil && record != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ok, err := s.repo.AbortUpload(cleanupCtx, record.ID, record.UploadToken)
			if err == nil && ok {
				if s.store.Remove(cleanupCtx, record.StorageKey) == nil {
					_ = s.repo.FinishCleanup(cleanupCtx, record.ID, record.UploadToken)
				}
			}
		}
	}()
	if err = s.repo.ReserveUpload(ctx, record, s.cfg.UserQuotaBytes, s.cfg.GlobalQuotaBytes, s.lease(), s.UploadTimeout(), s.cfg.UserConcurrentUploads, s.cfg.GlobalConcurrentUploads); err != nil {
		if record.ID == 0 {
			record = nil
		}
		return nil, err
	}
	uploadCtx, leaseStop := s.keepLease(ctx, func(c context.Context) error { return s.repo.RenewUpload(c, record.ID, record.UploadToken, s.lease()) })
	stop = leaseStop
	stopInterrupt := func() bool { return true }
	if hooks.InterruptRead != nil {
		stopInterrupt = context.AfterFunc(uploadCtx, hooks.InterruptRead)
	}
	defer func() { stopInterrupt() }()
	for {
		if err = uploadCtx.Err(); err != nil {
			return nil, err
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: malformed multipart: %w", ErrInvalidFile, err)
		}
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return nil, ErrInvalidFile
		}
		switch params["name"] {
		case "expires_after[anchor]", "expires_after[seconds]":
			if params["filename"] != "" {
				return nil, ErrInvalidFile
			}
			value, err := io.ReadAll(io.LimitReader(part, 128))
			if err != nil {
				return nil, err
			}
			if err := expiry.Set(params["name"], string(value)); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidFile, err)
			}
		case "purpose":
			if purposeSeen || params["filename"] != "" {
				return nil, ErrInvalidFile
			}
			purposeSeen = true
			value, err := io.ReadAll(io.LimitReader(part, 1025))
			if err != nil {
				return nil, err
			}
			if len(value) > 1024 {
				return nil, ErrInvalidFile
			}
			purpose = string(value)
		case "file":
			if fileSeen {
				return nil, fmt.Errorf("%w: exactly one file is required", ErrInvalidFile)
			}
			fileSeen = true
			filename := params["filename"]
			contentType := part.Header.Get("Content-Type")
			if err := validateFileMetadata(filename, contentType); err != nil {
				return nil, err
			}
			record.Filename, record.MIME = filename, contentType
			size, hash, err = s.store.Stage(uploadCtx, key, part, s.cfg.MaxFileBytes)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%w: unsupported multipart field", ErrInvalidFile)
		}
		if err := part.Close(); err != nil {
			return nil, err
		}
	}
	if !fileSeen || size == 0 || !validFilePurpose(purpose) {
		return nil, fmt.Errorf("%w: a nonempty file and supported purpose are required", ErrInvalidFile)
	}
	retention, err := expiry.RetentionSeconds(purpose, s.cfg.DefaultRetentionSeconds)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFile, err)
	}
	stopInterrupt()
	if err := stop(); err != nil {
		stop = nil
		return nil, err
	}
	stop = nil
	if err := s.repo.RenewUpload(ctx, record.ID, record.UploadToken, s.lease()); err != nil {
		return nil, err
	}
	if err := s.store.Commit(ctx, record.StorageKey); err != nil {
		return nil, err
	}
	if hooks.Reauthorize != nil {
		if err = hooks.Reauthorize(ctx); err != nil {
			return nil, err
		}
	}
	committed, err := s.repo.CommitUploadMetadata(ctx, record.ID, record.UploadToken, size, hash, purpose, record.Filename, record.MIME, retention)
	if err != nil {
		return nil, err
	}
	return committed, nil
}

func validateFileMetadata(filename, contentType string) error {
	if filename == "" || len(filename) > 255 || !utf8.ValidString(filename) || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("%w: unsafe filename", ErrInvalidFile)
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: unsafe filename", ErrInvalidFile)
		}
	}
	if filepath.Base(filename) != filename {
		return ErrInvalidFile
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return fmt.Errorf("%w: invalid content type", ErrInvalidFile)
	}
	switch mt {
	case "application/octet-stream", "application/pdf", "application/json", "application/zip", "application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "text/plain", "text/csv", "text/markdown", "image/png", "image/jpeg", "image/webp", "image/gif":
		return nil
	default:
		return fmt.Errorf("%w: unsupported content type", ErrInvalidFile)
	}
}

func validFilePurpose(p string) bool {
	switch p {
	case "user_data", "assistants", "batch", "fine-tune", "vision", "evals":
		return true
	}
	return false
}

func (s *FileService) Get(ctx context.Context, owner FileOwner, id string) (*StoredFile, error) {
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.APIKeyID <= 0 || owner.GroupID <= 0 {
		return nil, ErrFileNotFound
	}
	file, err := s.repo.GetForOwner(ctx, owner.UserID, owner.GroupID, id)
	if err != nil {
		return nil, err
	}
	if !fileOwnedBy(file, owner) {
		return nil, ErrFileNotFound
	}
	return file, nil
}

func (s *FileService) Open(ctx context.Context, owner FileOwner, id string) (*StoredFile, io.ReadCloser, error) {
	if !s.Enabled() {
		return nil, nil, ErrFilesDisabled
	}
	if _, err := s.Get(ctx, owner, id); err != nil {
		return nil, nil, err
	}
	token, err := fileToken()
	if err != nil {
		return nil, nil, err
	}
	started := time.Now()
	drain := time.Duration(s.cfg.ReadDrainSeconds) * time.Second
	record, grant, err := s.repo.AcquireReadGrant(ctx, owner.UserID, owner.GroupID, id, token, s.lease(), drain)
	if err != nil {
		return nil, nil, err
	}
	if record == nil {
		return nil, nil, ErrFileNotFound
	}
	release := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.repo.ReleaseRead(cleanupCtx, record.ID, token)
	}
	if !fileOwnedBy(record, owner) {
		release()
		return nil, nil, ErrFileNotFound
	}
	reader, err := s.store.Open(ctx, record.StorageKey)
	if err != nil {
		release()
		return nil, nil, err
	}
	return record, newGrantedFileReader(ctx, reader, started, grant, s.lease(), func(c context.Context) (FileReadGrant, error) {
		return s.repo.RenewReadGrant(c, record.ID, token, s.lease(), drain)
	}, release), nil
}

// Recover only reclaims expired upload/cleanup leases, never active uploads or reads.
// A crash after rename but before DB commit is treated as an incomplete local upload.
func (s *FileService) Recover(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	token, err := fileToken()
	if err != nil {
		return err
	}
	for {
		records, err := s.repo.ClaimStaleUploads(ctx, token, s.lease(), 100)
		if err != nil {
			return err
		}
		for _, record := range records {
			if err = s.store.Remove(ctx, record.StorageKey); err != nil {
				return err
			}
			if err = s.repo.FinishCleanup(ctx, record.ID, token); err != nil {
				return err
			}
		}
		if len(records) < 100 {
			return nil
		}
	}
}

func (s *FileService) keepLease(parent context.Context, renew func(context.Context) error) (context.Context, func() error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	stop := make(chan struct{})
	var once sync.Once
	var leaseErr error
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.lease() / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, s.lease()/6)
				err := renew(renewCtx)
				renewCancel()
				if err != nil {
					leaseErr = err
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() error { once.Do(func() { close(stop); <-done; cancel() }); return leaseErr }
}

func fileOwnedBy(file *StoredFile, owner FileOwner) bool {
	return file != nil && owner.UserID > 0 && owner.APIKeyID > 0 && owner.GroupID > 0 && file.UserID == owner.UserID && file.APIKeyID == owner.APIKeyID && file.GroupID == owner.GroupID
}
