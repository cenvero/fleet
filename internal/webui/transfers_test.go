// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

func TestProgressHubLifecycle(t *testing.T) {
	t.Parallel()
	h := newProgressHub()
	h.retention = 50 * time.Millisecond

	id, err := h.startMeta(transferMeta{Kind: "copy", Label: "a.txt", DstServer: "web-01", DstPath: "/srv/a.txt"})
	if err != nil || len(id) != 32 {
		t.Fatalf("startMeta = %q, %v", id, err)
	}
	if err := h.startWithID(id, transferMeta{}); err == nil {
		t.Fatalf("duplicate id accepted")
	}
	for _, bad := range []string{"", "xyz", "ABCDEFABCDEFABCD", strings.Repeat("a", 65), "../../../../etc/pas"} {
		if err := h.startWithID(bad, transferMeta{}); err == nil {
			t.Fatalf("bad id %q accepted", bad)
		}
	}

	items, rev := h.list(0)
	if len(items) != 1 || items[0].Kind != "copy" || items[0].DstServer != "web-01" || items[0].Cancellable {
		t.Fatalf("list = %+v", items)
	}
	if again, _ := h.list(rev); len(again) != 0 {
		t.Fatalf("unchanged items re-sent: %+v", again)
	}
	h.update(id, core.ProgressUpdate{BytesDone: 5, TotalBytes: 10, RatePerSec: 2})
	changed, _ := h.list(rev)
	if len(changed) != 1 || changed[0].Percent != 50 {
		t.Fatalf("changed = %+v", changed)
	}

	// Not cancellable (a core-driven copy) → conflict, not a silent no-op.
	if found, ok := h.requestCancel(id); !found || ok {
		t.Fatalf("requestCancel on uncancellable = %v,%v", found, ok)
	}
	cancelled := false
	h.setCancel(id, func() { cancelled = true })
	if found, ok := h.requestCancel(id); !found || !ok || !cancelled {
		t.Fatalf("requestCancel = %v,%v cancelled=%v", found, ok, cancelled)
	}
	h.finish(id, errors.New("boom"))
	snap, ok := h.snapshot(id)
	if !ok || !snap.Done || snap.Error != "boom" || !snap.Cancelled || snap.Cancellable {
		t.Fatalf("finished snapshot = %+v", snap)
	}
	// Updates after finish are ignored.
	h.update(id, core.ProgressUpdate{BytesDone: 9, TotalBytes: 10})
	if snap, _ := h.snapshot(id); snap.BytesDone != 5 {
		t.Fatalf("update after finish applied: %+v", snap)
	}
	time.Sleep(150 * time.Millisecond)
	if _, ok := h.snapshot(id); ok {
		t.Fatalf("finished record was not dropped after retention")
	}
	if found, _ := h.requestCancel(id); found {
		t.Fatalf("expired record still cancellable")
	}
}

func TestTransfersEndpointListsAndCancelReportsStatus(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	id, err := s.hub.startMeta(transferMeta{Kind: "upload", Label: "a.bin", DstServer: "web-01", DstPath: "/srv/a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(ts.URL + "/api/transfers?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Transfers []progressSnapshot `json:"transfers"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Transfers) != 1 || out.Transfers[0].ID != id || out.Transfers[0].Kind != "upload" {
		t.Fatalf("transfers = %+v", out.Transfers)
	}
	if code, _ := postAPI(t, s, ts.URL, "/api/transfers/cancel", url.Values{"id": {id}}, ts.URL); code != http.StatusConflict {
		t.Fatalf("cancel uncancellable: %d, want 409", code)
	}
	if code, _ := postAPI(t, s, ts.URL, "/api/transfers/cancel", url.Values{"id": {strings.Repeat("f", 32)}}, ts.URL); code != http.StatusNotFound {
		t.Fatalf("cancel unknown: %d, want 404", code)
	}
	if code, _ := postAPI(t, s, ts.URL, "/api/transfers/cancel", url.Values{}, ts.URL); code != http.StatusBadRequest {
		t.Fatalf("cancel without id: %d, want 400", code)
	}
}

// An id that never appears must not pin an SSE connection (and a server
// goroutine) for the whole 30-minute stream lifetime.
func TestProgressEndpointGivesUpOnUnknownID(t *testing.T) {
	old := progressUnknownGrace
	progressUnknownGrace = 100 * time.Millisecond
	t.Cleanup(func() { progressUnknownGrace = old })
	s, ts := newTestServer(t)

	start := time.Now()
	res, err := http.Get(ts.URL + "/api/progress?t=" + s.Token() + "&id=" + strings.Repeat("e", 32))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "event: gone") || time.Since(start) > 3*time.Second {
		t.Fatalf("progress stream for unknown id: %q after %v", body, time.Since(start))
	}
}
