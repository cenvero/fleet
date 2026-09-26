// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

// ---- progress hub ----
//
// The hub tracks every transfer the web UI starts (uploads, copies, moves and
// browser downloads) so the Transfers panel can show progress, speed and ETA,
// survive page navigation/reloads, and cancel what can be cancelled. Records
// are dropped hubRetention after they finish.

const (
	hubRetention = 30 * time.Second
	// hubMaxItems bounds memory: finished records are evicted first; if the hub
	// is full of live transfers a new one is refused rather than growing.
	hubMaxItems = 4096
)

var errHubFull = errors.New("too many active transfers")

// transferMeta describes a transfer for the Transfers panel.
type transferMeta struct {
	Kind  string `json:"kind,omitempty"` // upload | download | copy | move
	Label string `json:"label,omitempty"`
	// Endpoints; an empty server means the controller's Local filesystem
	// (or, for a download's destination / an upload's source, the browser).
	SrcServer string `json:"src_server,omitempty"`
	SrcPath   string `json:"src_path,omitempty"`
	DstServer string `json:"dst_server,omitempty"`
	DstPath   string `json:"dst_path,omitempty"`
}

type progressHub struct {
	mu    sync.Mutex
	items map[string]*liveTransfer
	rev   uint64
	// retention is how long finished records stay readable (tests shorten it).
	retention time.Duration
}

type liveTransfer struct {
	upd       core.ProgressUpdate
	done      bool
	err       string
	meta      transferMeta
	started   time.Time
	finished  time.Time
	cancel    context.CancelFunc
	cancelled bool
	rev       uint64
}

// progressSnapshot is the SSE / JSON payload. The first seven fields are the
// original /api/progress shape; the rest are additive.
type progressSnapshot struct {
	BytesDone     int64   `json:"bytes_done"`
	TotalBytes    int64   `json:"total_bytes"`
	RatePerSec    float64 `json:"rate_per_sec"`
	ActiveStreams int     `json:"active_streams"`
	Percent       int     `json:"percent"`
	Done          bool    `json:"done"`
	Error         string  `json:"error,omitempty"`
	ID            string  `json:"id,omitempty"`
	transferMeta
	StartedAt   time.Time `json:"started_at,omitzero"`
	Cancellable bool      `json:"cancellable,omitempty"`
	Cancelled   bool      `json:"cancelled,omitempty"`
}

func newProgressHub() *progressHub {
	return &progressHub{items: make(map[string]*liveTransfer), retention: hubRetention}
}

// start registers a new transfer under a fresh random id.
func (h *progressHub) start() string {
	id, _ := h.startMeta(transferMeta{})
	return id
}

// startMeta registers a new transfer with display metadata.
func (h *progressHub) startMeta(meta transferMeta) (string, error) {
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	id = id[:32]
	return id, h.startWithID(id, meta)
}

