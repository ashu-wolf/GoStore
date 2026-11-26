package domain

import (
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"
)

const (
	// MaxKeyBytes is the longest allowed object key, in bytes.
	MaxKeyBytes = 1024
	// MaxMetadataBytes limits the combined size of user metadata names and values.
	MaxMetadataBytes = 2048
)

// ValidateBucketName applies the core S3 naming rules for general purpose buckets.
func ValidateBucketName(name string) error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrInvalidBucketName, reason)
	}

	if len(name) < 3 || len(name) > 63 {
		return invalid("length must be 3 to 63 characters")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isLowerOrDigit(c) && c != '-' && c != '.' {
			return invalid("only lowercase letters, digits, hyphens and dots are allowed")
		}
	}
	if !isLowerOrDigit(name[0]) || !isLowerOrDigit(name[len(name)-1]) {
		return invalid("must start and end with a letter or digit")
	}
	if strings.Contains(name, "..") {
		return invalid("must not contain two dots in a row")
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return invalid("must not be an IP address")
	}
	for _, p := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, p) {
			return invalid("uses a reserved prefix")
		}
	}
	for _, s := range []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3"} {
		if strings.HasSuffix(name, s) {
			return invalid("uses a reserved suffix")
		}
	}
	return nil
}

func isLowerOrDigit(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// ValidateKey checks an object key. A key is data, not a file path, so values
// such as "../../etc/passwd" are valid. Storage code must never build a file
// path from a key.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: key is empty", ErrInvalidKey)
	case len(key) > MaxKeyBytes:
		return fmt.Errorf("%w: key is longer than %d bytes", ErrInvalidKey, MaxKeyBytes)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: key is not valid UTF-8", ErrInvalidKey)
	case strings.ContainsRune(key, 0):
		// A GoStore restriction: XML responses cannot represent this byte.
		return fmt.Errorf("%w: key contains a null byte", ErrInvalidKey)
	}
	return nil
}

// ValidateMetadata checks user metadata. Names exclude the "x-amz-meta-" prefix.
// Values are limited to printable ASCII in this version.
func ValidateMetadata(meta map[string]string) error {
	total := 0
	for name, value := range meta {
		if !validMetadataName(name) {
			return fmt.Errorf("%w: bad name %q", ErrInvalidMetadata, name)
		}
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] > 0x7e {
				return fmt.Errorf("%w: bad value for %q", ErrInvalidMetadata, name)
			}
		}
		total += len(name) + len(value)
	}
	if total > MaxMetadataBytes {
		return fmt.Errorf("%w: %d bytes, limit is %d", ErrMetadataTooLarge, total, MaxMetadataBytes)
	}
	return nil
}

// validMetadataName reports whether name is a lowercase HTTP header token.
func validMetadataName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isLowerOrDigit(name[i]) && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(name[i])) {
			return false
		}
	}
	return true
}
