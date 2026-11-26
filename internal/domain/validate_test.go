package domain

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateBucketName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		ok    bool
	}{
		{"simple", "photos", true},
		{"dots and hyphens", "my.photo-bucket1", true},
		{"minimum length", "abc", true},
		{"maximum length", strings.Repeat("a", 63), true},
		{"empty", "", false},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", 64), false},
		{"uppercase", "Photos", false},
		{"underscore", "my_bucket", false},
		{"slash", "a/b", false},
		{"leading hyphen", "-abc", false},
		{"trailing dot", "abc.", false},
		{"double dot", "a..b", false},
		{"ip address", "192.168.1.1", false},
		{"reserved prefix", "xn--abc", false},
		{"reserved suffix", "abc-s3alias", false},
		{"reserved operations path", "-", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBucketName(tt.input)
			if tt.ok && err != nil {
				t.Fatalf("%q: unexpected error: %v", tt.input, err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidBucketName) {
				t.Fatalf("%q: want ErrInvalidBucketName, got %v", tt.input, err)
			}
		})
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		ok    bool
	}{
		{"simple", "a.txt", true},
		{"nested", "logs/2026/app.log", true},
		{"path traversal text is only data", "../../etc/passwd", true},
		{"unicode", "फ़ोटो.jpg", true},
		{"maximum length", strings.Repeat("a", MaxKeyBytes), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", MaxKeyBytes+1), false},
		{"limit counts bytes", strings.Repeat("é", 513), false},
		{"invalid utf-8", "a\xff", false},
		{"null byte", "a\x00b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKey(tt.input)
			if tt.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("want ErrInvalidKey, got %v", err)
			}
		})
	}
}

func TestValidateMetadata(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]string
		want  error
	}{
		{"nil", nil, nil},
		{"valid", map[string]string{"owner": "ashu"}, nil},
		{"at limit", map[string]string{"k": strings.Repeat("v", MaxMetadataBytes-1)}, nil},
		{"over limit", map[string]string{"k": strings.Repeat("v", MaxMetadataBytes)}, ErrMetadataTooLarge},
		{"empty name", map[string]string{"": "v"}, ErrInvalidMetadata},
		{"uppercase name", map[string]string{"Owner": "v"}, ErrInvalidMetadata},
		{"space in name", map[string]string{"a b": "v"}, ErrInvalidMetadata},
		{"header injection", map[string]string{"k": "v\r\nX-Evil: 1"}, ErrInvalidMetadata},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateMetadata(tt.input); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}
}

func TestListToken(t *testing.T) {
	want := ListToken{Bucket: "photos", Prefix: "2026/", Delimiter: "/", After: "2026/a.jpg"}
	s, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodeListToken(s, "photos", "2026/", "/")
	if err != nil || got != want {
		t.Fatalf("got %+v, error %v", got, err)
	}

	bad := []struct{ name, token, bucket, prefix, delimiter string }{
		{"empty", "", "photos", "2026/", "/"},
		{"not base64", "!!!", "photos", "2026/", "/"},
		{"not json", "aGVsbG8", "photos", "2026/", "/"},
		{"too long", strings.Repeat("a", maxTokenLength+1), "photos", "2026/", "/"},
		{"other bucket", s, "videos", "2026/", "/"},
		{"other prefix", s, "photos", "2025/", "/"},
		{"other delimiter", s, "photos", "2026/", ""},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeListToken(tt.token, tt.bucket, tt.prefix, tt.delimiter)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("want ErrInvalidToken, got %v", err)
			}
		})
	}
}

func FuzzValidateBucketName(f *testing.F) {
	for _, s := range []string{"photos", "../etc", "a..b", "192.168.1.1", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidateBucketName(name) != nil {
			return
		}
		if len(name) < 3 || len(name) > 63 || strings.ContainsAny(name, "/\\\x00") || strings.Contains(name, "..") {
			t.Fatalf("accepted unsafe bucket name %q", name)
		}
	})
}

func FuzzValidateKey(f *testing.F) {
	for _, s := range []string{"a.txt", "../../etc/passwd", "a\x00b", "\xff", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if ValidateKey(key) != nil {
			return
		}
		if key == "" || len(key) > MaxKeyBytes || !utf8.ValidString(key) {
			t.Fatalf("accepted invalid key %q", key)
		}
	})
}

func FuzzDecodeListToken(f *testing.F) {
	f.Add("aGVsbG8")
	f.Add("eyJhIjoieCJ9")
	f.Fuzz(func(t *testing.T, s string) {
		tok, err := DecodeListToken(s, "", "", "")
		if err == nil && ValidateKey(tok.After) != nil {
			t.Fatalf("accepted token with invalid position %q", tok.After)
		}
	})
}
