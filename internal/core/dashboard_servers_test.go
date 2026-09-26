// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestServerListCacheMatchesListServersAndTracksChanges(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	for i := 0; i < 5; i++ {
		if err := app.AddServer(ServerRecord{Name: fmt.Sprintf("s-%d", i), Address: "10.0.0.1",
			Services: []ServiceRecord{{Name: "a.service"}}, OpenPorts: []int{22}}); err != nil {
			t.Fatal(err)
		}
	}
	// Age the files past the racy window so they can be cached.
	ageServerFiles(t, app, 10*time.Second)

	var cache ServerListCache
	check := func(label string) []ServerRecord {
		t.Helper()
		want, err := app.ListServers()
		if err != nil {
			t.Fatal(err)
		}
		got, err := app.listServersCached(&cache)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: cached list differs from ListServers()\ngot  %+v\nwant %+v", label, got, want)
		}
		return got
	}
	check("first")
	if cache.decodes != 5 {
		t.Fatalf("first listing decoded %d files, want 5", cache.decodes)
	}
	got := check("second")
	if cache.decodes != 5 {
		t.Fatalf("unchanged files were decoded again (%d decodes)", cache.decodes)
	}
	// Callers get copies: mutating a result does not leak into the cache.
	got[0].Services[0].Name = "mutated"
	got[0].OpenPorts[0] = 1
	check("after caller mutation")

	// A saved server (rename → new file) is re-read, even within the same
	// mtime tick.
	rec, err := app.GetServer("s-2")
	if err != nil {
		t.Fatal(err)
	}
	rec.Observed.Reachable = true
	if err := app.SaveServer(rec); err != nil {
		t.Fatal(err)
	}
	check("after save")
	if cache.decodes != 6 {
		t.Fatalf("decodes after one save = %d, want 6", cache.decodes)
	}

	// Removed and added servers.
	if err := os.Remove(filepath.Join(app.ConfigDir, "servers", "s-0.toml")); err != nil {
		t.Fatal(err)
	}
	if err := app.AddServer(ServerRecord{Name: "s-9", Address: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	if list := check("after remove/add"); len(list) != 5 || list[4].Name != "s-9" {
		t.Fatalf("unexpected list %+v", list)
	}
	if _, ok := cache.entries["s-0.toml"]; ok {
		t.Fatalf("removed server still cached")
	}
}

// A file whose mtime is within the racy window of the decode is never served
// from the cache, even if size and mtime still match: on filesystems with a
// coarse mtime a same-size rewrite in the same tick would be invisible.
func TestServerListCacheRacyWindow(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	if err := app.AddServer(ServerRecord{Name: "racy", Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	var cache ServerListCache
	for i := 0; i < 3; i++ {
		if _, err := app.listServersCached(&cache); err != nil {
			t.Fatal(err)
		}
	}
	if cache.decodes != 3 {
		t.Fatalf("a just-written file must be re-decoded each time, got %d decodes", cache.decodes)
	}
	// In-place same-size rewrite with the mtime pinned: once the file is old
	// enough to be trusted, a stat change (here: mtime) still invalidates it.
	path := filepath.Join(app.ConfigDir, "servers", "racy.toml")
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := app.listServersCached(&cache); err != nil {
		t.Fatal(err)
	}
	before := cache.decodes
	if _, err := app.listServersCached(&cache); err != nil {
		t.Fatal(err)
	}
	if cache.decodes != before {
		t.Fatalf("an old, unchanged file should be served from the cache")
	}
	newer := old.Add(time.Second)
	if err := os.Chtimes(path, newer, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := app.listServersCached(&cache); err != nil {
		t.Fatal(err)
	}
	if cache.decodes != before+1 {
		t.Fatalf("an mtime change must invalidate the cached record")
	}
}

func TestServerListCacheDecodeErrorMatchesListServers(t *testing.T) {
	t.Parallel()
	app := newDashboardTestApp(t)
	if err := os.WriteFile(filepath.Join(app.ConfigDir, "servers", "bad.toml"), []byte("name = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cache ServerListCache
	if _, err := app.listServersCached(&cache); err == nil {
		t.Fatalf("expected a decode error, like ListServers")
	}
	if _, err := app.ListServers(); err == nil {
		t.Fatalf("ListServers should fail too")
	}
}

// cloneServerRecord must copy every reference-typed field; this fails when a
// new slice or map field is added to ServerRecord without updating it.
func TestCloneServerRecordCoversReferenceFields(t *testing.T) {
	t.Parallel()
	handled := map[string]bool{"Capabilities": true, "Services": true, "OpenPorts": true, "Firewall.Rules": true}
	var walk func(prefix string, typ reflect.Type)
	walk = func(prefix string, typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := prefix + f.Name
			switch f.Type.Kind() {
			case reflect.Slice, reflect.Map, reflect.Pointer:
				if !handled[name] {
					t.Errorf("ServerRecord.%s is a %s: cloneServerRecord must copy it", name, f.Type.Kind())
				}
			case reflect.Struct:
				if f.Type.PkgPath() == "time" {
					continue
				}
				walk(name+".", f.Type)
			}
		}
	}
	walk("", reflect.TypeOf(ServerRecord{}))
}

func ageServerFiles(t *testing.T, app *App, age time.Duration) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.ConfigDir, "servers"))
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(app.ConfigDir, "servers", e.Name()), when, when); err != nil {
			t.Fatal(err)
		}
	}
}
