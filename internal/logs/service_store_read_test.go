// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package logs

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// legacyStoreRead is ServiceStore.Read as it was before it learned to read
// backwards: scan every file, oldest first, keep everything, cut the tail.
func legacyStoreRead(s *ServiceStore, serverName, serviceName, search string, tailLines int) (proto.LogReadResult, error) {
	basePath := s.basePath(serverName, serviceName)
	paths := s.readPaths(basePath)
	search = strings.ToLower(strings.TrimSpace(search))
	lines := make([]proto.LogLine, 0, 128)
	lineNumber := 0
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return proto.LogReadResult{}, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			if search != "" && !strings.Contains(strings.ToLower(line), search) {
				continue
			}
			lines = append(lines, proto.LogLine{Number: lineNumber, Text: line})
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			return proto.LogReadResult{}, err
		}
	}
	result := proto.LogReadResult{Path: basePath, Lines: lines}
	if tailLines <= 0 {
		tailLines = 200
	}
	if len(result.Lines) > tailLines {
		result.Truncated = true
		result.Lines = append([]proto.LogLine(nil), result.Lines[len(result.Lines)-tailLines:]...)
	}
	return result, nil
}

var storeWords = []string{"GET /health 200", "ERROR upstream timeout", "error: retry", "", "\r", "İ", "\u212Aelvin",
	"bad \xff utf8", "warn slow", "Straße", "x"}

func randomStoreFile(rng *rand.Rand) string {
	var b strings.Builder
	for n := rng.IntN(60); n > 0; n-- {
		b.WriteString(storeWords[rng.IntN(len(storeWords))])
		if rng.IntN(5) == 0 {
			b.WriteString(strings.Repeat("z", rng.IntN(80)))
		}
		if rng.IntN(7) == 0 {
			b.WriteString("\r")
		}
		b.WriteString("\n")
	}
	if rng.IntN(4) == 0 {
		b.WriteString("no newline")
	}
	return b.String()
}

func compareStoreRead(t *testing.T, store *ServiceStore, search string, tail int, ctx string) {
	t.Helper()
	want, werr := legacyStoreRead(store, "web-01", "nginx.service", search, tail)
	got, gerr := store.Read("web-01", "nginx.service", search, tail)
	if (werr != nil) != (gerr != nil) {
		t.Fatalf("%s: error mismatch: legacy %v, new %v", ctx, werr, gerr)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s search=%q tail=%d:\n got %+v\nwant %+v", ctx, search, tail, got, want)
	}
}

// TestServiceStoreReadMatchesLegacyOnRandomRotatedSets lays out random current
// + rotated files (with gaps, empty and unterminated files) and checks the
// backwards reader returns exactly what the full scan did.
func TestServiceStoreReadMatchesLegacyOnRandomRotatedSets(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	searches := []string{"", "error", " ERROR ", "i", "k", "200", "\r", "zzzz", "nothing-matches"}
	for iter := 0; iter < 200; iter++ {
		root := t.TempDir()
		store := NewServiceStore(root, 1<<20, 5, time.Hour)
		base := store.basePath("web-01", "nginx.service")
		if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
			t.Fatal(err)
		}
		if rng.IntN(5) != 0 {
			writeFile(t, base, randomStoreFile(rng))
		}
		for idx := 1; idx <= 6; idx++ {
			if rng.IntN(3) != 0 {
				writeFile(t, base+"."+strconv.Itoa(idx), randomStoreFile(rng))
			}
		}
		// Names readPaths must ignore.
		writeFile(t, base+".bak", "ignored\n")
		for _, search := range searches {
			for _, tail := range []int{0, 1, 2, 12, 50, 1000} {
				compareStoreRead(t, store, search, tail, fmt.Sprintf("iter %d", iter))
			}
		}
	}
}

// TestServiceStoreReadMatchesLegacyAcrossAppendsAndRotation drives the store
// through its own Append/rotation so the line-count cache sees files grow,
// rotate and expire between reads.
func TestServiceStoreReadMatchesLegacyAcrossAppendsAndRotation(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 14))
	store := NewServiceStore(t.TempDir(), 2048, 3, time.Hour)
	number := 0
	for step := 0; step < 400; step++ {
		batch := make([]proto.LogLine, 1+rng.IntN(20))
		for i := range batch {
			number++
			batch[i] = proto.LogLine{Number: number, Text: storeWords[rng.IntN(len(storeWords))] + strconv.Itoa(number)}
		}
		if rng.IntN(40) == 0 {
			number = 0 // remote log truncated: numbering restarts
		}
		if err := store.Append("web-01", "nginx.service", batch); err != nil {
			t.Fatal(err)
		}
		for _, search := range []string{"", "error", "7"} {
			for _, tail := range []int{1, 12, 200} {
				compareStoreRead(t, store, search, tail, fmt.Sprintf("step %d", step))
			}
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
