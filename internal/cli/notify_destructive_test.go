// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cenvero/fleet/internal/core"
)

func TestIsDestructiveOperation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		top, sub string
		args     []string
		want     bool
	}{
		{"server", "remove", []string{"web"}, true},
		{"file", "rm", []string{"web", "/x"}, true},
		{"secret", "rotate", []string{"k"}, true},
		{"key", "rotate", nil, true},
		{"sync", "", []string{"web", "a", "b"}, true},
		{"tag", "", []string{"web", "role=db"}, true},
		{"tag", "", []string{"web"}, false},
		{"exec", "", []string{"web", "rm -rf /x"}, false}, // exec never fires
		{"dashboard", "", nil, false},
		{"daemon", "", nil, false},
		{"server", "list", nil, false},
		{"key", "fingerprint", nil, false},
		{"config", "show", nil, false},
		{"file", "list", []string{"web"}, false},
	}
	for _, c := range cases {
		if got := core.IsDestructiveOperation(c.top, c.sub, c.args); got != c.want {
			t.Errorf("%s %s %v: destructive = %v, want %v", c.top, c.sub, c.args, got, c.want)
		}
	}
}

// TestDestructiveNotificationFires: the "destructive" event was advertised by
// `fleet notify` but never fired.
func TestDestructiveNotificationFires(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.String())
		mu.Unlock()
	}))
	defer srv.Close()

	dir, _ := setupExecFanout(t, map[string]fakeExecBehavior{"web": {}}, nil)
	if err := core.NewNotifyStore(dir).Add(core.NotifyTarget{Kind: core.NotifyKindWebhook, URL: srv.URL, Events: []string{core.NotifyEventDestructive}, AllowInternal: true}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"exec", "web", "--", "true"},
		{"server", "list"},
		{"tag", "web"},
	} {
		if r := runExecFleet(t, dir, args...); r.err != nil {
			t.Fatalf("%v: %v", args, r.err)
		}
	}
	mu.Lock()
	if len(bodies) != 0 {
		t.Fatalf("non-destructive commands fired notifications: %v", bodies)
	}
	mu.Unlock()

	if r := runExecFleet(t, dir, "tag", "web", "role=db"); r.err != nil {
		t.Fatal(r.err)
	}
	if r := runExecFleet(t, dir, "secret", "set", "tok", "--value", "sup3r-s3cret"); r.err != nil {
		t.Fatal(r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("want 2 destructive notifications, got %d: %v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[0], `"event":"destructive"`) || !strings.Contains(bodies[0], "fleet tag on web by ") {
		t.Fatalf("unexpected notification: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], "fleet secret set") || strings.Contains(bodies[1], "sup3r-s3cret") || strings.Contains(bodies[1], "tok") {
		t.Fatalf("secret notification must not carry its arguments: %s", bodies[1])
	}
}
