package managedfiles_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func exampleUpload(m managedfiles.AdmitManifest) managedfiles.EnsureResult {
	f := m.Files[0]
	return managedfiles.EnsureResult{
		Contract: managedfiles.Contract, Identity: m.Expected, OperationID: "operation-a",
		FileRef: f.FileRef, RemoteID: "file-remote-a", Bytes: f.Bytes, SHA256: f.SHA256,
		Filename: f.Filename, MIME: f.MIME, InputTokens: 5,
	}
}

func TestBindRejectsUnconfirmedOrTamperedResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*managedfiles.EnsureResult)
	}{
		{"contract", func(r *managedfiles.EnsureResult) { r.Contract = "bps-files/2" }},
		{"identity backend", func(r *managedfiles.EnsureResult) { r.Identity.BackendID = "another" }},
		{"identity subject", func(r *managedfiles.EnsureResult) {
			r.Identity.SubjectFingerprint = "sha256:" + strings.Repeat("2", 64)
		}},
		{"identity generation", func(r *managedfiles.EnsureResult) { r.Identity.CredentialGeneration = "epoch-2" }},
		{"unknown file", func(r *managedfiles.EnsureResult) { r.FileRef = "file-unknown" }},
		{"invalid remote", func(r *managedfiles.EnsureResult) { r.RemoteID = "private/path" }},
		{"missing operation", func(r *managedfiles.EnsureResult) { r.OperationID = "" }},
		{"reused operation", func(r *managedfiles.EnsureResult) { r.OperationID = "operation-b" }},
		{"bytes", func(r *managedfiles.EnsureResult) { r.Bytes++ }},
		{"sha256", func(r *managedfiles.EnsureResult) { r.SHA256 = strings.Repeat("c", 64) }},
		{"filename", func(r *managedfiles.EnsureResult) { r.Filename = "other.txt" }},
		{"mime", func(r *managedfiles.EnsureResult) { r.MIME = "application/pdf" }},
		{"negative tokens", func(r *managedfiles.EnsureResult) { r.InputTokens = -1 }},
		{"wire hash", func(r *managedfiles.EnsureResult) { r.WireSHA256 = strings.Repeat("f", 64) }},
		{"binding hash", func(r *managedfiles.EnsureResult) { r.BindingSHA256 = strings.Repeat("f", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := exampleManifest()
			r := exampleUpload(m)
			tc.change(&r)
			_, err := managedfiles.Bind(m, []managedfiles.EnsureResult{r})
			var typed *managedfiles.Error
			if !errors.As(err, &typed) || typed.Code != "invalid_managed_binding" {
				t.Fatalf("accepted changed upload metadata: %v", err)
			}
		})
	}
	m := exampleManifest()
	if _, err := managedfiles.Bind(m, nil); err == nil {
		t.Fatal("accepted missing upload confirmation")
	}
	r := exampleUpload(m)
	if _, err := managedfiles.Bind(m, []managedfiles.EnsureResult{r, r}); err == nil {
		t.Fatal("accepted duplicate upload confirmation")
	}
}

func TestBindExistingBatchNeedsNoEnsureAndDoesNotBindTransportFields(t *testing.T) {
	m := exampleManifest()
	mixed, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil {
		t.Fatal(err)
	}
	m.Files[0].Mode, m.Files[0].Part = "existing", ""
	m.Files[0].RemoteID, m.Files[0].OperationID = "file-remote-a", "operation-a"
	m.RequestID = "different-request"
	allExisting, err := managedfiles.Bind(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(allExisting.Wire) != string(mixed.Wire) || allExisting.BindingSHA256 != mixed.BindingSHA256 {
		t.Fatal("transport fields changed request binding")
	}
}

func TestBindChecksOptionalFinalHashesWithoutTreatingReadyAsProof(t *testing.T) {
	m := exampleManifest()
	r := exampleUpload(m) // Ready=false is expected for an earlier Ensure response.
	bound, err := managedfiles.Bind(m, []managedfiles.EnsureResult{r})
	if err != nil {
		t.Fatal(err)
	}
	r.Ready, r.WireSHA256, r.BindingSHA256 = true, bound.WireSHA256, bound.BindingSHA256
	confirmed, err := managedfiles.Bind(m, []managedfiles.EnsureResult{r})
	if err != nil || confirmed.BindingSHA256 != bound.BindingSHA256 {
		t.Fatalf("valid final claims rejected: %v", err)
	}
	r.FileRef = "file-unknown"
	if _, err := managedfiles.Bind(m, []managedfiles.EnsureResult{r}); err == nil {
		t.Fatal("ready=true bypassed metadata validation")
	}
}

func TestBindingCoversAllGenerationControlsAndOwnerScope(t *testing.T) {
	m := exampleManifest()
	base, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ old, replacement string }{
		{`"model":"fixture"`, `"model":"another"`},
		{`9007199254740993`, `9007199254740992`},
		{`"n":9007199254740993`, `"n":null`},
		{`"n":9007199254740993`, `"n":9007199254740993.0`},
	} {
		changed := exampleManifest()
		changed.RequestTemplate = []byte(strings.Replace(string(changed.RequestTemplate), mutation.old, mutation.replacement, 1))
		bound, err := managedfiles.Bind(changed, []managedfiles.EnsureResult{exampleUpload(changed)})
		if err != nil {
			t.Fatal(err)
		}
		if bound.TemplateSHA256 == base.TemplateSHA256 || bound.WireSHA256 == base.WireSHA256 || bound.BindingSHA256 == base.BindingSHA256 {
			t.Fatalf("generation control escaped binding: %s", mutation.replacement)
		}
	}
	m.OwnerRef, m.GroupRef = "another-owner", "another-group"
	scoped, err := managedfiles.Bind(m, []managedfiles.EnsureResult{exampleUpload(m)})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.BindingSHA256 == base.BindingSHA256 || scoped.WireSHA256 != base.WireSHA256 || scoped.TemplateSHA256 != base.TemplateSHA256 {
		t.Fatal("binding did not isolate owner/group from request bytes")
	}
}
