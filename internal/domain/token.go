package domain

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

const maxTokenLength = 16 * 1024

// ListToken records where the next page of a listing starts.
// It is tied to one query, so it cannot be reused with another.
// It is encoded, not signed or encrypted: never use it for access control.
type ListToken struct {
	Bucket    string `json:"b"`
	Prefix    string `json:"p"`
	Delimiter string `json:"d"`
	After     string `json:"a"` // last key or common prefix already returned
}

// Encode returns the token as a URL-safe string.
func (t ListToken) Encode() (string, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeListToken parses a token and checks that it belongs to this query.
func DecodeListToken(s, bucket, prefix, delimiter string) (ListToken, error) {
	if s == "" || len(s) > maxTokenLength {
		return ListToken{}, ErrInvalidToken
	}
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ListToken{}, ErrInvalidToken
	}
	var t ListToken
	if err := json.Unmarshal(data, &t); err != nil {
		return ListToken{}, ErrInvalidToken
	}
	if t.Bucket != bucket || t.Prefix != prefix || t.Delimiter != delimiter {
		return ListToken{}, ErrInvalidToken
	}
	if ValidateKey(t.After) != nil || !strings.HasPrefix(t.After, prefix) {
		return ListToken{}, ErrInvalidToken
	}
	return t, nil
}
