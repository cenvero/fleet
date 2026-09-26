// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func chunkSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestChunkListDigestRequiresAnExactTiling(t *testing.T) {
	t.Parallel()
	good := []FileRangeChecksum{
		{Offset: 0, Length: 4, SHA256: chunkSum("abcd")},
		{Offset: 4, Length: 2, SHA256: chunkSum("ef")},
	}
	d1, err := ChunkListDigest(6, good)
	if err != nil {
		t.Fatalf("ChunkListDigest: %v", err)
	}
	if d2, _ := ChunkListDigest(6, good); d2 != d1 {
		t.Fatal("digest is not deterministic")
	}
	// It is never the plain content digest.
	if d1 == chunkSum("abcdef") {
		t.Fatal("chunk-list digest collides with the content digest")
	}
	// Any change to a checksum, a boundary or the size changes the digest.
	changed := []FileRangeChecksum{{Offset: 0, Length: 4, SHA256: chunkSum("abcd")}, {Offset: 4, Length: 2, SHA256: chunkSum("eF")}}
	if d, err := ChunkListDigest(6, changed); err != nil || d == d1 {
		t.Fatalf("changed chunk gave %s, %v", d, err)
	}
	resplit := []FileRangeChecksum{{Offset: 0, Length: 3, SHA256: chunkSum("abc")}, {Offset: 3, Length: 3, SHA256: chunkSum("def")}}
	if d, err := ChunkListDigest(6, resplit); err != nil || d == d1 {
		t.Fatalf("re-split chunks gave %s, %v", d, err)
	}
	for name, bad := range map[string][]FileRangeChecksum{
		"gap":      {{Offset: 0, Length: 3, SHA256: chunkSum("abc")}, {Offset: 4, Length: 2, SHA256: chunkSum("ef")}},
		"overlap":  {{Offset: 0, Length: 4, SHA256: chunkSum("abcd")}, {Offset: 3, Length: 3, SHA256: chunkSum("def")}},
		"short":    {{Offset: 0, Length: 4, SHA256: chunkSum("abcd")}},
		"empty":    {{Offset: 0, Length: 0, SHA256: chunkSum("")}, {Offset: 0, Length: 6, SHA256: chunkSum("abcdef")}},
		"too long": {{Offset: 0, Length: 7, SHA256: chunkSum("abcdefg")}},
		"bad hex":  {{Offset: 0, Length: 6, SHA256: "zz"}},
	} {
		if _, err := ChunkListDigest(6, bad); err == nil {
			t.Errorf("%s: accepted an invalid chunk list", name)
		}
	}
	if _, err := ChunkListDigest(0, nil); err != nil {
		t.Fatalf("empty file: %v", err)
	}
	if _, err := ChunkListDigest(-1, nil); err == nil {
		t.Fatal("negative size accepted")
	}
}
