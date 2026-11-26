//go:build linux

package server

import (
	"errors"
	"strconv"
	"strings"
)

var errInvalidRange = errors.New("invalid range")

type byteRange struct {
	start int64
	end   int64 // inclusive
}

func (r byteRange) length() int64 {
	return r.end - r.start + 1
}

// parseByteRange parses a single HTTP byte range against an object of total size.
func parseByteRange(header string, size int64) (byteRange, error) {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok || spec == "" || strings.Contains(spec, ",") {
		return byteRange{}, errInvalidRange
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return byteRange{}, errInvalidRange
	}

	// Suffix range: bytes=-N
	if startStr == "" {
		suffix, err := parseNonNegInt(endStr)
		if err != nil || suffix <= 0 || size == 0 {
			return byteRange{}, errInvalidRange
		}
		if suffix >= size {
			return byteRange{start: 0, end: size - 1}, nil
		}
		return byteRange{start: size - suffix, end: size - 1}, nil
	}

	start, err := parseNonNegInt(startStr)
	if err != nil || start >= size {
		return byteRange{}, errInvalidRange
	}

	// Open-ended range: bytes=N-
	if endStr == "" {
		return byteRange{start: start, end: size - 1}, nil
	}

	end, err := parseNonNegInt(endStr)
	if err != nil || end < start {
		return byteRange{}, errInvalidRange
	}
	if end >= size {
		end = size - 1
	}
	return byteRange{start: start, end: end}, nil
}

func parseNonNegInt(s string) (int64, error) {
	if s == "" {
		return 0, errInvalidRange
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errInvalidRange
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, errInvalidRange
	}
	return v, nil
}
