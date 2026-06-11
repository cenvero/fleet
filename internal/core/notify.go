// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// NotifyStore is a small standalone store of notification targets (webhooks and
// Slack incoming webhooks). It is persisted as a single JSON document at
// <configDir>/notify.json (0600). Like TagStore it is kept off *App so it does
// not require touching app.go: open it from a config dir, then read/modify/write.
type NotifyStore struct {
	path string
	mu   sync.Mutex

	// client is the HTTP client used by Send. It is set to a short-timeout
	// default but may be overridden in tests.
	client *http.Client
}

// NotifyKind is the kind of notification target.
type NotifyKind string

const (
	// NotifyKindSlack posts {"text": message} to a Slack incoming webhook URL.
	NotifyKindSlack NotifyKind = "slack"
	// NotifyKindWebhook posts {event, message, time} to an arbitrary URL.
	NotifyKindWebhook NotifyKind = "webhook"
)

// Notification events that a target may subscribe to.
const (
	NotifyEventOffline     = "offline"
	NotifyEventJobFailed   = "job-failed"
	NotifyEventDestructive = "destructive"
	NotifyEventDrift       = "drift"
	NotifyEventOnline      = "online"
)

// NotifyEvents is the canonical, ordered list of known events.
var NotifyEvents = []string{
	NotifyEventOffline,
	NotifyEventJobFailed,
	NotifyEventDestructive,
	NotifyEventDrift,
	NotifyEventOnline,
}

// ValidNotifyEvent reports whether event is one of the known events.
func ValidNotifyEvent(event string) bool {
	for _, e := range NotifyEvents {
		if e == event {
			return true
		}
	}
	return false
}

// NotifyTarget is a single delivery destination plus the set of events it wants.
type NotifyTarget struct {
	Kind   NotifyKind `json:"kind"`
	URL    string     `json:"url"`
	Events []string   `json:"events"`
}

// Subscribed reports whether this target wants the given event.
func (t NotifyTarget) Subscribed(event string) bool {
	for _, e := range t.Events {
		if e == event {
			return true
		}
	}
	return false
}

// notifyDocument is the on-disk JSON shape.
type notifyDocument struct {
	Targets []NotifyTarget `json:"targets"`
}

// NewNotifyStore opens (without reading) a notify store rooted at configDir. If
// configDir is empty the default config dir is used.
func NewNotifyStore(configDir string) *NotifyStore {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	return &NotifyStore{
		path:   NotifyPath(configDir),
		client: &http.Client{Timeout: 8 * time.Second},
	}
}

// NotifyPath returns the on-disk location of the notify document for a config dir.
func NotifyPath(configDir string) string {
	return filepath.Join(configDir, "notify.json")
}

func (s *NotifyStore) read() (notifyDocument, error) {
	doc := notifyDocument{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, fmt.Errorf("read notify targets: %w", err)
	}
	if len(data) == 0 {
		return doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("decode notify targets: %w", err)
	}
	return doc, nil
}

func (s *NotifyStore) write(doc notifyDocument) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode notify targets: %w", err)
	}
	// Atomic write: temp file in the same dir -> chmod 0600 -> rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".notify-*.json")
	if err != nil {
		return fmt.Errorf("write notify targets: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write notify targets: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write notify targets: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write notify targets: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("write notify targets: %w", err)
	}
	return nil
}

// normalizeTarget validates and cleans a target before it is stored.
func normalizeTarget(t NotifyTarget) (NotifyTarget, error) {
	t.Kind = NotifyKind(strings.ToLower(strings.TrimSpace(string(t.Kind))))
	switch t.Kind {
	case NotifyKindSlack, NotifyKindWebhook:
	default:
		return NotifyTarget{}, fmt.Errorf("invalid kind %q (want %q or %q)", t.Kind, NotifyKindSlack, NotifyKindWebhook)
	}
	t.URL = strings.TrimSpace(t.URL)
	if !strings.HasPrefix(t.URL, "http://") && !strings.HasPrefix(t.URL, "https://") {
		return NotifyTarget{}, fmt.Errorf("invalid url %q (must start with http:// or https://)", t.URL)
	}
	// De-duplicate and validate events, preserving canonical order.
	seen := map[string]bool{}
	var events []string
	for _, e := range t.Events {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !ValidNotifyEvent(e) {
			return NotifyTarget{}, fmt.Errorf("invalid event %q (want one of %s)", e, strings.Join(NotifyEvents, ", "))
		}
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		return NotifyTarget{}, fmt.Errorf("at least one event is required (one of %s)", strings.Join(NotifyEvents, ", "))
	}
	sort.SliceStable(events, func(i, j int) bool {
		return notifyEventOrder(events[i]) < notifyEventOrder(events[j])
	})
	t.Events = events
	return t, nil
}

