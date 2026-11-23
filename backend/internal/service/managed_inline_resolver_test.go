//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

func TestManagedInlineResolverPreparesInlineWithoutPermanentFiles(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	ledger := &managedResolverLedger{}
	files := managedResolverFiles(t, repo)
	resolver := NewManagedFileResolver(files, ledger, nil, nil)
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	require.NotNil(t, plan, "inline files must enter the managed generation fence")
	require.NoError(t, plan.Close())
	require.Empty(t, repo.files)
	opened, released := repo.readActivity()
	require.Empty(t, opened)
	require.Empty(t, released)
	require.Zero(t, ledger.lookupCalls, "inline request must not invent public ledger bindings")
	entries, err := os.ReadDir(files.cfg.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestManagedInlineResolverRejectsFinalPayloadMutationBeforeRemoteAccess(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	files := managedResolverFiles(t, repo)
	ledger := &managedResolverLedger{}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	account := managedInlineTestAccount(server.URL)
	changed := []byte(strings.Replace(string(body), "aGVsbG8=", "d29ybGQ=", 1))
	resolved, err := resolver.Resolve(managedResolverAuthorizedContext(), plan, account, changed)
	require.ErrorIs(t, err, ErrManagedFileUnsupportedInput)
	require.Nil(t, resolved)
	require.Zero(t, calls.Load())
}

func managedInlineTestAccount(controlURL string) *Account {
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = controlURL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	return &account
}

func TestManagedInlineResolverRejectsEntireMixedRequestBeforeAnyOpenOrStaging(t *testing.T) {
	for name, body := range map[string]string{
		"public then invalid inline": `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"late.txt","file_data":"!invalid"}]}]}`,
		"inline then foreign public": `{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="},{"type":"input_file","file_id":"file-foreign"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-public": managedResolverRecord("file-public")}}
			ledger := &managedResolverLedger{}
			files := managedResolverFiles(t, repo)
			resolver := NewManagedFileResolver(files, ledger, nil, nil)
			plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, []byte(body))
			require.Error(t, err)
			require.Nil(t, plan)
			opened, _ := repo.readActivity()
			require.Empty(t, opened)
			require.Zero(t, ledger.lookupCalls)
			entries, err := os.ReadDir(files.cfg.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestManagedInlineCollectorIncludesMixedPublicReferencesAndCandidate(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="},{"type":"input_file","file_id":"file-public"}]}]}`)
	refs, err := CollectManagedResponseFiles(body)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "file-public", refs[0].PublicID)
	require.Equal(t, 1, refs[0].Positions[0].Part)
	for _, input := range []string{`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`, `{"input":[{"type":"function_call_output","output":[{"type":"input_file","file_data":"aGVsbG8="}]}]}`} {
		require.True(t, HasManagedResponseFiles([]byte(input)))
	}
	require.False(t, HasManagedResponseFiles([]byte(`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_file\",\"file_data\":\"opaque\"}"}]}`)))
}

type managedInlineResolverLedger struct {
	*managedResolverLedger
	inline                                         *ManagedInlineReservation
	inlineStarts, inlineCompletes, inlineUncertain []string
	inlineAuthorizations, inlineReleases           int
	lateConfirmation                               bool
	finalAuthorizationErr                          error
}

func (l *managedInlineResolverLedger) ReserveInline(_ context.Context, in ManagedInlineReserveInput) (*ManagedInlineReservation, error) {
	l.inline = &ManagedInlineReservation{Owner: in.Owner, Selection: in.Selection, RequestID: in.RequestID, LeaseToken: in.LeaseToken}
	for _, f := range in.Files {
		l.inline.Files = append(l.inline.Files, ManagedInlineBinding{Descriptor: f, Operation: ManagedFileOperation{ID: "operation-" + f.FileRef, State: "reserved"}})
	}
	return l.inline, nil
}
func (l *managedInlineResolverLedger) StartInline(_ context.Context, _ *ManagedInlineReservation, ref string) error {
	l.inlineStarts = append(l.inlineStarts, ref)
	return nil
}
func (l *managedInlineResolverLedger) CompleteInline(_ context.Context, _ *ManagedInlineReservation, ref string, _ managedfiles.EnsureResult) (bool, error) {
	l.inlineCompletes = append(l.inlineCompletes, ref)
	return !l.lateConfirmation, nil
}
func (l *managedInlineResolverLedger) MarkInlineUncertain(_ context.Context, _ *ManagedInlineReservation, ref, _ string) error {
	l.inlineUncertain = append(l.inlineUncertain, ref)
	return nil
}
func (l *managedInlineResolverLedger) AuthorizeInlineGeneration(context.Context, *ManagedInlineReservation) error {
	l.inlineAuthorizations++
	return l.finalAuthorizationErr
}
func (l *managedInlineResolverLedger) ReleaseInline(context.Context, *ManagedInlineReservation) error {
	l.inlineReleases++
	return nil
}

func TestManagedInlineResolverUploadsWithDurableRequestFence(t *testing.T) {
	for _, tc := range []struct {
		name, publicMode                                      string
		uploadFailure, lateConfirmation, authorizationFailure bool
	}{
		{name: "inline only"},
		{name: "mixed public upload", publicMode: "reserved"},
		{name: "mixed public confirmed", publicMode: "succeeded"},
		{name: "unknown upload outcome", uploadFailure: true},
		{name: "late confirmed operation", lateConfirmation: true},
		{name: "final inline authorization revoked", authorizationFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
			if tc.publicMode != "" {
				repo.files["file-public"] = managedResolverRecord("file-public")
			}
			ledger := &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{existing: tc.publicMode == "succeeded"}, lateConfirmation: tc.lateConfirmation}
			if tc.authorizationFailure {
				ledger.finalAuthorizationErr = ErrFileLeaseConflict
			}
			var manifest managedfiles.AdmitManifest
			var results []managedfiles.EnsureResult
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case managedfiles.Prefix + "describe":
					json.NewEncoder(w).Encode(bpsFilesTestDescription())
				case managedfiles.Prefix + "admit":
					require.NotNil(t, ledger.inline, "write-ahead inline reservation precedes remote staging")
					mr, err := r.MultipartReader()
					require.NoError(t, err)
					part, err := mr.NextPart()
					require.NoError(t, err)
					raw, err := io.ReadAll(part)
					require.NoError(t, err)
					manifest, err = managedfiles.DecodeManifest(raw)
					require.NoError(t, err)
					wantFiles := 1
					if tc.publicMode != "" {
						wantFiles = 3
					}
					require.Len(t, manifest.Files, wantFiles)
					admitted := []managedfiles.AdmittedFile{}
					for _, f := range manifest.Files {
						require.Equal(t, "text/plain", f.MIME)
						require.EqualValues(t, 5, f.Bytes)
						if f.FileRef == "file-public" {
							require.Equal(t, []managedfiles.Position{{Input: 0, Part: 2}, {Input: 0, Part: 3}}, f.Positions)
						} else {
							require.Contains(t, f.FileRef, "file-inline-")
						}
						if f.Mode == "existing" {
							admitted = append(admitted, managedfiles.AdmittedFile{FileRef: f.FileRef, State: "known"})
							continue
						}
						part, err = mr.NextPart()
						require.NoError(t, err)
						require.Equal(t, f.Part, part.FormName())
						content, err := io.ReadAll(part)
						require.NoError(t, err)
						require.Equal(t, "hello", string(content))
						admitted = append(admitted, managedfiles.AdmittedFile{FileRef: f.FileRef, State: "staged"})
					}
					_, err = mr.NextPart()
					require.ErrorIs(t, err, io.EOF)
					require.NotContains(t, string(manifest.RequestTemplate), "file_data")
					require.Contains(t, string(manifest.RequestTemplate), "9007199254740993")
					hash, err := managedfiles.TemplateHash(manifest.RequestTemplate)
					require.NoError(t, err)
					json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "inline-admission", ExpiresAt: time.Now().Add(time.Minute).Unix(), TemplateSHA256: hash, Files: admitted})
				case managedfiles.Prefix + "ensure-uploaded":
					var req managedfiles.EnsureRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					var file managedfiles.File
					for _, f := range manifest.Files {
						if f.FileRef == req.FileRef {
							file = f
						}
					}
					if req.FileRef == "file-public" {
						require.Contains(t, ledger.started, req.FileRef)
					} else {
						require.Contains(t, ledger.inlineStarts, req.FileRef, "durable inline Start must precede upload")
					}
					if tc.uploadFailure {
						w.WriteHeader(502)
						return
					}
					remote := "file-inline-remote-" + req.FileRef
					result := managedfiles.EnsureResult{Contract: managedfiles.Contract, Identity: bpsFilesTestIdentity(), OperationID: req.OperationID, FileRef: file.FileRef, RemoteID: remote, Bytes: file.Bytes, SHA256: file.SHA256, Filename: file.Filename, MIME: file.MIME, InputTokens: 3}
					results = append(results, result)
					uploads := 0
					for _, f := range manifest.Files {
						if f.Mode == "upload" {
							uploads++
						}
					}
					result.Ready = len(results) == uploads
					if result.Ready {
						bound, err := managedfiles.Bind(manifest, results)
						require.NoError(t, err)
						result.WireSHA256 = bound.WireSHA256
						result.BindingSHA256 = bound.BindingSHA256
					}
					json.NewEncoder(w).Encode(result)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			files := managedResolverFiles(t, repo)
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
			resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
			parts := `{"type":"input_text","text":"read"},{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}`
			if tc.publicMode != "" {
				parts += `,{"type":"input_file","file_id":"file-public"},{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"second.txt","file_data":"data:text/plain;base64,aGVsbG8="}`
			}
			body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[` + parts + `]}],"metadata":{"large":9007199254740993}}`)
			plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
			require.NoError(t, err)
			defer plan.Close()
			resolved, err := resolver.Resolve(managedResolverAuthorizedContext(), plan, managedInlineTestAccount(server.URL), body)
			if tc.uploadFailure || tc.lateConfirmation || tc.authorizationFailure {
				require.Error(t, err)
				require.Nil(t, resolved)
				if tc.uploadFailure {
					require.Equal(t, ledger.inlineStarts, ledger.inlineUncertain)
					require.Empty(t, ledger.inlineCompletes)
				}
				if tc.lateConfirmation {
					require.Len(t, ledger.inlineCompletes, 1)
					require.Zero(t, ledger.inlineAuthorizations)
					require.Empty(t, ledger.inlineUncertain)
				}
			} else {
				require.NoError(t, err)
				require.NotNil(t, resolved)
				require.Contains(t, string(resolved.Body), `"file_id":"file-inline-remote-`)
				require.NotContains(t, string(resolved.Body), "file_data")
				require.Equal(t, ledger.inlineStarts, ledger.inlineCompletes)
				wantInline := 1
				if tc.publicMode != "" {
					wantInline = 2
				}
				require.Len(t, ledger.inlineCompletes, wantInline)
				require.NoError(t, resolved.Authorize(managedResolverAuthorizedContext()))
				require.Positive(t, ledger.inlineAuthorizations)
				if tc.publicMode == "succeeded" {
					require.Empty(t, ledger.started)
					require.Empty(t, ledger.completed)
					require.Contains(t, string(resolved.Body), `"file_id":"file-existing"`)
				}
				if tc.publicMode == "reserved" {
					require.Equal(t, []string{"file-public"}, ledger.started)
					require.Equal(t, ledger.started, ledger.completed)
				}
			}
			require.NoError(t, plan.Close())
			require.Equal(t, 1, ledger.inlineReleases)
			if tc.publicMode == "" {
				require.Nil(t, ledger.reservation, "inline request must never reserve permanent Files bindings")
				require.Empty(t, repo.files)
			} else {
				require.Equal(t, 1, ledger.releases)
			}
			_, released := repo.readActivity()
			wantReleased := 0
			if tc.publicMode != "" {
				wantReleased = 1
			}
			require.Len(t, released, wantReleased)
			entries, err := os.ReadDir(files.cfg.Root)
			require.NoError(t, err)
			require.Empty(t, entries)
			if resolved != nil {
				require.Error(t, resolved.Authorize(managedResolverAuthorizedContext()))
			}
		})
	}
}

