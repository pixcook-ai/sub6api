package service

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

type ManagedInlineLimits struct {
	JSONBytes, FileBytes, MessageBytes, TotalBytes int64
	MessagePositions, UniqueFiles                  int
}

func DefaultManagedInlineLimits() ManagedInlineLimits {
	return ManagedInlineLimits{int64(managedfiles.MaxJSONBytes), managedfiles.MaxFileBytes, managedfiles.MaxMessageBytes, managedfiles.MaxMessageBytes, managedfiles.MaxMessagePositions, managedfiles.MaxUniqueFiles}
}

type ManagedInlineFileDescriptor struct {
	PublicID string
	Filename string
	MIME     string
	Bytes    int64
	Position managedfiles.Position
}

// Public metadata must come from the caller's owner- and state-scoped lookup.
// Supplying metadata is a budget operation, never proof of authorization.
type ManagedInlinePublicFile struct {
	PublicID string
	Bytes    int64
}

type managedInlineEntry struct {
	description ManagedInlineFileDescriptor
	encoded     string
	sourceURL   string
	source      managedfiles.ToolFileSource
}

// ManagedInlinePreparation holds private immutable payloads and original positions.
// A separate metadata snapshot completes mixed public/inline request budgets.
type ManagedInlinePreparation struct {
	entries []managedInlineEntry
	limits  ManagedInlineLimits
	checked bool
}

func (p *ManagedInlinePreparation) Files() []ManagedInlineFileDescriptor {
	if p == nil {
		return nil
	}
	result := make([]ManagedInlineFileDescriptor, len(p.entries))
	for i, entry := range p.entries {
		result[i] = entry.description
	}
	return result
}

func (p *ManagedInlinePreparation) ReadyForStaging() bool { return p != nil && p.checked }

func (p *ManagedInlinePreparation) WithPublicFiles(files []ManagedInlinePublicFile) (*ManagedInlinePreparation, error) {
	if p == nil {
		return nil, inlineReject("invalid_inline_plan", "input")
	}
	metadata := make(map[string]int64, len(files))
	for _, file := range files {
		if _, exists := metadata[file.PublicID]; exists {
			return nil, inlineReject("invalid_public_metadata", "input")
		}
		metadata[file.PublicID] = file.Bytes
	}
	result := &ManagedInlinePreparation{entries: append([]managedInlineEntry(nil), p.entries...), limits: p.limits}
	used := make(map[string]bool)
	for i := range result.entries {
		d := &result.entries[i].description
		if d.PublicID == "" {
			continue
		}
		size, exists := metadata[d.PublicID]
		if !exists || size <= 0 {
			return nil, inlineReject("invalid_public_metadata", "input")
		}
		d.Bytes = size
		used[d.PublicID] = true
	}
	if len(used) != len(metadata) {
		return nil, inlineReject("invalid_public_metadata", "input")
	}
	if err := validateManagedInlineBudget(result.entries, result.limits); err != nil {
		return nil, err
	}
	result.checked = true
	return result, nil
}

