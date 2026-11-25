package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

const bpsFilesResponseLimit int64 = 1 << 20

type BPSFilesBackendConfig struct {
	ControlURL, ServiceKey string
	Expected               managedfiles.Identity
	ProxyURL               string
	AccountID              int64
	Concurrency            int
}

// BPSFilesBackendError exposes fixed codes only; upstream payloads, URLs and
// service credentials never become client errors. Unknown upload outcomes
// must remain in the durable operation ledger instead of being retried.
type BPSFilesBackendError struct {
	Code           string
	HTTPStatus     int
	OutcomeUnknown bool
}

func (e *BPSFilesBackendError) Error() string         { return "bps files: " + e.Code }
func bpsFilesError(code string) *BPSFilesBackendError { return &BPSFilesBackendError{Code: code} }

type BPSFilesBackend struct {
	config   BPSFilesBackendConfig
	upstream HTTPUpstream
	baseURL  string
}

func NewBPSFilesBackend(c BPSFilesBackendConfig, upstream HTTPUpstream, cfg *config.Config) (*BPSFilesBackend, error) {
	validIdentity := managedfiles.ValidateIdentity(c.Expected) == nil
	validDiscoveryIdentity := c.Expected.BackendID != "" && c.Expected.CredentialGeneration != "" && c.Expected.SubjectFingerprint == ""
	if upstream == nil || c.AccountID <= 0 || c.Concurrency <= 0 || !bpsFilesOpaque(c.ServiceKey, 4096) || (!validIdentity && !validDiscoveryIdentity) {
		return nil, bpsFilesError("invalid_backend_config")
	}
	normalized, err := validateOutboundURLWithConfig(cfg, c.ControlURL)
	if err != nil {
		return nil, bpsFilesError("invalid_backend_url")
	}
	u, err := url.Parse(normalized)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) {
		return nil, bpsFilesError("invalid_backend_url")
	}
	return &BPSFilesBackend{config: c, upstream: upstream, baseURL: strings.TrimRight(normalized, "/")}, nil
}
func bpsFilesOpaque(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func (b *BPSFilesBackend) request(ctx context.Context, method, operation string, body io.Reader, contentType string) (*http.Request, error) {
	if err := ctx.Err(); err != nil {
		return nil, bpsFilesError("request_cancelled")
	}
	req, err := http.NewRequestWithContext(WithHTTPUpstreamRedirectsDisabled(ctx), method, b.baseURL+managedfiles.Prefix+operation, body)
	if err != nil {
		return nil, bpsFilesError("invalid_request")
	}
	req.GetBody = nil
	req.Header.Set(managedfiles.ServiceKeyHeader, b.config.ServiceKey)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}
func (b *BPSFilesBackend) execute(req *http.Request, out any, upload bool) error {
	if req.Context().Err() != nil {
		return bpsFilesError("request_cancelled")
	}
	resp, err := b.upstream.Do(req, b.config.ProxyURL, b.config.AccountID, b.config.Concurrency)
	if err != nil {
		return &BPSFilesBackendError{Code: "transport_failed", OutcomeUnknown: upload}
	}
	if resp == nil || resp.Body == nil {
		return &BPSFilesBackendError{Code: "invalid_response", OutcomeUnknown: upload}
	}
	defer resp.Body.Close()
	fail := func(code string) error {
		return &BPSFilesBackendError{Code: code, HTTPStatus: resp.StatusCode, OutcomeUnknown: upload}
	}
	if resp.StatusCode != http.StatusOK {
		return fail("backend_rejected")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bpsFilesResponseLimit+1))
	if err != nil {
		return fail("response_read_failed")
	}
	if int64(len(raw)) > bpsFilesResponseLimit {
		return fail("response_too_large")
	}
	value, err := managedfiles.Parse(raw)
	if err != nil || !bpsFilesExactShape(value, reflect.TypeOf(out).Elem()) {
		return fail("invalid_response")
	}
	if root, ok := value.(map[string]any); ok {
		if ready, present := root["ready"].(bool); present {
			_, wire := root["wire_sha256"]
			_, binding := root["binding_sha256"]
			if ready && (!wire || !binding) || !ready && (wire || binding) {
				return fail("invalid_response")
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		return fail("invalid_response")
	}
	return nil
}

// Shape validation retains exact case, required-field presence and null/type
// distinctions that encoding/json alone intentionally accepts.
func bpsFilesExactShape(value any, t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		if value == nil && t == reflect.TypeOf((*BPSPublicationFenceReceipt)(nil)) {
			return true
		}
		return bpsFilesExactShape(value, t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		fields := map[string]reflect.StructField{}
		var gather func(reflect.Type)
		gather = func(typ reflect.Type) {
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if f.Anonymous {
					gather(f.Type)
					continue
				}
				fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
			}
		}
		gather(t)
		for name, v := range object {
			f, ok := fields[name]
			if !ok || !bpsFilesExactShape(v, f.Type) {
				return false
			}
		}
		for name, f := range fields {
			if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				if _, ok := object[name]; !ok {
					return false
				}
			}
		}
		return true
	case reflect.Slice:
		values, ok := value.([]any)
		if !ok {
			return false
		}
		for _, v := range values {
			if !bpsFilesExactShape(v, t.Elem()) {
				return false
			}
		}
		return true
	case reflect.String:
		_, ok := value.(string)
		return ok
	case reflect.Bool:
		_, ok := value.(bool)
		return ok
	case reflect.Int, reflect.Int64:
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	}
	return false
}
func (b *BPSFilesBackend) Describe(ctx context.Context) (managedfiles.DescribeResult, error) {
	return b.describe(ctx, false)
}

