package managedfiles

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// Parse rejects ambiguous JSON before object construction and retains numeric
// lexemes. Its return tree contains only maps, slices, Number and JSON scalars.
func Parse(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > MaxJSONBytes || !utf8.Valid(raw) || !validUnicodeEscapes(raw) {
		return nil, reject("invalid_managed_json", "body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, reject("invalid_managed_json", "body")
	}
	return v, nil
}

// encoding/json replaces lone surrogate escapes with U+FFFD. Reject them
// before decoding so different invalid inputs cannot acquire the same digest.
func validUnicodeEscapes(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] != '\\' {
				i++
				continue
			}
			if i+1 >= len(raw) {
				return false
			}
			if raw[i+1] != 'u' {
				i += 2
				continue
			}
			unit, ok := escapedUnit(raw, i)
			if !ok {
				return false
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				low, ok := escapedUnit(raw, i+6)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 12
				continue
			}
			if unit >= 0xdc00 && unit <= 0xdfff {
				return false
			}
			i += 6
		}
		if i >= len(raw) {
			return false
		}
	}
	return true
}

func escapedUnit(raw []byte, at int) (uint16, bool) {
	if at+6 > len(raw) || raw[at] != '\\' || raw[at+1] != 'u' {
		return 0, false
	}
	var n uint16
	for _, b := range raw[at+2 : at+6] {
		n <<= 4
		switch {
		case b >= '0' && b <= '9':
			n += uint16(b - '0')
		case b >= 'a' && b <= 'f':
			n += uint16(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			n += uint16(b - 'A' + 10)
		default:
			return 0, false
		}
	}
	return n, true
}

func parseValue(dec *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, reject("managed_json_limit", "body")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, reject("invalid_managed_json", "body")
	}
	if delim, ok := token.(json.Delim); ok {
		if depth >= MaxJSONDepth {
			return nil, reject("managed_json_limit", "body")
		}
		switch delim {
		case '{':
			object := make(map[string]any)
			for dec.More() {
				t, err := dec.Token()
				key, ok := t.(string)
				if err != nil || !ok {
					return nil, reject("invalid_managed_json", "body")
				}
				if _, exists := object[key]; exists {
					return nil, reject("duplicate_json_key", "body")
				}
				value, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				return nil, reject("invalid_managed_json", "body")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for dec.More() {
				value, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				return nil, reject("invalid_managed_json", "body")
			}
			return array, nil
		default:
			return nil, reject("invalid_managed_json", "body")
		}
	}
	switch token.(type) {
	case nil, bool, string, json.Number:
		return token, nil
	default:
		return nil, reject("invalid_managed_json", "body")
	}
}

// Canonical implements bps-files-json/1, deliberately preserving number
// spelling. It is not RFC 8785 and never routes numbers through float64.
func Canonical(raw []byte) ([]byte, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	result, err := json.Marshal(v)
	if err != nil {
		return nil, reject("invalid_managed_json", "body")
	}
	return result, nil
}
