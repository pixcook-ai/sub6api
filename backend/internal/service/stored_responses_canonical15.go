package service

import (
	"encoding/json"
	"strconv"
	"strings"
)

// CanonicalStoredResponseJSON15 is shared by persistence hashing and recovery
// digests. It keeps the repository's existing bounded JSONB numeric semantics.
func CanonicalStoredResponseJSON15(raw []byte) ([]byte, error) {
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return nil, err
	}
	var normalize func(any) (any, error)
	normalize = func(v any) (any, error) {
		switch item := v.(type) {
		case json.Number:
			number, err := normalizeStoredResponseNumber15(string(item))
			if err != nil {
				return nil, err
			}
			return json.Number(number), nil
		case map[string]any:
			for key, child := range item {
				checked, err := normalize(child)
				if err != nil {
					return nil, err
				}
				item[key] = checked
			}
			return item, nil
		case []any:
			for index, child := range item {
				checked, err := normalize(child)
				if err != nil {
					return nil, err
				}
				item[index] = checked
			}
			return item, nil
		default:
			return v, nil
		}
	}
	value, err = normalize(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func normalizeStoredResponseNumber15(raw string) (string, error) {
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(raw, "-")
	mantissa, exponent := raw, 0
	if at := strings.IndexAny(raw, "eE"); at >= 0 {
		mantissa = raw[:at]
		n, err := strconv.Atoi(raw[at+1:])
		if err != nil || n < -128 || n > 128 {
			return "", ErrStoredResponseInvalid
		}
		exponent = n
	}
	if len(mantissa) > 128 {
		return "", ErrStoredResponseInvalid
	}
	fraction := 0
	if at := strings.IndexByte(mantissa, '.'); at >= 0 {
		fraction = len(mantissa) - at - 1
		mantissa = mantissa[:at] + mantissa[at+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return "0", nil
	}
	scale := fraction - exponent
	var value string
	if scale <= 0 {
		value = digits + strings.Repeat("0", -scale)
	} else if scale >= len(digits) {
		value = "0." + strings.Repeat("0", scale-len(digits)) + digits
	} else {
		value = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if strings.Contains(value, ".") {
		value = strings.TrimRight(strings.TrimRight(value, "0"), ".")
	}
	if negative {
		value = "-" + value
	}
	return value, nil
}
