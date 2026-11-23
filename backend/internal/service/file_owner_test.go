package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
)

type fileOwnerRepository struct {
	FileRepository
	record, granted                                 *StoredFile
	getCalls, grantCalls, releases, commits, aborts int
}

func (r *fileOwnerRepository) GetForOwner(context.Context, int64, int64, string) (*StoredFile, error) {
	r.getCalls++
	return r.record, nil
}
func (r *fileOwnerRepository) AcquireReadGrant(context.Context, int64, int64, string, string, time.Duration, time.Duration) (*StoredFile, FileReadGrant, error) {
	r.grantCalls++
	now := time.Now()
	return r.granted, FileReadGrant{DBNow: now, Until: now.Add(time.Minute)}, nil
}
func (r *fileOwnerRepository) ReleaseRead(context.Context, int64, string) error {
	r.releases++
	return nil
}
func (r *fileOwnerRepository) ClaimStaleUploads(context.Context, string, time.Duration, int) ([]*StoredFile, error) {
	return nil, nil
}
func (r *fileOwnerRepository) ReserveUpload(_ context.Context, f *StoredFile, _ int64, _ int64, _ time.Duration, _ time.Duration, _ int, _ int) error {
	f.ID = 1
	r.record = f
	return nil
}
func (r *fileOwnerRepository) RenewUpload(context.Context, int64, string, time.Duration) error {
	return nil
}
func (r *fileOwnerRepository) AbortUpload(context.Context, int64, string) (bool, error) {
	r.aborts++
	return true, nil
}
func (r *fileOwnerRepository) FinishCleanup(context.Context, int64, string) error { return nil }
func (r *fileOwnerRepository) CommitUploadMetadata(context.Context, int64, string, int64, string, string, string, string, *int64) (*StoredFile, error) {
	r.commits++
	return r.record, nil
}

type fileOwnerStore struct {
	FileBlobStore
	opens int
}

func (s *fileOwnerStore) Open(context.Context, string) (io.ReadCloser, error) {
	s.opens++
	return io.NopCloser(strings.NewReader("private")), nil
}
func newFileOwnerService(t *testing.T, r FileRepository, store FileBlobStore) *FileService {
	t.Helper()
	cfg := config.DefaultFilesConfig()
	cfg.Enabled = true
	cfg.Root = t.TempDir()
	s, err := NewFileService(r, store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFileOwnerGetAndOpenRequireFullScope(t *testing.T) {
	owner := FileOwner{UserID: 7, APIKeyID: 11, GroupID: 42}
	for _, tc := range []struct {
		name string
		got  FileOwner
	}{
		{"other key", FileOwner{7, 12, 42}}, {"other user", FileOwner{8, 11, 42}}, {"other group", FileOwner{7, 11, 43}}, {"missing key", FileOwner{7, 0, 42}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fileOwnerRepository{record: &StoredFile{ID: 1, PublicID: "file-private", UserID: owner.UserID, APIKeyID: owner.APIKeyID, GroupID: owner.GroupID}}
			store := &fileOwnerStore{}
			s := newFileOwnerService(t, r, store)
			if _, err := s.Get(context.Background(), tc.got, "file-private"); !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("Get = %v", err)
			}
			if _, _, err := s.Open(context.Background(), tc.got, "file-private"); !errors.Is(err, ErrFileNotFound) {
				t.Fatalf("Open = %v", err)
			}
			if r.grantCalls != 0 || store.opens != 0 {
				t.Fatal("unauthorized owner acquired a lease or opened private content")
			}
			if tc.got.APIKeyID == 0 && r.getCalls != 0 {
				t.Fatal("invalid owner reached repository")
			}
		})
	}
}

func TestFileOwnerListRejectsMissingScopeBeforeRepository(t *testing.T) {
	s := newFileOwnerService(t, &fileOwnerRepository{}, &fileOwnerStore{})
	for _, owner := range []FileOwner{{0, 11, 42}, {7, 0, 42}, {7, 11, 0}} {
		_, err := s.List(context.Background(), owner, FileListQuery{Limit: 1})
		if !errors.Is(err, ErrInvalidFile) {
			t.Fatalf("invalid list owner: %v", err)
		}
	}
}

func TestFileOwnerOpenRechecksLeaseRecordAndReleases(t *testing.T) {
	for _, crossKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "same owner", true: "changed lease owner"}[crossKey], func(t *testing.T) {
			owner := FileOwner{7, 11, 42}
			f := &StoredFile{ID: 1, PublicID: "file-private", UserID: 7, APIKeyID: 11, GroupID: 42}
			g := *f
			if crossKey {
				g.APIKeyID++
			}
			r := &fileOwnerRepository{record: f, granted: &g}
			store := &fileOwnerStore{}
			s := newFileOwnerService(t, r, store)
			_, reader, err := s.Open(context.Background(), owner, f.PublicID)
			if crossKey {
				if !errors.Is(err, ErrFileNotFound) || r.releases != 1 || store.opens != 0 {
					t.Fatalf("changed lease scope: err=%v releases=%d opens=%d", err, r.releases, store.opens)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil || string(data) != "private" {
				t.Fatalf("authorized read: %q %v", data, err)
			}
			if err = reader.Close(); err != nil {
				t.Fatal(err)
			}
			if r.releases != 1 || store.opens != 1 {
				t.Fatalf("lease not closed: %+v", r)
			}
		})
	}
}

func TestFileUploadFreshAuthorizationRejectsBeforePublication(t *testing.T) {
	root := t.TempDir()
	store, err := NewPrivateFileBlobStore(filepath.Join(root, "files"), 0)
	if err != nil {
		t.Fatal(err)
	}
	r := &fileOwnerRepository{}
	s := newFileOwnerService(t, r, store)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err = mw.WriteField("purpose", "user_data"); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "private.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, "secret"); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}
	checks := 0
	_, err = s.UploadMultipartWithHooks(context.Background(), FileOwner{7, 11, 42}, multipart.NewReader(&body, mw.Boundary()), FileUploadHooks{Reauthorize: func(context.Context) error { checks++; return ErrFileAuthorizationRevoked }})
	if !errors.Is(err, ErrFileAuthorizationRevoked) || checks != 1 || r.commits != 0 || r.aborts != 1 {
		t.Fatalf("revoked upload: err=%v checks=%d commits=%d aborts=%d", err, checks, r.commits, r.aborts)
	}
	for _, directory := range []string{"staging", "blobs"} {
		entries, e := os.ReadDir(filepath.Join(root, "files", directory))
		if e != nil || len(entries) != 0 {
			t.Fatalf("revoked upload retained %s content: %v %v", directory, entries, e)
		}
	}
}