// DescribeIdentity is used only while installing a managed binding, when the
// worker is authoritative for its subject fingerprint. Runtime clients must
// use Describe and provide the complete expected identity.
func (b *BPSFilesBackend) DescribeIdentity(ctx context.Context) (managedfiles.DescribeResult, error) {
	if b == nil || b.config.Expected.SubjectFingerprint != "" {
		return managedfiles.DescribeResult{}, bpsFilesError("invalid_backend_config")
	}
	return b.describe(ctx, true)
}

func (b *BPSFilesBackend) describe(ctx context.Context, discoverIdentity bool) (managedfiles.DescribeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result managedfiles.DescribeResult
	req, err := b.request(ctx, http.MethodGet, "describe", nil, "")
	if err != nil {
		return result, err
	}
	req.Header.Set(managedfiles.RecoveryFeatureHeader, "bps-metadata-recovery/1")
	if err = b.execute(req, &result, false); err != nil {
		return managedfiles.DescribeResult{}, err
	}
	l := result.Limits
	identityMatches := result.Identity == b.config.Expected
	if discoverIdentity {
		identityMatches = result.Identity.BackendID == b.config.Expected.BackendID && result.Identity.CredentialGeneration == b.config.Expected.CredentialGeneration && result.Identity.SubjectFingerprint != ""
	}
	if result.Contract != managedfiles.Contract || result.JSONProfile != managedfiles.JSONProfile || !identityMatches || managedfiles.ValidateIdentity(result.Identity) != nil || !result.Capabilities.BatchAdmit || !result.Capabilities.EnsureUpload || l.FileBytes <= 0 || l.FileBytes > managedfiles.MaxFileBytes || l.MessageBytes <= 0 || l.MessageBytes > managedfiles.MaxMessageBytes || l.MessagePositions <= 0 || l.MessagePositions > managedfiles.MaxMessagePositions || l.StagingBytes <= 0 || l.AdmissionBytes <= 0 || l.ConcurrentAdmissions <= 0 || l.LiveAdmissions <= 0 || l.AdmissionTTLSeconds <= 0 || l.ManifestBytes <= 0 || l.ManifestBytes > managedfiles.MaxJSONBytes || l.UniqueFiles <= 0 || l.UniqueFiles > managedfiles.MaxUniqueFiles || l.FreeSpaceReserveBytes < 0 || l.AdmitTimeoutSeconds <= 0 || l.EnsureTimeoutSeconds <= 0 {
		return managedfiles.DescribeResult{}, bpsFilesError("invalid_description")
	}
	return result, nil
}

