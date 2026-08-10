// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"
)

func defaultsEnabledStore(t *testing.T) *RedactStore {
	t.Helper()
	s, err := NewRedactStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewRedactStore: %v", err)
	}
	if err := s.SetDefaults(true); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	return s
}

// The regression this guards: the default pattern used to match only the
// `-----BEGIN ... KEY-----` header. Go's regexp is line-oriented, so the base64
// body and END line passed through verbatim while the output *looked* redacted.
func TestRedactMasksEntirePrivateKeyBlock(t *testing.T) {
	s := defaultsEnabledStore(t)

	const body = "MIIEowIBAAKCAQEAxSecretKeyMaterialThatMustNeverBeEmitted"
	input := "before\n" +
		"-----BEGIN RSA PRIVATE KEY-----\n" +
		body + "\n" +
		"AnotherLineOfKeyMaterial\n" +
		"-----END RSA PRIVATE KEY-----\n" +
		"after\n"

	got := s.Redact(input)

	if strings.Contains(got, body) {
		t.Errorf("private key body leaked through redaction:\n%s", got)
	}
	if strings.Contains(got, "AnotherLineOfKeyMaterial") {
		t.Errorf("private key body leaked through redaction:\n%s", got)
	}
	if strings.Contains(got, "-----END RSA PRIVATE KEY-----") {
		t.Errorf("END line leaked through redaction:\n%s", got)
	}
	if !strings.Contains(got, RedactPlaceholder) {
		t.Errorf("expected placeholder in output, got:\n%s", got)
	}
	// Surrounding output must survive — redaction should not swallow the stream.
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("redaction removed surrounding output:\n%s", got)
	}
}

// A key with no END line must still have its header masked; the block pattern
// cannot match, so the header-only fallback has to catch it.
func TestRedactMasksTruncatedPrivateKeyHeader(t *testing.T) {
	s := defaultsEnabledStore(t)
	got := s.Redact("-----BEGIN OPENSSH PRIVATE KEY-----\ntruncated")
	if strings.Contains(got, "BEGIN OPENSSH PRIVATE KEY") {
		t.Errorf("truncated key header not redacted: %q", got)
	}
}

// Two concatenated keys must be redacted as two blocks, not merged into one
// span that swallows whatever sits between them.
func TestRedactHandlesTwoKeysNonGreedily(t *testing.T) {
	s := defaultsEnabledStore(t)
	input := "-----BEGIN EC PRIVATE KEY-----\naaa\n-----END EC PRIVATE KEY-----\n" +
		"MIDDLE-MARKER\n" +
		"-----BEGIN EC PRIVATE KEY-----\nbbb\n-----END EC PRIVATE KEY-----\n"
	got := s.Redact(input)
	if !strings.Contains(got, "MIDDLE-MARKER") {
		t.Errorf("non-greedy match failed; text between keys was swallowed:\n%s", got)
	}
	if strings.Contains(got, "aaa") || strings.Contains(got, "bbb") {
		t.Errorf("key material leaked:\n%s", got)
	}
}

func TestRedactDefaultCredentialShapes(t *testing.T) {
	s := defaultsEnabledStore(t)
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"aws access key id", "key=AKIAIOSFODNN7EXAMPLE done", "AKIAIOSFODNN7EXAMPLE"},
		{"jwt", "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk done", "eyJhbGciOiJIUzI1NiJ9"},
		{"url credentials", "https://admin:hunter2@example.com/path", "hunter2"},
		{"bare bearer token", "Bearer abcdef0123456789abcdef", "abcdef0123456789abcdef"},
		{"password assignment", "password: supersecretvalue", "supersecretvalue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Redact(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Errorf("%s leaked: %q", tc.name, got)
			}
		})
	}
}

// Redaction must be off unless explicitly enabled, and must not mangle ordinary
// output when it is on.
func TestRedactDefaultsOffAndNoFalsePositives(t *testing.T) {
	off, err := NewRedactStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewRedactStore: %v", err)
	}
	const plain = "password: hunter2"
	if got := off.Redact(plain); got != plain {
		t.Errorf("defaults should be off until enabled, got %q", got)
	}

	s := defaultsEnabledStore(t)
	ordinary := "total 24\ndrwxr-xr-x 2 root root 4096 Jan  1 00:00 etc\nhttps://example.com/path?a=1\nCPU 12%"
	if got := s.Redact(ordinary); got != ordinary {
		t.Errorf("ordinary output was mangled:\ngot:  %q\nwant: %q", got, ordinary)
	}
}
