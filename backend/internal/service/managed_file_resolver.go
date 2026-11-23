package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"io"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
)

const managedFileReservationLease = 15 * time.Minute

type ManagedFileResolver struct {
	files    *FileService
	ledger   ManagedFileRepository
	upstream HTTPUpstream
	cfg      *config.Config
	// stageURLs substitutes the private downloader boundary in package tests.
	stageURLs func(context.Context, []ManagedFileURLSource, []ManagedInlineFileDescriptor, ManagedInlineLimits, string) (*ManagedInlineStaging, error)
}

func NewManagedFileResolver(files *FileService, ledger ManagedFileRepository, upstream HTTPUpstream, cfg *config.Config) *ManagedFileResolver {
	return &ManagedFileResolver{files: files, ledger: ledger, upstream: upstream, cfg: cfg}
}
func (s *ManagedFileResolver) Enabled() bool { return s != nil && s.files.Enabled() && s.ledger != nil }

type managedFilePlanKey struct{}

func WithManagedFilePlan(ctx context.Context, plan *ManagedFilePlan) context.Context {
	return context.WithValue(ctx, managedFilePlanKey{}, plan)
}
func ManagedFilePlanFromContext(ctx context.Context) *ManagedFilePlan {
	plan, _ := ctx.Value(managedFilePlanKey{}).(*ManagedFilePlan)
	return plan
}

type ManagedFilePlan struct {
	RequiredAccountID int64
	owner             FileOwner
	refs              []ManagedFileReference
	files             []*StoredFile
	preparation       *ManagedInlinePreparation
	staging           *ManagedInlineStaging
	inlineRefs        map[managedfiles.Position]string
	requestID         string
	inlineReservation *ManagedInlineReservation
	inlineLedger      ManagedInlineOperationRepository
	readers           map[string]io.ReadCloser
	resolver          *ManagedFileResolver
	reservation       *ManagedFileReservation
	mu                sync.Mutex
	closed            bool
	resolving         bool
	once              sync.Once
	closeErr          error
}

