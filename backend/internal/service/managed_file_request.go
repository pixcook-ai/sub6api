package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/tidwall/gjson"
)

var ErrManagedFileUnsupportedInput = errors.New("unsupported managed file input")

type ManagedFileReference struct {
	PublicID  string
	Positions []managedfiles.Position
}

func managedPublicReferences(files []ManagedInlineFileDescriptor) []ManagedFileReference {
	byID := make(map[string][]managedfiles.Position)
	for _, file := range files {
		if file.PublicID != "" {
			byID[file.PublicID] = append(byID[file.PublicID], file.Position)
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := make([]ManagedFileReference, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, ManagedFileReference{PublicID: id, Positions: byID[id]})
	}
	return refs
}

// CollectManagedResponseFiles reads only typed Responses content. Tool
// arguments and custom input strings remain opaque, even when they contain IDs.
func CollectManagedResponseFiles(body []byte) ([]ManagedFileReference, error) {
	if !HasManagedResponseFiles(body) {
		return nil, nil
	}
	plan, err := PreflightManagedInlineFiles(context.Background(), body, DefaultManagedInlineLimits())
	if err != nil {
		return nil, fmt.Errorf("%w: invalid managed files", ErrManagedFileUnsupportedInput)
	}
	return managedPublicReferences(plan.Files()), nil
}

// HasManagedResponseFiles only discovers protocol-owned candidates; it never
// decodes payloads or interprets opaque arguments and user text.
func HasManagedResponseFiles(body []byte) bool {
	found, _ := managedResponseFileCandidate(body)
	return found
}

// Attachments cannot be retained as Responses history until their replay
// contract exists. Check original caller intent before compatibility rewrites.
func managedResponseStorageUnsupported(body []byte) bool {
	unsupported := false
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), "store") {
			unsupported = key.String() != "store" || (value.Type != gjson.False && value.Type != gjson.Null)
		}
		return !unsupported
	})
	return unsupported
}

// Walk only protocol-owned input/content/output containers, including duplicate
// and case aliases. Opaque tool arguments and user text never participate.
func managedResponseFileCandidate(body []byte) (bool, bool) {
	found, alias := false, false
	scanPart := func(part gjson.Result) {
		partFound, partAlias := managedFilePartCandidate(part, 0)
		found, alias = found || partFound, alias || partAlias
	}
	scanItem := func(item gjson.Result) {
		scanPart(item)
		item.ForEach(func(k, v gjson.Result) bool {
			if strings.EqualFold(k.String(), "content") || strings.EqualFold(k.String(), "output") {
				alias = alias || (k.String() != "content" && k.String() != "output")
				if v.IsObject() {
					scanPart(v)
				} else {
					v.ForEach(func(_, p gjson.Result) bool { scanPart(p); return !found })
				}
			}
			return !found
		})
	}
	gjson.ParseBytes(body).ForEach(func(k, v gjson.Result) bool {
		if strings.EqualFold(k.String(), "input") {
			alias = alias || k.String() != "input"
			if v.IsObject() {
				scanItem(v)
			} else {
				v.ForEach(func(_, item gjson.Result) bool { scanItem(item); return !found })
			}
		}
		return !found
	})
	return found, alias
}

// Only typed tool-result content is traversed. Tool arguments and arbitrary
// objects remain opaque; duplicate type keys are all considered before Parse
// rejects ambiguous JSON.
func managedFilePartCandidate(part gjson.Result, depth int) (found, alias bool) {
	if !part.IsObject() {
		return false, false
	}
	if depth > 64 {
		return true, true
	}
	fileKind, imageKind, typed, hasID := false, false, false, false
	part.ForEach(func(k, v gjson.Result) bool {
		switch {
		case strings.EqualFold(k.String(), "type"):
			typed = true
			fileKind = fileKind || strings.EqualFold(v.String(), "input_file")
			imageKind = imageKind || strings.EqualFold(v.String(), "input_image")
			alias = alias || k.String() != "type"
		case strings.EqualFold(k.String(), "file_id"):
			hasID = true
			alias = alias || k.String() != "file_id"
		}
		return true
	})
	if fileKind || imageKind && hasID {
		return true, alias
	}
	if typed {
		part.ForEach(func(k, v gjson.Result) bool {
			if strings.EqualFold(k.String(), "content") || strings.EqualFold(k.String(), "output") {
				alias = alias || (k.String() != "content" && k.String() != "output")
				v.ForEach(func(_, child gjson.Result) bool {
					childFound, childAlias := managedFilePartCandidate(child, depth+1)
					found, alias = found || childFound, alias || childAlias
					return true
				})
			}
			return true
		})
	}
	return found, alias
}