func PreflightManagedInlineFiles(ctx context.Context, body []byte, limits ManagedInlineLimits) (*ManagedInlinePreparation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := normalizeManagedInlineLimits(limits)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limits.JSONBytes {
		return nil, inlineReject("managed_json_limit", "body")
	}
	value, err := managedfiles.Parse(body)
	if err != nil {
		return nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, inlineReject("invalid_inline_request", "input")
	}
	if managedInlineAlias(root, "input") {
		return nil, inlineReject("invalid_inline_request", "input")
	}
	plan := &ManagedInlinePreparation{limits: limits, checked: true}
	input, _ := root["input"].([]any)
	toolSources, err := managedfiles.ValidateToolFileSources(input)
	if err != nil {
		return nil, inlineReject("unsupported_inline_container", "input")
	}
	if item, ok := root["input"].(map[string]any); ok && managedInlineInItem(item) {
		return nil, inlineReject("unsupported_inline_container", "input")
	}
	for i, rawItem := range input {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if managedInlineAlias(item, "type", "role", "content", "output", "call_id") {
			return nil, inlineReject("invalid_inline_request", "input")
		}
		kind, _ := item["type"].(string)
		_, hasType := item["type"]
		_, hasRole := item["role"].(string)
		if _, toolOutput := toolSources[i]; !toolOutput && kind != "message" && !(!hasType && hasRole) {
			if managedInlineInItem(item) {
				return nil, inlineReject("unsupported_inline_container", "input")
			}
			continue
		}
		parts, field := managedfiles.TypedFileContent(item)
		if _, ok := item[field].(map[string]any); ok && managedInlineInContainer(item[field]) {
			return nil, inlineReject("unsupported_inline_container", "input")
		}
		for j, rawPart := range parts {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if managedInlineAlias(part, "type", "file_id", "file_data", "file_url", "filename") {
				return nil, inlineReject("invalid_inline_request", "input")
			}
			if part["type"] != "input_file" {
				if managedInlineInContainer(part) {
					return nil, inlineReject("unsupported_inline_container", "input")
				}
				continue
			}
			d := ManagedInlineFileDescriptor{Position: managedfiles.Position{Input: i, Part: j}}
			entry := managedInlineEntry{source: toolSources[i]}
			_, hasID := part["file_id"]
			_, hasData := part["file_data"]
			_, hasURL := part["file_url"]
			sources := 0
			for _, present := range []bool{hasID, hasData, hasURL} {
				if present {
					sources++
				}
			}
			if sources != 1 {
				return nil, inlineReject("invalid_inline_file", "input")
			}
			if hasID {
				id, ok := part["file_id"].(string)
				if !ok || !managedfiles.ValidRemoteID(id) {
					return nil, inlineReject("invalid_public_id", "input")
				}
				d.PublicID = id
				plan.checked = false
			} else if hasURL {
				raw, ok := part["file_url"].(string)
				if !ok {
					return nil, inlineReject("invalid_file_url", "input")
				}
				target, err := parseManagedFileURL(raw)
				if err != nil {
					return nil, err
				}
				if name, exists := part["filename"]; exists {
					d.Filename, ok = name.(string)
					if !ok || !validManagedInlineFilename(d.Filename) {
						return nil, inlineReject("invalid_inline_filename", "input")
					}
				} else {
					d.Filename = path.Base(target.Path)
					if d.Filename == "." || d.Filename == "/" {
						d.Filename = "attachment"
					}
					if !validManagedInlineFilename(d.Filename) {
						return nil, inlineReject("invalid_inline_filename", "input")
					}
				}
				entry.sourceURL = raw
			} else {
				encoded, ok := part["file_data"].(string)
				if !ok {
					return nil, inlineReject("invalid_inline_file", "input")
				}
				if raw, exists := part["filename"]; exists {
					d.Filename, ok = raw.(string)
					if !ok {
						return nil, inlineReject("invalid_inline_filename", "input")
					}
				}
				if len(encoded) >= 5 && strings.EqualFold(encoded[:5], "data:") {
					header, payload, found := strings.Cut(encoded[5:], ",")
					if !found || len(header) < 7 || !strings.EqualFold(header[len(header)-7:], ";base64") {
						return nil, inlineReject("invalid_inline_base64", "input")
					}
					header = header[:len(header)-7]
					if len(header) > 1024 || strings.ContainsAny(header, "\r\n") {
						return nil, inlineReject("invalid_inline_mime", "input")
					}
					if header != "" {
						mediaType, _, err := mime.ParseMediaType(header)
						if err != nil || strings.Contains(mediaType, "*") || strings.Count(mediaType, "/") != 1 {
							return nil, inlineReject("invalid_inline_mime", "input")
						}
						d.MIME = strings.ToLower(mediaType)
					}
					encoded = payload
					if d.Filename == "" {
						d.Filename = "attachment"
					}
				}
				if !validManagedInlineFilename(d.Filename) {
					return nil, inlineReject("invalid_inline_filename", "input")
				}
				d.MIME, err = managedInlineMediaType(d.Filename, d.MIME)
				if err != nil {
					return nil, err
				}
				if strings.ContainsAny(encoded, "\r\n\t ") {
					return nil, inlineReject("invalid_inline_base64", "input")
				}
				limit := limits.FileBytes
				if managedInlineImageType(d) != "" && limit > managedInlineImageMaxBytes {
					limit = managedInlineImageMaxBytes
				}
				reader := managedInlineContextReader{ctx: ctx, reader: strings.NewReader(encoded)}
				n, err := io.Copy(io.Discard, io.LimitReader(base64.NewDecoder(base64.StdEncoding.Strict(), reader), limit+1))
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if err != nil {
					return nil, inlineReject("invalid_inline_base64", "input")
				}
				if n == 0 || n > limit {
					return nil, inlineReject("managed_file_limit", "input")
				}
				d.Bytes, entry.encoded = n, encoded
				if err := validateManagedInlineContent(ctx, d, encoded); err != nil {
					return nil, err
				}
			}
			entry.description = d
			plan.entries = append(plan.entries, entry)
		}
	}
	if err := validateManagedInlineBudget(plan.entries, limits); err != nil {
		return nil, err
	}
	return plan, nil
}

