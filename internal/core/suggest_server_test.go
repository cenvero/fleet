// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"strings"
	"testing"

	"github.com/cenvero/fleet/internal/transport"
)

// suggestServerReference is the original suggestion rule: the first server
// with the smallest edit distance, if that distance is at most 3.
func suggestServerReference(names []string, name string) string {
	best := ""
	bestDist := len(name) + 1
	for _, candidate := range names {
		if d := editDistance(name, candidate); d < bestDist && d <= 3 {
			bestDist = d
			best = candidate
		}
	}
	return best
}

// TestSuggestServerSkipsLengthMismatches pins that skipping names whose length
// differs by more than 3 never changes a suggestion, and that an arbitrarily
// long unknown name is answered without building its edit-distance table.
func TestSuggestServerSkipsLengthMismatches(t *testing.T) {
	t.Parallel()
	app := newFanoutTestApp(t, 0)
	names := []string{"db", "db-primary", "web-01", "web-02", "worker-long-name-1"}
	for _, n := range names {
		if err := app.AddServer(ServerRecord{Name: n, Address: "127.0.0.1", Mode: transport.ModeReverse}); err != nil {
			t.Fatal(err)
		}
	}
	servers, err := app.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	listed := make([]string, 0, len(servers))
	for _, s := range servers {
		listed = append(listed, s.Name)
	}
	for _, query := range []string{
		"", "d", "db", "dbx", "db-prim", "db-primry", "db-primary-x", "db-primary-xyz",
		"web", "web-1", "web-0", "web-011", "web-01-long", "wrker-long-name-1",
		"worker-long-name-1234", "worker-long-name-12345", "zzzzzzzz",
	} {
		want := suggestServerReference(listed, query)
		if got := app.suggestServer(query); got != want {
			t.Errorf("suggestServer(%q) = %q, want %q", query, got, want)
		}
	}
	if got := app.suggestServer(strings.Repeat("w", 1<<20)); got != "" {
		t.Fatalf("suggestServer(1 MiB name) = %q, want no suggestion", got)
	}
}
