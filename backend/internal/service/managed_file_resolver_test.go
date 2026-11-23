package service

import (
	"context"
	"encoding/json"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type managedResolverFileRepo struct {
	FileRepository
	mu               sync.Mutex
	files            map[string]*StoredFile
	opened, released []string
}

func (r *managedResolverFileRepo) GetForOwner(_ context.Context, u, g int64, id string) (*StoredFile, error) {
	f := r.files[id]
	if f == nil || f.UserID != u || f.GroupID != g {
		return nil, ErrFileNotFound
	}
	copy := *f
	return &copy, nil
}
func (r *managedResolverFileRepo) AcquireReadGrant(ctx context.Context, u, g int64, id, token string, lease, _ time.Duration) (*StoredFile, FileReadGrant, error) {
	f, e := r.GetForOwner(ctx, u, g, id)
	if e == nil {
		r.mu.Lock()
		r.opened = append(r.opened, id)
		r.mu.Unlock()
	}
	now := time.Now()
	return f, FileReadGrant{DBNow: now, Until: now.Add(lease)}, e
}
func (r *managedResolverFileRepo) RenewReadGrant(_ context.Context, _ int64, _ string, lease, _ time.Duration) (FileReadGrant, error) {
	now := time.Now()
	return FileReadGrant{DBNow: now, Until: now.Add(lease)}, nil
}
func (r *managedResolverFileRepo) ReleaseRead(_ context.Context, id int64, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.released = append(r.released, "released")
	return nil
}
func (r *managedResolverFileRepo) readActivity() (opened, released []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.opened...), append([]string(nil), r.released...)
}

type managedResolverBlob struct{ FileBlobStore }

func (managedResolverBlob) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("hello")), nil
}

type managedResolverLedger struct {
	ManagedFileRepository
	lookup                        ManagedFileLookup
	lookupCalls                   int
	reservation                   *ManagedFileReservation
	started, completed, uncertain []string
	authorized, releases          int
	authorizeErr                  error
	existing                      bool
}

func (l *managedResolverLedger) LookupBindings(context.Context, FileOwner, []string) (ManagedFileLookup, error) {
	l.lookupCalls++
	return l.lookup, nil
}

func managedResolverFiles(t *testing.T, repo *managedResolverFileRepo) *FileService {
	t.Helper()
	c := config.DefaultFilesConfig()
	c.Enabled = true
	c.Root = t.TempDir()
	s, e := NewFileService(repo, managedResolverBlob{}, c)
	require.NoError(t, e)
	return s
}
func managedResolverRecord(id string) *StoredFile {
	return &StoredFile{ID: 1, PublicID: id, StorageKey: id, UserID: 1, GroupID: 2, APIKeyID: 3, State: "ready", Version: 4, Filename: "test.txt", MIME: "text/plain", Bytes: 5, SHA256: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}
}
func TestManagedFileResolverChecksAllOwnersBeforeOpeningAnyFile(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, nil, nil)
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"},{"type":"input_file","file_id":"file-foreign"}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.ErrorIs(t, err, ErrFileNotFound)
	require.Nil(t, plan)
	opened, _ := repo.readActivity()
	require.Empty(t, opened)
	require.Zero(t, ledger.lookupCalls)
}

func (l *managedResolverLedger) Reserve(_ context.Context, in ManagedFileReserveInput) (*ManagedFileReservation, error) {
	l.reservation = &ManagedFileReservation{Owner: in.Owner, Selection: in.Selection, LeaseToken: in.LeaseToken}
	for _, v := range in.Files {
		f := managedResolverRecord(v.PublicID)
		l.reservation.Files = append(l.reservation.Files, ManagedFileBinding{File: f, Selection: in.Selection, Version: 1, Operation: ManagedFileOperation{ID: "operation-" + v.PublicID, State: "reserved"}})
	}
	if l.existing {
		for i := range l.reservation.Files {
			l.reservation.Files[i].Operation.State = "succeeded"
			l.reservation.Files[i].Operation.RemoteID = "file-existing"
		}
	}
	return l.reservation, nil
}
func (l *managedResolverLedger) StartOperation(_ context.Context, _ *ManagedFileReservation, id string) error {
	l.started = append(l.started, id)
	return nil
}
func (l *managedResolverLedger) CompleteOperation(_ context.Context, _ *ManagedFileReservation, id string, _ managedfiles.EnsureResult) (bool, error) {
	l.completed = append(l.completed, id)
	return true, nil
}
func (l *managedResolverLedger) MarkUncertain(_ context.Context, _ *ManagedFileReservation, id, _ string) error {
	l.uncertain = append(l.uncertain, id)
	return nil
}
func (l *managedResolverLedger) AuthorizeGeneration(context.Context, *ManagedFileReservation) error {
	l.authorized++
	return l.authorizeErr
}
func (l *managedResolverLedger) ReleaseReservation(context.Context, *ManagedFileReservation) error {
	l.releases++
	return nil
}