// Admit sends only bounded framing in memory; file content remains a streaming
// reader. A fixed boundary permits an exact Content-Length without buffering.
func (b *BPSFilesBackend) Admit(ctx context.Context, m managedfiles.AdmitManifest, readers map[string]io.Reader) (managedfiles.AdmitResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var result managedfiles.AdmitResult
	if m.Expected != b.config.Expected || managedfiles.ValidateManifest(m) != nil {
		return result, bpsFilesError("invalid_manifest")
	}
	encoded, err := json.Marshal(m)
	if err != nil || len(encoded) > managedfiles.MaxJSONBytes {
		return result, bpsFilesError("invalid_manifest")
	}
	templateHash, err := managedfiles.TemplateHash(m.RequestTemplate)
	if err != nil {
		return result, bpsFilesError("invalid_manifest")
	}
	var frame bytes.Buffer
	writer := multipart.NewWriter(&frame)
	parts := []io.Reader{}
	var length, total int64
	appendFrame := func() {
		raw := append([]byte(nil), frame.Bytes()...)
		frame.Reset()
		parts = append(parts, bytes.NewReader(raw))
		length += int64(len(raw))
	}
	manifest, err := writer.CreateFormField("manifest")
	if err != nil {
		return result, bpsFilesError("invalid_manifest")
	}
	_, _ = manifest.Write(encoded)
	expectedReaders := 0
	for _, file := range m.Files {
		if file.Mode != "upload" {
			continue
		}
		expectedReaders++
		reader := readers[file.FileRef]
		if reader == nil {
			return result, bpsFilesError("missing_file_reader")
		}
		total += file.Bytes
		if total > int64(managedfiles.MaxUniqueFiles)*managedfiles.MaxFileBytes {
			return result, bpsFilesError("admission_too_large")
		}
		headers := make(textproto.MIMEHeader)
		headers.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, bpsFilesEscape(file.Part), bpsFilesEscape(file.Filename)))
		headers.Set("Content-Type", file.MIME)
		if _, err = writer.CreatePart(headers); err != nil {
			return result, bpsFilesError("invalid_manifest")
		}
		appendFrame()
		parts = append(parts, &bpsFilesLengthReader{reader: reader, remaining: file.Bytes})
		length += file.Bytes
	}
	if len(readers) != expectedReaders {
		return result, bpsFilesError("unexpected_file_reader")
	}
	if err = writer.Close(); err != nil {
		return result, bpsFilesError("invalid_manifest")
	}
	appendFrame()
	req, err := b.request(ctx, http.MethodPost, "admit", io.MultiReader(parts...), writer.FormDataContentType())
	if err != nil {
		return result, err
	}
	req.ContentLength = length
	if err = b.execute(req, &result, false); err != nil {
		return managedfiles.AdmitResult{}, err
	}
	if result.Contract != managedfiles.Contract || result.JSONProfile != managedfiles.JSONProfile || result.Identity != m.Expected || !bpsFilesOpaque(result.AdmissionID, 256) || result.ExpiresAt <= time.Now().Unix() || result.TemplateSHA256 != templateHash || len(result.Files) != len(m.Files) {
		return managedfiles.AdmitResult{}, bpsFilesError("invalid_admission")
	}
	states := make(map[string]string, len(m.Files))
	allExisting := true
	for _, file := range m.Files {
		state := "known"
		if file.Mode == "upload" {
			state = "staged"
			allExisting = false
		}
		states[file.FileRef] = state
	}
	for _, file := range result.Files {
		expected, ok := states[file.FileRef]
		if !ok || expected != file.State {
			return managedfiles.AdmitResult{}, bpsFilesError("invalid_admission")
		}
		delete(states, file.FileRef)
	}
	if result.Ready != allExisting {
		return managedfiles.AdmitResult{}, bpsFilesError("invalid_admission")
	}
	if allExisting {
		bound, err := managedfiles.Bind(m, nil)
		if err != nil || result.WireSHA256 != bound.WireSHA256 || result.BindingSHA256 != bound.BindingSHA256 {
			return managedfiles.AdmitResult{}, bpsFilesError("invalid_admission")
		}
	} else if result.WireSHA256 != "" || result.BindingSHA256 != "" {
		return managedfiles.AdmitResult{}, bpsFilesError("invalid_admission")
	}
	return result, nil
}
func bpsFilesEscape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s)
}

type bpsFilesLengthReader struct {
	reader    io.Reader
	remaining int64
}

func (r *bpsFilesLengthReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (b *BPSFilesBackend) EnsureUploaded(ctx context.Context, request managedfiles.EnsureRequest, file managedfiles.File) (managedfiles.EnsureResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	var result managedfiles.EnsureResult
	if request.Contract != managedfiles.Contract || request.Expected != b.config.Expected || !bpsFilesOpaque(request.AdmissionID, 256) || !bpsFilesOpaque(request.OperationID, 256) || request.FileRef != file.FileRef || file.Mode != "upload" || file.Bytes <= 0 || file.Bytes > managedfiles.MaxFileBytes || !bpsFilesDigest(file.SHA256) || validateFileMetadata(file.Filename, file.MIME) != nil {
		return result, bpsFilesError("invalid_ensure_request")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return result, bpsFilesError("invalid_ensure_request")
	}
	req, err := b.request(ctx, http.MethodPost, "ensure-uploaded", bytes.NewReader(raw), "application/json")
	if err != nil {
		return result, err
	}
	if err = b.execute(req, &result, true); err != nil {
		return managedfiles.EnsureResult{}, err
	}
	if result.Contract != managedfiles.Contract || result.Identity != request.Expected || result.OperationID != request.OperationID || result.FileRef != file.FileRef || !managedfiles.ValidRemoteID(result.RemoteID) || result.Bytes != file.Bytes || result.SHA256 != file.SHA256 || result.Filename != file.Filename || result.MIME != file.MIME || result.InputTokens < 0 || result.Ready && (!bpsFilesDigest(result.WireSHA256) || !bpsFilesDigest(result.BindingSHA256)) || !result.Ready && (result.WireSHA256 != "" || result.BindingSHA256 != "") {
		return managedfiles.EnsureResult{}, &BPSFilesBackendError{Code: "invalid_upload_result", OutcomeUnknown: true}
	}
	return result, nil
}
func bpsFilesDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