func (p *ManagedFilePlan) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.once.Do(func() {
		p.closed = true
		for _, reader := range p.readers {
			p.closeErr = errors.Join(p.closeErr, reader.Close())
		}
		if p.reservation != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p.closeErr = errors.Join(p.closeErr, p.resolver.ledger.ReleaseReservation(ctx, p.reservation))
		}
		if p.inlineReservation != nil && p.inlineLedger != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			p.closeErr = errors.Join(p.closeErr, p.inlineLedger.ReleaseInline(ctx, p.inlineReservation))
		}
	})
	var stagingErr error
	if p.staging != nil {
		stagingErr = p.staging.Close()
	}
	return errors.Join(p.closeErr, stagingErr)
}
func (s *ManagedFileResolver) Prepare(ctx context.Context, owner FileOwner, body []byte) (_ *ManagedFilePlan, retErr error) {
	preparation, err := PreflightManagedInlineFiles(ctx, body, DefaultManagedInlineLimits())
	if err != nil {
		return nil, err
	}
	if len(preparation.Files()) == 0 {
		return nil, nil
	}
	if managedResponseStorageUnsupported(body) {
		return nil, ErrManagedFileUnsupportedInput
	}
	refs := managedPublicReferences(preparation.Files())
	if !s.Enabled() {
		return nil, ErrFilesDisabled
	}
	if owner.UserID <= 0 || owner.GroupID <= 0 || owner.APIKeyID <= 0 {
		return nil, ErrInvalidFile
	}
	plan := &ManagedFilePlan{owner: owner, refs: refs, readers: make(map[string]io.ReadCloser), resolver: s}
	defer func() {
		if retErr != nil {
			_ = plan.Close()
		}
	}()
	ids := make([]string, 0, len(refs))
	metadata := make([]ManagedInlinePublicFile, 0, len(refs))
	snapshots := make(map[string]*StoredFile, len(refs))
	for _, ref := range refs {
		f, err := s.files.Get(ctx, owner, ref.PublicID)
		if err != nil {
			return nil, err
		}
		if !managedResolverReady(f, owner) {
			return nil, ErrFileNotFound
		}
		if f.Bytes <= 0 || f.Bytes > managedfiles.MaxFileBytes {
			return nil, ErrInvalidFile
		}
		metadata = append(metadata, ManagedInlinePublicFile{PublicID: f.PublicID, Bytes: f.Bytes})
		snapshots[ref.PublicID] = f
		ids = append(ids, ref.PublicID)
	}
	checked, err := preparation.WithPublicFiles(metadata)
	if err != nil {
		return nil, err
	}
	plan.preparation = checked
	if len(ids) > 0 {
		lookup, err := s.ledger.LookupBindings(ctx, owner, ids)
		if err != nil {
			return nil, err
		}
		plan.RequiredAccountID = lookup.AccountID
	}
	for _, ref := range refs {
		f, reader, err := s.files.Open(ctx, owner, ref.PublicID)
		if err != nil {
			return nil, err
		}
		plan.readers[ref.PublicID] = reader
		if !managedResolverReady(f, owner) {
			return nil, ErrFileNotFound
		}
		before := snapshots[ref.PublicID]
		if f.Version != before.Version || f.Bytes != before.Bytes || f.SHA256 != before.SHA256 || f.Filename != before.Filename || f.MIME != before.MIME {
			return nil, ErrFileLeaseConflict
		}
		plan.files = append(plan.files, f)
	}
	hasInline := false
	for _, file := range checked.Files() {
		hasInline = hasInline || file.PublicID == ""
	}
	if hasInline {
		plan.staging, err = StageManagedInlineFiles(ctx, checked, s.files.cfg.Root)
		if err != nil {
			return nil, err
		}
		var sources []ManagedFileURLSource
		var existing []ManagedInlineFileDescriptor
		for _, entry := range checked.entries {
			if entry.sourceURL != "" {
				sources = append(sources, ManagedFileURLSource{URL: entry.sourceURL, Filename: entry.description.Filename, Position: entry.description.Position})
			} else {
				existing = append(existing, entry.description)
			}
		}
		if len(sources) > 0 {
			download := s.stageURLs
			if download == nil {
				download = StageManagedFileURLs
			}
			urls, err := download(ctx, sources, existing, checked.limits, s.files.cfg.Root)
			if err != nil {
				return nil, err
			}
			combined, err := combineManagedInlineStages(ctx, checked, plan.staging, urls)
			if err != nil {
				return nil, err
			}
			plan.staging = combined
		}
		plan.requestID, err = fileToken()
		if err != nil {
			return nil, err
		}
		plan.inlineRefs = make(map[managedfiles.Position]string)
		for _, file := range checked.Files() {
			if file.PublicID != "" {
				continue
			}
			token, err := fileToken()
			if err != nil {
				return nil, err
			}
			plan.inlineRefs[file.Position] = "file-inline-" + token
		}
	}
	return plan, nil
}
func managedResolverReady(f *StoredFile, owner FileOwner) bool {
	return f != nil && f.UserID == owner.UserID && f.APIKeyID == owner.APIKeyID && f.GroupID == owner.GroupID && f.State == "ready" && f.RetiredAt == nil && f.LocalPurgedAt == nil && (f.ExpiresAt == nil || time.Now().Before(*f.ExpiresAt))
}

type ManagedFileResolved struct {
	Body                                           []byte
	AdmissionID, OwnerRef, GroupRef, BindingSHA256 string
	AccountID                                      int64
	Config                                         BPSFilesBackendConfig
	nativeCopies                                   []managedfiles.NativeAttachmentWireCopy15
	authorize                                      func(context.Context) error
}