// trackIDPattern bounds client-chosen tracking ids (used for browser
// downloads, which are plain GET navigations and so cannot receive an id from
// a prior response).
var trackIDPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// startWithID registers a transfer under a caller-chosen id. It refuses a
// malformed or already-used id so one request cannot hijack another's record.
func (h *progressHub) startWithID(id string, meta transferMeta) error {
	if !trackIDPattern.MatchString(id) {
		return fmt.Errorf("invalid transfer id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.items[id]; exists {
		return fmt.Errorf("transfer id already in use")
	}
	if len(h.items) >= hubMaxItems {
		h.evictFinishedLocked()
		if len(h.items) >= hubMaxItems {
			return errHubFull
		}
	}
	h.rev++
	h.items[id] = &liveTransfer{meta: meta, started: time.Now(), rev: h.rev}
	return nil
}

func (h *progressHub) evictFinishedLocked() {
	for id, t := range h.items {
		if t.done {
			delete(h.items, id)
		}
	}
}

func (h *progressHub) update(id string, u core.ProgressUpdate) {
	h.mu.Lock()
	if t, ok := h.items[id]; ok && !t.done {
		t.upd = u
		h.rev++
		t.rev = h.rev
	}
	h.mu.Unlock()
}

// setCancel makes a transfer cancellable through /api/transfers/cancel.
func (h *progressHub) setCancel(id string, cancel context.CancelFunc) {
	h.mu.Lock()
	if t, ok := h.items[id]; ok && !t.done {
		t.cancel = cancel
		h.rev++
		t.rev = h.rev
	}
	h.mu.Unlock()
}

// requestCancel cancels a live, cancellable transfer. found reports whether
// the id exists; ok whether a cancellation was actually delivered.
func (h *progressHub) requestCancel(id string) (found, ok bool) {
	h.mu.Lock()
	t, exists := h.items[id]
	if !exists {
		h.mu.Unlock()
		return false, false
	}
	if t.done || t.cancel == nil {
		h.mu.Unlock()
		return true, false
	}
	cancel := t.cancel
	t.cancelled = true
	h.rev++
	t.rev = h.rev
	h.mu.Unlock()
	cancel()
	return true, true
}

func (h *progressHub) finish(id string, err error) {
	h.mu.Lock()
	t, ok := h.items[id]
	if ok && !t.done {
		t.done = true
		t.finished = time.Now()
		t.cancel = nil
		if err != nil {
			t.err = err.Error()
		} else if t.upd.TotalBytes > 0 {
			t.upd.BytesDone = t.upd.TotalBytes
		}
		h.rev++
		t.rev = h.rev
	}
	retention := h.retention
	h.mu.Unlock()
	if !ok {
		return
	}
	// Drop the record a little later so a slow poller can still read the
	// terminal state.
	time.AfterFunc(retention, func() {
		h.mu.Lock()
		if cur, exists := h.items[id]; exists && cur == t {
			delete(h.items, id)
		}
		h.mu.Unlock()
	})
}

func (t *liveTransfer) snapshot(id string) progressSnapshot {
	pct := 0
	if t.upd.TotalBytes > 0 {
		pct = int(t.upd.BytesDone * 100 / t.upd.TotalBytes)
	} else if t.done && t.err == "" {
		pct = 100
	}
	return progressSnapshot{
		BytesDone:     t.upd.BytesDone,
		TotalBytes:    t.upd.TotalBytes,
		RatePerSec:    t.upd.RatePerSec,
		ActiveStreams: t.upd.ActiveStreams,
		Percent:       pct,
		Done:          t.done,
		Error:         t.err,
		ID:            id,
		transferMeta:  t.meta,
		StartedAt:     t.started,
		Cancellable:   t.cancel != nil && !t.done,
		Cancelled:     t.cancelled,
	}
}

func (h *progressHub) snapshot(id string) (progressSnapshot, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.items[id]
	if !ok {
		return progressSnapshot{}, false
	}
	return t.snapshot(id), true
}

// list returns every record changed after rev (0 = all), oldest first, plus
// the hub's current revision.
func (h *progressHub) list(after uint64) ([]progressSnapshot, uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]progressSnapshot, 0, len(h.items))
	for id, t := range h.items {
		if t.rev > after {
			out = append(out, t.snapshot(id))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, h.rev
}

// displayBase is the file's base name in its source's path style.
func (s *Server) displayBase(server, p string) string {
	if server == "" {
		return filepath.Base(p)
	}
	if style, err := s.targetPathStyle(server); err == nil {
		return style.Base(p)
	}
	return p
}

// ---- handlers ----

// handleTransfers lists live and recently finished transfers (GET).
func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	items, _ := s.hub.list(0)
	writeJSON(w, map[string]any{"transfers": items})
}

// handleTransferCancel cancels a cancellable transfer (POST, CSRF-checked).
// Server-side copy/move/upload legs run inside core APIs that cannot be
// interrupted, so only transfers the web UI itself streams are cancellable.
func (s *Server) handleTransferCancel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	found, ok := s.hub.requestCancel(id)
	switch {
	case !found:
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "unknown transfer"})
	case !ok:
		writeJSONStatus(w, http.StatusConflict, map[string]string{"error": "this transfer can no longer be cancelled"})
	default:
		writeJSON(w, map[string]string{"status": "cancelling"})
	}
}

// sseHeartbeat keeps idle event streams alive through proxies and lets the
// server notice a vanished client.
const sseHeartbeat = 15 * time.Second

// handleTransferStream is a single multiplexed SSE feed of every transfer's
// progress. Browsers cap concurrent HTTP/1.1 connections per host (typically
// six), so one shared stream — instead of one EventSource per transfer — keeps
// many parallel transfers from starving the UI's API calls.
func (s *Server) handleTransferStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx, cancel := context.WithTimeout(r.Context(), maxSSELifetime)
	defer cancel()

	var sent uint64
	send := func(force bool) bool {
		items, rev := s.hub.list(sent)
		if len(items) == 0 && !force {
			return true
		}
		payload, err := json.Marshal(items)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		sent = rev
		return rc.Flush() == nil
	}
	if !send(true) {
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	lastWrite := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			before := sent
			if !send(false) {
				return
			}
			if sent != before {
				lastWrite = time.Now()
				continue
			}
			if time.Since(lastWrite) >= sseHeartbeat {
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
				lastWrite = time.Now()
			}
		}
	}
}

func writeJSONStatus(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