func TestManagedInlineResolverRejectsPersistentStoreBeforeLocalOrRemoteActivity(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-public": managedResolverRecord("file-public")}}
	files := managedResolverFiles(t, repo)
	ledger := &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{}}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"store":true,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	if plan != nil {
		defer plan.Close()
	}
	require.ErrorIs(t, err, ErrManagedFileUnsupportedInput)
	require.Nil(t, plan)
	opened, _ := repo.readActivity()
	require.Empty(t, opened)
	require.Zero(t, ledger.lookupCalls)
	require.Zero(t, calls.Load())
	entries, err := os.ReadDir(files.cfg.Root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestManagedInlineResolverRejectsFinalPersistentStoreBeforeRemoteActivity(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{}}
	files := managedResolverFiles(t, repo)
	ledger := &managedInlineResolverLedger{managedResolverLedger: &managedResolverLedger{}}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(files, ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"store":false,"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	changed := []byte(strings.Replace(string(body), `"store":false`, `"store":true`, 1))
	resolved, err := resolver.Resolve(managedResolverAuthorizedContext(), plan, managedInlineTestAccount(server.URL), changed)
	require.ErrorIs(t, err, ErrManagedFileUnsupportedInput)
	require.Nil(t, resolved)
	require.Zero(t, calls.Load())
	require.Nil(t, ledger.inline)
}
