package service

import (
	"errors"
	"fmt"
	"strconv"
)

var ErrInvalidFileRetention = errors.New("invalid file retention")

type FileExpiryInput struct {
	Anchor     string
	Seconds    string
	AnchorSet  bool
	SecondsSet bool
}

func (i *FileExpiryInput) Set(name, value string) error {
	if len(value) >= 128 {
		return fmt.Errorf("%w: expires_after field must be shorter than 128 bytes", ErrInvalidFileRetention)
	}
	switch name {
	case "expires_after[anchor]":
		if i.AnchorSet {
			return fmt.Errorf("%w: duplicate expires_after anchor", ErrInvalidFileRetention)
		}
		i.Anchor, i.AnchorSet = value, true
	case "expires_after[seconds]":
		if i.SecondsSet {
			return fmt.Errorf("%w: duplicate expires_after seconds", ErrInvalidFileRetention)
		}
		i.Seconds, i.SecondsSet = value, true
	default:
		return fmt.Errorf("%w: unknown expires_after field", ErrInvalidFileRetention)
	}
	return nil
}

func (i FileExpiryInput) RetentionSeconds(purpose string, defaultSeconds int64) (*int64, error) {
	if defaultSeconds != 0 && (defaultSeconds < 3600 || defaultSeconds > 2592000) {
		return nil, fmt.Errorf("%w: configured default must be 0 or between 3600 and 2592000", ErrInvalidFileRetention)
	}
	if len(i.Anchor) >= 128 || len(i.Seconds) >= 128 {
		return nil, fmt.Errorf("%w: expires_after field must be shorter than 128 bytes", ErrInvalidFileRetention)
	}
	if !i.AnchorSet && !i.SecondsSet {
		if i.Anchor != "" || i.Seconds != "" {
			return nil, fmt.Errorf("%w: expiry values require presence markers", ErrInvalidFileRetention)
		}
		seconds := defaultSeconds
		if purpose == "batch" {
			seconds = 2592000
		}
		if seconds == 0 {
			return nil, nil
		}
		return &seconds, nil
	}
	if !i.AnchorSet || !i.SecondsSet {
		return nil, fmt.Errorf("%w: anchor and seconds must be provided together", ErrInvalidFileRetention)
	}
	if i.Anchor != "created_at" {
		return nil, fmt.Errorf("%w: anchor must be created_at", ErrInvalidFileRetention)
	}
	for _, digit := range i.Seconds {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%w: seconds must be an ASCII decimal integer", ErrInvalidFileRetention)
		}
	}
	seconds, err := strconv.ParseInt(i.Seconds, 10, 64)
	if err != nil || seconds < 3600 || seconds > 2592000 {
		return nil, fmt.Errorf("%w: seconds must be between 3600 and 2592000", ErrInvalidFileRetention)
	}
	return &seconds, nil
}
