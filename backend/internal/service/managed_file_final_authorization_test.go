package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/stretchr/testify/require"
)

type managedFinalAuthorizationLedger struct {
	ManagedFileRepository
	ManagedInlineOperationRepository
	authorize func(context.Context) error
	inline    func(context.Context) error
}

func (l *managedFinalAuthorizationLedger) AuthorizeGeneration(ctx context.Context, _ *ManagedFileReservation) error {
	return l.authorize(ctx)
}

func (l *managedFinalAuthorizationLedger) AuthorizeInlineGeneration(ctx context.Context, _ *ManagedInlineReservation) error {
	return l.inline(ctx)
}

func TestManagedFileFinalAuthorizationChecksRetirementAfterPolicyWait(t *testing.T) {
	for _, reason := range []error{ErrFileNotFound, ErrFileLeaseConflict} {
		t.Run(reason.Error(), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var retired atomic.Bool
			ledger := &managedFinalAuthorizationLedger{authorize: func(context.Context) error {
				if retired.Load() {
					return reason
				}
				return nil
			}}
			resolver := &ManagedFileResolver{ledger: ledger}
			ctx := withManagedFilePreparationAuthorization(context.Background(), func(context.Context) error {
				close(entered)
				<-release
				return nil
			})
			result := make(chan error, 1)
			go func() {
				result <- resolver.authorizeManagedPlan(ctx, &ManagedFilePlan{reservation: &ManagedFileReservation{}})
			}()
			<-entered
			retired.Store(true)
			close(release)
			require.ErrorIs(t, <-result, reason)
		})
	}
}

func TestManagedFileFinalAuthorizationNeverWaitsOnPolicyAfterLedger(t *testing.T) {
	var order []string
	ledger := &managedFinalAuthorizationLedger{
		inline:    func(context.Context) error { order = append(order, "inline"); return nil },
		authorize: func(context.Context) error { order = append(order, "durable"); return nil },
	}
	ctx := withManagedFilePreparationAuthorization(context.Background(), func(context.Context) error {
		order = append(order, "policy")
		return nil
	})
	resolver := &ManagedFileResolver{ledger: ledger}
	plan := &ManagedFilePlan{reservation: &ManagedFileReservation{}, inlineReservation: &ManagedInlineReservation{}, inlineLedger: ledger}
	require.NoError(t, resolver.authorizeManagedPlan(ctx, plan))
	require.Equal(t, []string{"policy", "inline", "durable"}, order)
}

func TestManagedFileFinalAuthorizationRechecksDurableAfterMixedInlineWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var retired atomic.Bool
	ledger := &managedFinalAuthorizationLedger{
		inline: func(context.Context) error { close(entered); <-release; return nil },
		authorize: func(context.Context) error {
			if retired.Load() {
				return ErrFileNotFound
			}
			return nil
		},
	}
	resolver := &ManagedFileResolver{ledger: ledger}
	plan := &ManagedFilePlan{reservation: &ManagedFileReservation{}, inlineReservation: &ManagedInlineReservation{}, inlineLedger: ledger}
	result := make(chan error, 1)
	go func() { result <- resolver.authorizeManagedPlan(managedResolverAuthorizedContext(), plan) }()
	<-entered
	retired.Store(true)
	close(release)
	require.ErrorIs(t, <-result, ErrFileNotFound)
}

func TestManagedFileFinalAuthorizationRechecksAdmissionExpiryAfterWait(t *testing.T) {
	repo := &managedResolverFileRepo{files: map[string]*StoredFile{"file-a": managedResolverRecord("file-a")}}
	ledger := &managedResolverLedger{lookup: ManagedFileLookup{AccountID: 7}, existing: true}
	expires := time.Now().Add(2 * time.Second).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case managedfiles.Prefix + "describe":
			json.NewEncoder(w).Encode(bpsFilesTestDescription())
		case managedfiles.Prefix + "admit":
			reader, err := r.MultipartReader()
			require.NoError(t, err)
			part, err := reader.NextPart()
			require.NoError(t, err)
			raw, err := io.ReadAll(part)
			require.NoError(t, err)
			manifest, err := managedfiles.DecodeManifest(raw)
			require.NoError(t, err)
			bound, err := managedfiles.Bind(manifest, nil)
			require.NoError(t, err)
			json.NewEncoder(w).Encode(managedfiles.AdmitResult{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, Identity: bpsFilesTestIdentity(), AdmissionID: "admission-expiry", ExpiresAt: expires, TemplateSHA256: bound.TemplateSHA256, Ready: true, WireSHA256: bound.WireSHA256, BindingSHA256: bound.BindingSHA256, Files: []managedfiles.AdmittedFile{{FileRef: "file-a", State: "known"}}})
		default:
			t.Errorf("unexpected external operation: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true}}}
	resolver := NewManagedFileResolver(managedResolverFiles(t, repo), ledger, &bpsFilesTestTransport{client: server.Client()}, cfg)
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-a"}]}]}`)
	plan, err := resolver.Prepare(context.Background(), FileOwner{UserID: 1, GroupID: 2, APIKeyID: 3}, body)
	require.NoError(t, err)
	defer plan.Close()
	account := managedSchedulerTestAccount(7, 2)
	identity := bpsFilesTestIdentity()
	account.Credentials["bps_files_control_url"] = server.URL
	account.Credentials["bps_files_backend_id"] = identity.BackendID
	account.Credentials["bps_files_subject_fingerprint"] = identity.SubjectFingerprint
	account.Credentials["bps_files_credential_generation"] = identity.CredentialGeneration
	resolved, err := resolver.Resolve(managedResolverAuthorizedContext(), plan, &account, body)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	ctx := withManagedFilePreparationAuthorization(context.Background(), func(context.Context) error { close(entered); <-release; return nil })
	result := make(chan error, 1)
	go func() { result <- resolved.Authorize(ctx) }()
	<-entered
	timer := time.NewTimer(time.Until(time.Unix(expires, 0).Add(time.Millisecond)))
	defer timer.Stop()
	<-timer.C
	close(release)
	require.ErrorIs(t, <-result, ErrManagedFileNotReady)
}

func TestManagedFileFinalAuthorizationChecksCancellationAfterLedger(t *testing.T) {
	ctx, cancel := context.WithCancel(managedResolverAuthorizedContext())
	ledger := &managedFinalAuthorizationLedger{authorize: func(context.Context) error { cancel(); return nil }}
	resolver := &ManagedFileResolver{ledger: ledger}
	require.ErrorIs(t, resolver.authorizeManagedPlan(ctx, &ManagedFilePlan{reservation: &ManagedFileReservation{}}), context.Canceled)
}
