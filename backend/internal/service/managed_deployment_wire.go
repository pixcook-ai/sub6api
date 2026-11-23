package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func scopeManagedDeploymentWireHeaders(ctx context.Context, headers http.Header) http.Header {
	if !openAIManagedDeploymentNoReplay(ctx) {
		return headers
	}
	scoped := headers.Clone()
	if scoped == nil {
		scoped = make(http.Header)
	}
	for _, key := range []string{"session-id", "session_id", "conversation_id"} {
		value, _ := ScopeManagedDeploymentAffinity(ctx, strings.TrimSpace(headers.Get(key)))
		scoped.Set(key, value)
	}
	return scoped
}

// Scope only protocol affinity fields. Native response/resource IDs and opaque
// reasoning remain unchanged, so existing turns retain their original owner.
func scopeManagedDeploymentWirePayload(ctx context.Context, raw []byte) ([]byte, error) {
	if !openAIManagedDeploymentNoReplay(ctx) {
		return raw, nil
	}
	if !managedDeploymentUniqueJSON(raw) || !gjson.ParseBytes(raw).IsObject() {
		return nil, ErrManagedDeploymentInvalid
	}
	eventType := gjson.GetBytes(raw, "type").String()
	inject := eventType == "" || eventType == "response.create"
	prefix := ""
	if eventType == "session.update" {
		prefix = "session."
	}
	metadata := gjson.GetBytes(raw, prefix+"client_metadata")
	if metadata.Exists() && metadata.Type != gjson.Null && !metadata.IsObject() {
		return nil, ErrManagedDeploymentInvalid
	}
	fields := []string{"prompt_cache_key", "client_metadata.session_id", "client_metadata.thread_id", "client_metadata.conversation_id"}
	for _, field := range fields {
		path := prefix + field
		value := gjson.GetBytes(raw, path)
		// BPS accepts prompt_cache_key but deliberately rejects the Codex-only
		// client_metadata extension. Scope existing metadata without inventing it.
		if !value.Exists() && (!inject || field != "prompt_cache_key") {
			continue
		}
		if value.Exists() && value.Type != gjson.String && value.Type != gjson.Null {
			return nil, ErrManagedDeploymentInvalid
		}
		scoped, _ := ScopeManagedDeploymentAffinity(ctx, value.String())
		var err error
		raw, err = sjson.SetBytes(raw, path, scoped)
		if err != nil {
			return nil, err
		}
	}
	return raw, nil
}

type managedDeploymentWireScopedKey struct{}

func scopeManagedDeploymentHTTPRequest(request *http.Request) (*http.Request, error) {
	if scoped, _ := request.Context().Value(managedDeploymentWireScopedKey{}).(bool); scoped {
		return request, nil
	}
	if !openAIManagedDeploymentNoReplay(request.Context()) {
		return request, nil
	}
	scoped := request.Clone(context.WithValue(request.Context(), managedDeploymentWireScopedKey{}, true))
	scoped.Header = scopeManagedDeploymentWireHeaders(request.Context(), request.Header)
	if request.Body == nil || request.Body == http.NoBody || request.URL == nil {
		return scoped, nil
	}
	path := strings.TrimRight(request.URL.Path, "/")
	if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/responses/compact") {
		return scoped, nil
	}
	source := request.Body
	if request.GetBody != nil {
		var err error
		source, err = request.GetBody()
		if err != nil {
			return nil, err
		}
	}
	raw, err := io.ReadAll(source)
	_ = source.Close()
	if err != nil {
		return nil, err
	}
	body, err := scopeManagedDeploymentWirePayload(request.Context(), raw)
	if err != nil {
		return nil, err
	}
	_ = request.Body.Close()
	scoped.Body = io.NopCloser(bytes.NewReader(body))
	scoped.ContentLength = int64(len(body))
	scoped.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return scoped, nil
}