func notifyEventOrder(event string) int {
	for i, e := range NotifyEvents {
		if e == event {
			return i
		}
	}
	return len(NotifyEvents)
}

// Add validates and appends a target. Adding a target whose kind+URL already
// exists replaces its event set instead of creating a duplicate.
func (s *NotifyStore) Add(target NotifyTarget) error {
	normalized, err := normalizeTarget(target)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return err
	}
	for i := range doc.Targets {
		if doc.Targets[i].Kind == normalized.Kind && doc.Targets[i].URL == normalized.URL {
			doc.Targets[i].Events = normalized.Events
			return s.write(doc)
		}
	}
	doc.Targets = append(doc.Targets, normalized)
	return s.write(doc)
}

// List returns a copy of all configured targets.
func (s *NotifyStore) List() ([]NotifyTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]NotifyTarget, len(doc.Targets))
	copy(out, doc.Targets)
	return out, nil
}

// Remove deletes a target by zero-based index or by exact URL match. It returns
// the removed target so callers can report what was removed.
func (s *NotifyStore) Remove(indexOrURL string) (NotifyTarget, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.read()
	if err != nil {
		return NotifyTarget{}, err
	}
	if len(doc.Targets) == 0 {
		return NotifyTarget{}, fmt.Errorf("no notification targets configured")
	}
	idx := -1
	// Try index first.
	if n, convErr := parseIndex(indexOrURL); convErr == nil {
		if n < 0 || n >= len(doc.Targets) {
			return NotifyTarget{}, fmt.Errorf("index %d out of range (have %d targets)", n, len(doc.Targets))
		}
		idx = n
	} else {
		// Fall back to URL match.
		want := strings.TrimSpace(indexOrURL)
		for i := range doc.Targets {
			if doc.Targets[i].URL == want {
				idx = i
				break
			}
		}
		if idx == -1 {
			return NotifyTarget{}, fmt.Errorf("no target matching %q (use an index from 'fleet notify list' or the exact url)", indexOrURL)
		}
	}
	removed := doc.Targets[idx]
	doc.Targets = append(doc.Targets[:idx], doc.Targets[idx+1:]...)
	if err := s.write(doc); err != nil {
		return NotifyTarget{}, err
	}
	return removed, nil
}

// parseIndex parses a non-negative integer index, rejecting anything that looks
// like a URL so "https://..." doesn't accidentally parse.
func parseIndex(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not an index")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// Send POSTs a single notification to one target. Slack targets receive
// {"text": message}; webhook targets receive {event, message, time}. A non-2xx
// HTTP status is treated as an error.
func (s *NotifyStore) Send(target NotifyTarget, event, message string) error {
	var body []byte
	var err error
	switch target.Kind {
	case NotifyKindSlack:
		body, err = json.Marshal(map[string]string{"text": message})
	case NotifyKindWebhook:
		body, err = json.Marshal(map[string]string{
			"event":   event,
			"message": message,
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	default:
		return fmt.Errorf("invalid kind %q", target.Kind)
	}
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post to %s: %w", target.URL, err)
	}
	defer resp.Body.Close()
	// Drain a little of the body so the connection can be reused, then discard.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("post to %s: unexpected status %s", target.URL, resp.Status)
	}
	return nil
}

// Fire sends message to every target subscribed to event. It attempts all
// matching targets and returns a combined error if any failed. A nil error
// means every matching target (possibly zero) was delivered successfully.
func (s *NotifyStore) Fire(event, message string) error {
	targets, err := s.List()
	if err != nil {
		return err
	}
	var errs []string
	for _, t := range targets {
		if !t.Subscribed(event) {
			continue
		}
		if sendErr := s.Send(t, event, message); sendErr != nil {
			errs = append(errs, sendErr.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("notify fire %q: %s", event, strings.Join(errs, "; "))
	}
	return nil
}