func (r *ManagedFileResolved) Authorize(ctx context.Context) error {
	if r == nil || r.authorize == nil {
		return ErrManagedFileNotReady
	}
	return r.authorize(ctx)
}
func (s *ManagedFileResolver) Resolve(ctx context.Context, plan *ManagedFilePlan, account *Account, finalBody []byte) (*ManagedFileResolved, error) {
	if !s.Enabled() || plan == nil || plan.resolver != s {
		return nil, ErrManagedFileNotReady
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.closed || plan.resolving {
		return nil, ErrManagedFileNotReady
	}
	plan.resolving = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	finalPreparation, err := PreflightManagedInlineFiles(ctx, finalBody, DefaultManagedInlineLimits())
	if err != nil || managedResponseStorageUnsupported(finalBody) || !sameManagedFilePreparation(plan.preparation, finalPreparation) {
		return nil, ErrManagedFileUnsupportedInput
	}
	if plan.staging != nil {
		var ok bool
		plan.inlineLedger, ok = s.ledger.(ManagedInlineOperationRepository)
		if !ok {
			return nil, ErrManagedFileNotReady
		}
	}
	template, err := managedInlineTemplate(plan, finalBody)
	if err != nil {
		return nil, err
	}
	backendConfig, err := BPSFilesConfigForAccount(account)
	if err != nil {
		return nil, ErrManagedFileNotReady
	}
	if plan.RequiredAccountID != 0 && plan.RequiredAccountID != account.ID {
		return nil, ErrManagedFileBindingConflict
	}
	backend, err := NewBPSFilesBackend(backendConfig, s.upstream, s.cfg)
	if err != nil {
		return nil, err
	}
	resolveCtx, cancel := context.WithTimeout(ctx, managedFileReservationLease)
	defer cancel()
	if err := checkManagedFilePreparationAuthorization(resolveCtx); err != nil {
		return nil, err
	}
	description, err := describeBPSSessionBinding(resolveCtx, backend)
	if err != nil {
		return nil, err
	}
	if err := checkManagedFilePreparationAuthorization(resolveCtx); err != nil {
		return nil, err
	}
	token, err := fileToken()
	if err != nil {
		return nil, err
	}
	requestID, err := fileToken()
	if err != nil {
		return nil, err
	}
	if plan.requestID != "" {
		requestID = plan.requestID
	}
	selection := ManagedFileSelection{AccountID: account.ID, Identity: backendConfig.Expected}
	versions := make([]ManagedFileVersion, 0, len(plan.files))
	for _, file := range plan.files {
		versions = append(versions, ManagedFileVersion{PublicID: file.PublicID, Version: file.Version})
	}
	var reservation *ManagedFileReservation
	bindings := make(map[string]ManagedFileBinding, len(plan.files))
	if len(versions) > 0 {
		reservation, err = s.ledger.Reserve(resolveCtx, ManagedFileReserveInput{Owner: plan.owner, Files: versions, Selection: selection, LeaseToken: token, Lease: managedFileReservationLease})
		if err != nil {
			return nil, err
		}
		plan.reservation = reservation
		if reservation == nil || reservation.Owner != plan.owner || reservation.Selection != selection || len(reservation.Files) != len(plan.files) {
			return nil, ErrManagedFileNotReady
		}
		for _, binding := range reservation.Files {
			if binding.File == nil || bindings[binding.File.PublicID].File != nil {
				return nil, ErrManagedFileNotReady
			}
			bindings[binding.File.PublicID] = binding
		}
	}
	manifest := managedfiles.AdmitManifest{Contract: managedfiles.Contract, JSONProfile: managedfiles.JSONProfile, RequestID: requestID, Expected: backendConfig.Expected, OwnerRef: strconv.FormatInt(plan.owner.UserID, 10), GroupRef: strconv.FormatInt(plan.owner.GroupID, 10), RequestTemplate: template}
	if conversation := bpsSessionBindingConversation(ctx); conversation != "" {
		manifest.SessionBindingContract = managedfiles.SessionBindingContract
		manifest.ConversationRef = conversation
	}
	readers := make(map[string]io.Reader)
	var total int64
	for i, file := range plan.files {
		binding, ok := bindings[file.PublicID]
		if !ok || binding.File.Version != file.Version || binding.File.Bytes != file.Bytes || binding.File.SHA256 != file.SHA256 || binding.File.Filename != file.Filename || binding.File.MIME != file.MIME || binding.Selection != reservation.Selection || binding.Operation.ID == "" {
			return nil, ErrManagedFileNotReady
		}
		f := managedfiles.File{FileRef: file.PublicID, Filename: file.Filename, MIME: file.MIME, Bytes: file.Bytes, SHA256: file.SHA256, Positions: plan.refs[i].Positions}
		switch binding.Operation.State {
		case "reserved":
			f.Mode = "upload"
			f.Part = fmt.Sprintf("file_%d", i)
			readers[file.PublicID] = plan.readers[file.PublicID]
			total += file.Bytes
		case "succeeded":
			f.Mode = "existing"
			f.RemoteID = binding.Operation.RemoteID
			f.OperationID = binding.Operation.ID
		case "started", "uncertain":
			return nil, ErrManagedFileUncertain
		default:
			return nil, ErrManagedFileNotReady
		}
		manifest.Files = append(manifest.Files, f)
	}
	inlineBindings := make(map[string]ManagedInlineBinding)
	if plan.staging != nil {
		staged := plan.staging.Files()
		descriptors := make([]ManagedInlineDescriptor, 0, len(plan.inlineRefs))
		for _, file := range staged {
			if file.PublicID == "" {
				descriptors = append(descriptors, ManagedInlineDescriptor{FileRef: plan.inlineRefs[file.Position], Filename: file.Filename, MIME: file.MIME, SHA256: file.SHA256, Bytes: file.Bytes})
			}
		}
		inline, err := plan.inlineLedger.ReserveInline(resolveCtx, ManagedInlineReserveInput{Owner: plan.owner, Selection: selection, RequestID: requestID, LeaseToken: token, Lease: managedFileReservationLease, Files: descriptors})
		if err != nil {
			return nil, err
		}
		plan.inlineReservation = inline
		if inline == nil || inline.Owner != plan.owner || inline.Selection != selection || inline.RequestID != requestID || inline.LeaseToken != token || len(inline.Files) != len(descriptors) {
			return nil, ErrManagedFileNotReady
		}
		for _, binding := range inline.Files {
			if inlineBindings[binding.Descriptor.FileRef].Operation.ID != "" || binding.Operation.ID == "" || binding.Operation.State != "reserved" {
				return nil, ErrManagedFileNotReady
			}
			inlineBindings[binding.Descriptor.FileRef] = binding
		}
		for i, file := range staged {
			if file.PublicID != "" {
				continue
			}
			ref := plan.inlineRefs[file.Position]
			binding, ok := inlineBindings[ref]
			want := ManagedInlineDescriptor{FileRef: ref, Filename: file.Filename, MIME: file.MIME, SHA256: file.SHA256, Bytes: file.Bytes}
			if !ok || binding.Descriptor != want {
				return nil, ErrManagedFileNotReady
			}
			reader, err := plan.staging.Open(i)
			if err != nil {
				return nil, err
			}
			plan.readers[ref], readers[ref] = reader, reader
			manifest.Files = append(manifest.Files, managedfiles.File{FileRef: ref, Mode: "upload", Part: fmt.Sprintf("file_%d", len(manifest.Files)), Filename: file.Filename, MIME: file.MIME, Bytes: file.Bytes, SHA256: file.SHA256, Positions: []managedfiles.Position{file.Position}})
			total += file.Bytes
		}
	}
	if err := managedfiles.ValidateManifest(manifest); err != nil {
		return nil, err
	}
	limits := description.Limits
	encodedManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, ErrManagedFileUnsupportedInput
	}
	if len(manifest.Files) > limits.UniqueFiles || int64(len(encodedManifest)) > limits.ManifestBytes || total > limits.AdmissionBytes {
		return nil, bpsFilesError("admission_too_large")
	}
	type budget struct {
		positions int
		bytes     int64
	}
	budgets := map[int]budget{}
	for _, f := range manifest.Files {
		if f.Bytes > limits.FileBytes {
			return nil, bpsFilesError("file_too_large")
		}
		for _, position := range f.Positions {
			b := budgets[position.Input]
			if b.positions >= limits.MessagePositions || f.Bytes > limits.MessageBytes-b.bytes {
				return nil, bpsFilesError("message_too_large")
			}
			b.positions++
			b.bytes += f.Bytes
			budgets[position.Input] = b
		}
	}
	if err := checkManagedFilePreparationAuthorization(resolveCtx); err != nil {
		return nil, err
	}
	var admission managedfiles.AdmitResult
	if description.Capabilities.MetadataRecovery {
		round := new(ManagedFileRecoveryRound)
		recovered, recoveryErr := round.Admit(resolveCtx, ManagedFileRecoveryAdmissionInput{
			Plan: plan, Backend: backend, Manifest: manifest, Reservation: reservation, Limits: limits,
			Authorize: checkManagedFilePreparationAuthorization, Open: managedRecoveryPlanOpener(plan, reservation),
		})
		if recoveryErr != nil {
			return nil, recoveryErr
		}
		manifest, reservation, admission = recovered.Manifest, recovered.Reservation, recovered.Admission
		bindings = managedRecoveryBindings(reservation)
	} else {
		admission, err = backend.Admit(resolveCtx, manifest, readers)
	}
	if err != nil {
		return nil, err
	}
	expires := time.Unix(admission.ExpiresAt, 0)
	results := make([]managedfiles.EnsureResult, 0, len(manifest.Files))
	ready := admission.Ready
	for _, file := range manifest.Files {
		if file.Mode != "upload" {
			continue
		}
		if err := resolveCtx.Err(); err != nil {
			return nil, err
		}
		if !time.Now().Before(expires) {
			return nil, ErrManagedFileNotReady
		}
		binding, permanent := bindings[file.FileRef]
		if err := checkManagedFilePreparationAuthorization(resolveCtx); err != nil {
			return nil, err
		}
		operationID := inlineBindings[file.FileRef].Operation.ID
		if permanent {
			operationID = binding.Operation.ID
			err = s.ledger.StartOperation(resolveCtx, reservation, file.FileRef)
		} else {
			err = plan.inlineLedger.StartInline(resolveCtx, plan.inlineReservation, file.FileRef)
		}
		if err != nil {
			return nil, err
		}
		// Durable Start can wait on database locks. Revocation during that wait
		// must still prevent the external upload; retain the started fact.
		if err := checkManagedFilePreparationAuthorization(resolveCtx); err != nil {
			s.markPlanFileUncertain(plan, file.FileRef, permanent)
			return nil, err
		}
		result, err := backend.EnsureUploaded(resolveCtx, managedfiles.EnsureRequest{Contract: managedfiles.Contract, Expected: backendConfig.Expected, AdmissionID: admission.AdmissionID, FileRef: file.FileRef, OperationID: operationID}, file)
		if err != nil {
			s.markPlanFileUncertain(plan, file.FileRef, permanent)
			return nil, err
		}
		// Preserve a confirmed remote copy even when caller cancellation or a late
		// deadline would otherwise prevent this local acknowledgement from committing.
		persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
		var active bool
		var completeErr error
		if permanent {
			active, completeErr = s.ledger.CompleteOperation(persistCtx, reservation, file.FileRef, result)
		} else {
			active, completeErr = plan.inlineLedger.CompleteInline(persistCtx, plan.inlineReservation, file.FileRef, result)
		}
		persistCancel()
		if completeErr != nil {
			s.markPlanFileUncertain(plan, file.FileRef, permanent)
			return nil, completeErr
		}
		if !active {
			return nil, ErrFileLeaseConflict
		}
		results = append(results, result)
		ready = result.Ready
	}
	if !ready {
		return nil, ErrManagedFileNotReady
	}
	bound, err := managedfiles.Bind(manifest, results)
	if err != nil {
		return nil, err
	}
	if admission.Ready && (admission.WireSHA256 != bound.WireSHA256 || admission.BindingSHA256 != bound.BindingSHA256) {
		return nil, ErrManagedFileNotReady
	}
	if err := resolveCtx.Err(); err != nil {
		return nil, err
	}
	resolved := &ManagedFileResolved{Body: bound.Wire, AdmissionID: admission.AdmissionID, OwnerRef: manifest.OwnerRef, GroupRef: manifest.GroupRef, BindingSHA256: bound.BindingSHA256, AccountID: account.ID, Config: backendConfig}
	// Confirmed copy facts are retained separately from the fresh lease closure.
	// They are evidence for native history, never permission to read a file.
	if plan.staging == nil {
		byResult := make(map[string]managedfiles.EnsureResult, len(results))
		for _, result := range results {
			byResult[result.FileRef] = result
		}
		for _, file := range manifest.Files {
			binding, ok := bindings[file.FileRef]
			if !ok || binding.File == nil {
				return nil, ErrManagedFileNotReady
			}
			remote, operation, tokens := file.RemoteID, file.OperationID, binding.Operation.InputTokens
			if file.Mode == "upload" {
				result, ok := byResult[file.FileRef]
				if !ok {
					return nil, ErrManagedFileNotReady
				}
				remote, operation, tokens = result.RemoteID, result.OperationID, result.InputTokens
			}
			resolved.nativeCopies = append(resolved.nativeCopies, managedfiles.NativeAttachmentWireCopy15{
				PublicFileID: file.FileRef, FileVersion: binding.File.Version, SHA256: file.SHA256,
				Filename: file.Filename, MIME: file.MIME, Bytes: file.Bytes, AccountID: account.ID,
				BackendID: backendConfig.Expected.BackendID, SubjectDigest: managedfiles.NativeResponsesSubjectDigest15(backendConfig.Expected),
				CredentialGeneration: backendConfig.Expected.CredentialGeneration, OperationID: operation, RemoteID: remote, InputTokens: tokens,
			})
		}
	}
	resolved.authorize = func(c context.Context) error {
		plan.mu.Lock()
		defer plan.mu.Unlock()
		if plan.closed || !time.Now().Before(expires) {
			return ErrManagedFileNotReady
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.authorizeManagedPlan(c, plan); err != nil {
			return err
		}
		if !time.Now().Before(expires) {
			return ErrManagedFileNotReady
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return c.Err()
	}
	// The transport repeats this check immediately before the one external send.
	if !time.Now().Before(expires) {
		return nil, ErrManagedFileNotReady
	}
	if err := s.authorizeManagedPlan(resolveCtx, plan); err != nil {
		return nil, err
	}
	if !time.Now().Before(expires) {
		return nil, ErrManagedFileNotReady
	}
	return resolved, nil
}

func managedInlineTemplate(plan *ManagedFilePlan, body []byte) ([]byte, error) {
	if len(plan.inlineRefs) == 0 {
		return append([]byte(nil), body...), nil
	}
	value, err := managedfiles.Parse(body)
	if err != nil {
		return nil, ErrManagedFileUnsupportedInput
	}
	root := value.(map[string]any)
	for position, ref := range plan.inlineRefs {
		part, err := managedfiles.FilePartAt(root, position)
		if err != nil {
			return nil, ErrManagedFileUnsupportedInput
		}
		part["file_id"] = ref
		delete(part, "file_data")
		delete(part, "file_url")
	}
	return json.Marshal(root)
}

func (s *ManagedFileResolver) authorizeManagedPlan(ctx context.Context, plan *ManagedFilePlan) error {
	if err := checkManagedFilePreparationAuthorization(ctx); err != nil {
		return err
	}
	if plan.inlineReservation != nil {
		if err := plan.inlineLedger.AuthorizeInlineGeneration(ctx, plan.inlineReservation); err != nil {
			return err
		}
	}
	if plan.reservation != nil {
		if err := s.ledger.AuthorizeGeneration(ctx, plan.reservation); err != nil {
			return err
		}
	}
	// Keep durable file ownership/retirement fences after external policy waits.
	// Mixed inline/durable batches use separate transactions; their commits are
	// not an atomic fence against later concurrent changes.
	return ctx.Err()
}

func (s *ManagedFileResolver) markPlanFileUncertain(plan *ManagedFilePlan, ref string, permanent bool) {
	if permanent {
		s.markUncertain(plan.reservation, ref)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = plan.inlineLedger.MarkInlineUncertain(ctx, plan.inlineReservation, ref, "upload_outcome_unknown")
}

func sameManagedFilePreparation(original, final *ManagedInlinePreparation) bool {
	if original == nil || final == nil || len(original.entries) != len(final.entries) {
		return false
	}
	for i, before := range original.entries {
		after := final.entries[i]
		if before.description.PublicID != "" {
			before.description.Bytes = 0 // Fresh owner metadata supplies this budget.
		}
		if !reflect.DeepEqual(before, after) {
			return false
		}
	}
	return true
}
func (s *ManagedFileResolver) markUncertain(reservation *ManagedFileReservation, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.ledger.MarkUncertain(ctx, reservation, ref, "upload_outcome_unknown")
}