func TestManagedFileResolverBindsUploadedFileAndRechecksBeforeGeneration(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{}
	var calls atomic.Int32
	var manifest managedfiles.AdmitManifest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			json.NewEncoder(w).Encode(bpsFilesTestDescription())
		case managedfiles.Prefix + "admit":
			mr, e := r.MultipartReader()
			require.NoError(t, e)
			part, e := mr.NextPart()
			require.NoError(t, e)
			raw, e := io.ReadAll(part)
			require.NoError(t, e)
			manifest, e = managedfiles.DecodeManifest(raw)
			require.NoError(t, e)
			part, e = mr.NextPart()
			require.NoError(t, e)
			content, e := io.ReadAll(part)
			require.NoError(t, e)
			require.Equal(t, "hello", string(content))
			require.Equal(t, "1", manifest.OwnerRef)
			require.Equal(t, "2", manifest.GroupRef)
			hash, e := managedfiles.TemplateHash(manifest.RequestTemplate)
			require.NoError(t, e)
			json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-a", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: "file-a", State: "staged"}}})
		case managedfiles.Prefix + "ensure-uploaded":
			require.Equal(t, []string{"file-a"}, ledger.started, "durable start precedes external upload")
			var req managedfiles.EnsureRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			f := manifest.Files[0]
			result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: req.OperationID, FileRef: f.FileRef, RemoteID: "file-remote", Bytes: f.Bytes, SHA256: f.SHA256, Filename: f.Filename, MIME: f.MIME, InputTokens: 17, Ready: true}
			bound, e := managedfiles.Bind(manifest, []managedfiles.EnsureResult{result})
			require.NoError(t, e)
			result.WireSHA256 = bound.WireSHA256
			result.BindingSHA256 = bound.BindingSHA256
			json.NewEncoder(w).Encode(result)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}],"metadata":{"large":9007199254740993}}`)
	plan, e := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, e)
	require.Zero(t, calls.Load())
	defer plan.Close()
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	result, e := resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.NoError(t, e)
	require.EqualValues(t, 3, calls.Load())
	require.Contains(t, string(result.Body), `"file_id":"file-remote"`)
	require.Contains(t, string(result.Body), `9007199254740993`)
	require.Equal(t, []string{"file-a"}, ledger.completed)
	require.NoError(t, result.Authorize(managedResolverAuthorizedContext()))
	require.Positive(t, ledger.authorized)
	ledger.authorizeErr = ErrFileNotFound
	require.ErrorIs(t, result.Authorize(managedResolverAuthorizedContext()), ErrFileNotFound)
	ledger.authorizeErr = nil
	require.NoError(t, plan.Close())
	require.Equal(t, 1, ledger.releases)
	_, released := repo.readActivity()
	require.Len(t, released, 1)
}

func TestManagedFileResolverUploadFailureStopsAndBecomesUncertain(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			json.NewEncoder(w).Encode(bpsFilesTestDescription())
		case managedfiles.Prefix + "admit":
			mr, e := r.MultipartReader()
			require.NoError(t, e)
			part, e := mr.NextPart()
			require.NoError(t, e)
			raw, e := io.ReadAll(part)
			require.NoError(t, e)
			manifest, e := managedfiles.DecodeManifest(raw)
			require.NoError(t, e)
			_, e = io.Copy(io.Discard, r.Body)
			require.NoError(t, e)
			hash, e := managedfiles.TemplateHash(manifest.RequestTemplate)
			require.NoError(t, e)
			json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-failed", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: "file-a", State: "staged"}}})
		case managedfiles.Prefix + "ensure-uploaded":
			w.WriteHeader(502)
		default:
			t.Errorf("unexpected generation or retry %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`)
	plan, e := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, e)
	defer plan.Close()
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	result, e := resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.Error(t, e)
	require.Nil(t, result)
	require.EqualValues(t, 3, calls.Load())
	require.Equal(t, []string{"file-a"}, ledger.uncertain)
	require.Empty(t, ledger.completed)
	require.Zero(t, ledger.authorized)
	_, e = resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.Error(t, e)
	require.EqualValues(t, 3, calls.Load(), "same plan cannot upload again")
}