func inlineReject(code, param string) error { return &managedfiles.Error{Code: code, Param: param} }

func validManagedInlineFilename(filename string) bool {
	if filename == "" || filename == "." || filename == ".." || len(filename) > 1024 || strings.TrimSpace(filename) != filename || !utf8.ValidString(filename) || strings.ContainsAny(filename, `/\\:`) {
		return false
	}
	for _, r := range filename {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func managedInlineAlias(object map[string]any, fields ...string) bool {
	for key := range object {
		for _, field := range fields {
			if key != field && strings.EqualFold(key, field) {
				return true
			}
		}
	}
	return false
}

// Only protocol-owned typed containers are traversed. Arguments, input strings,
// text and arbitrary nested values remain opaque.
func managedInlineInContainer(value any) bool {
	if values, ok := value.([]any); ok {
		for _, child := range values {
			if managedInlineInContainer(child) {
				return true
			}
		}
		return false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	typed := false
	for key, raw := range object {
		if !strings.EqualFold(key, "type") {
			continue
		}
		typed = true
		kind, _ := raw.(string)
		if strings.EqualFold(kind, "input_file") {
			return true
		}
		if strings.EqualFold(kind, "input_image") {
			for field := range object {
				if strings.EqualFold(field, "file_id") {
					return true
				}
			}
		}
	}
	if typed {
		for key, child := range object {
			if (strings.EqualFold(key, "content") || strings.EqualFold(key, "output")) && managedInlineInContainer(child) {
				return true
			}
		}
	}
	return false
}

func managedInlineInItem(item map[string]any) bool {
	if managedInlineInContainer(item) {
		return true
	}
	for key, value := range item {
		if (strings.EqualFold(key, "content") || strings.EqualFold(key, "output")) && managedInlineInContainer(value) {
			return true
		}
	}
	return false
}

func normalizeManagedInlineLimits(limits ManagedInlineLimits) (ManagedInlineLimits, error) {
	defaults := DefaultManagedInlineLimits()
	fields := []*int64{&limits.JSONBytes, &limits.FileBytes, &limits.MessageBytes, &limits.TotalBytes}
	ceilings := []int64{defaults.JSONBytes, defaults.FileBytes, defaults.MessageBytes, defaults.TotalBytes}
	for i, value := range fields {
		if *value == 0 {
			*value = ceilings[i]
		}
		if *value < 0 || *value > ceilings[i] {
			return ManagedInlineLimits{}, inlineReject("invalid_inline_limits", "input")
		}
	}
	if limits.MessagePositions == 0 {
		limits.MessagePositions = defaults.MessagePositions
	}
	if limits.UniqueFiles == 0 {
		limits.UniqueFiles = defaults.UniqueFiles
	}
	if limits.MessagePositions < 0 || limits.MessagePositions > defaults.MessagePositions || limits.UniqueFiles < 0 || limits.UniqueFiles > defaults.UniqueFiles {
		return ManagedInlineLimits{}, inlineReject("invalid_inline_limits", "input")
	}
	return limits, nil
}

func validateManagedInlineBudget(entries []managedInlineEntry, limits ManagedInlineLimits) error {
	positions := make(map[int]int)
	public := make(map[string]bool)
	messageBytes := make(map[int]int64)
	unique := 0
	var total int64
	for _, entry := range entries {
		d := entry.description
		positions[d.Position.Input]++
		if positions[d.Position.Input] > limits.MessagePositions {
			return inlineReject("managed_message_limit", "input")
		}
		if d.PublicID == "" {
			unique++
		} else if !public[d.PublicID] {
			public[d.PublicID] = true
			unique++
		}
		if unique > limits.UniqueFiles || d.Bytes < 0 || d.Bytes > limits.FileBytes {
			return inlineReject("managed_file_limit", "input")
		}
		if d.Bytes > limits.TotalBytes-total {
			return inlineReject("managed_request_limit", "input")
		}
		total += d.Bytes
		if d.Bytes > limits.MessageBytes-messageBytes[d.Position.Input] {
			return inlineReject("managed_message_limit", "input")
		}
		messageBytes[d.Position.Input] += d.Bytes
	}
	return nil
}

type managedInlineContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r managedInlineContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