func TestManagedFileResolverReusesConfirmedCopyWithoutEnsure(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{lookup: ManagedFileLookup{AccountID: 7}, existing: true}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			json.NewEncoder(w).Encode(bpsFilesTestDescription())
		case managedfiles.Prefix + "admit":
			mr, e := r.MultipartReader()
			require.NoError(t, e)
			part, e := mr.NextPart()
			require.NoError(t, e)
			raw, e := io.ReadAll(part)
			require.NoError(t, e)
			manifest, e := managedfiles.DecodeManifest(raw)
			require.NoError(t, e)
			require.Equal(t, "existing", manifest.Files[0].Mode)
			require.Equal(t, "file-existing", manifest.Files[0].RemoteID)
			_, e = mr.NextPart()
			require.ErrorIs(t, e, io.EOF)
			bound, e := managedfiles.Bind(manifest, nil)
			require.NoError(t, e)
			json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-existing", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: bound.TemplateSHA256, Ready: true, WireSHA256: bound.WireSHA256, BindingSHA256: bound.BindingSHA256, Files: []managedfiles.AdmittedFile{{FileRef: "file-a", State: "known"}}})
		default:
			t.Errorf("unexpected upload %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`)
	plan, e := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, e)
	defer plan.Close()
	require.EqualValues(t, 7, plan.RequiredAccountID)
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	result, e := resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.NoError(t, e)
	require.EqualValues(t, 2, calls.Load())
	require.Contains(t, string(result.Body), `"file_id":"file-existing"`)
	require.Empty(t, ledger.started)
	require.NoError(t, plan.Close())
	require.ErrorIs(t, result.Authorize(managedResolverAuthorizedContext()), ErrManagedFileNotReady)
}

func TestManagedFileResolverPreservesFirstCopyWhenSecondUploadFails(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a"), "file-b": managedResolverRecord("file-b")}}
	ledger := &managedResolverLedger{}
	var manifest managedfiles.AdmitManifest
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			json.NewEncoder(w).Encode(bpsFilesTestDescription())
		case managedfiles.Prefix + "admit":
			mr, e := r.MultipartReader()
			require.NoError(t, e)
			p, e := mr.NextPart()
			require.NoError(t, e)
			raw, e := io.ReadAll(p)
			require.NoError(t, e)
			manifest, e = managedfiles.DecodeManifest(raw)
			require.NoError(t, e)
			for range 2 {
				p, e = mr.NextPart()
				require.NoError(t, e)
				_, e = io.Copy(io.Discard, p)
				require.NoError(t, e)
			}
			hash, e := managedfiles.TemplateHash(manifest.RequestTemplate)
			require.NoError(t, e)
			json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-two", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: []managedfiles.AdmittedFile{{FileRef: "file-a", State: "staged"}, {FileRef: "file-b", State: "staged"}}})
		case managedfiles.Prefix + "ensure-uploaded":
			var req managedfiles.EnsureRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			n := uploads.Add(1)
			if n == 2 {
				require.Equal(t, []string{"file-a"}, ledger.completed)
				w.WriteHeader(502)
				return
			}
			f := manifest.Files[0]
			json.NewEncoder(w).Encode(managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: req.OperationID, FileRef: f.FileRef, RemoteID: "file-first", Bytes: f.Bytes, SHA256: f.SHA256, Filename: f.Filename, MIME: f.MIME, InputTokens: 17})
		default:
			t.Errorf("unexpected generation %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"},{"type":"input_file","file_id":"file-b"}]}]}`)
	plan, e := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, e)
	defer plan.Close()
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	result, e := resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.Error(t, e)
	require.Nil(t, result)
	require.Equal(t, []string{"file-a"}, ledger.completed)
	require.Equal(t, []string{"file-b"}, ledger.uncertain)
	require.EqualValues(t, 2, uploads.Load())
	require.Zero(t, ledger.authorized)
}

func TestManagedFileResolverRejectsRepeatedPositionBudgetBeforeOpening(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, nil, nil)
	parts := make([]string, 21)
	for i := range parts {
		parts[i] = `{"type":"input_file","file_id":"file-a"}`
	}
	body := []byte(`{"input":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.Error(t, err)
	if plan != nil {
		defer plan.Close()
	}
	require.Nil(t, plan)
	opened, _ := repo.readActivity()
	require.Empty(t, opened)
	require.Zero(t, ledger.lookupCalls)
}

func managedResolverAuthorizedContext() context.Context {
	return withManagedFilePreparationAuthorization(context.Background(), func(context.Context) error { return nil })
}
