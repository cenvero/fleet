// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// editHistory keeps the previous version of files edited through Fleet, so an
// edit can be undone. It lives in the controller's private data directory:
//
//	data/edit-history/<sha256(server)>/<sha256(path)>/<id>.json   metadata
//	data/edit-history/<sha256(server)>/<sha256(path)>/<id>.orig   content
//
// Directory and file names are hashes and generated ids, never text from a
// request, so no server name or remote path can steer where anything is
// written. Everything is created 0700/0600 and opened through an os.Root.
type editHistory struct {
	dir string
}

type editHistoryEntry struct {
	ID           string    `json:"id"`
	Server       string    `json:"server"`
	Path         string    `json:"path"`
	ResolvedPath string    `json:"resolved_path"`
	Time         time.Time `json:"time"`
	Operator     string    `json:"operator,omitempty"`
	OldSHA256    string    `json:"old_sha256"`
	NewSHA256    string    `json:"new_sha256"`
	OldSize      int64     `json:"old_size"`
	// dirRel is where the entry was found (not serialised).
	dirRel string
}

// EditHistoryItem is one undo entry as shown to users.
type EditHistoryItem struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Operator  string    `json:"operator,omitempty"`
	Path      string    `json:"path"`
	OldSHA256 string    `json:"old_sha256"`
	NewSHA256 string    `json:"new_sha256"`
	OldSize   int64     `json:"old_size"`
}

func (e editHistoryEntry) item() EditHistoryItem {
	return EditHistoryItem{ID: e.ID, Time: e.Time, Operator: e.Operator, Path: e.ResolvedPath, OldSHA256: e.OldSHA256, NewSHA256: e.NewSHA256, OldSize: e.OldSize}
}

func (a *App) editHistory() editHistory {
	return editHistory{dir: filepath.Join(a.ConfigDir, "data", "edit-history")}
}

func hashName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func (h editHistory) open(create bool) (*os.Root, error) {
	if create {
		if err := os.MkdirAll(h.dir, 0o700); err != nil {
			return nil, err
		}
	}
	return os.OpenRoot(h.dir)
}

// record stores original as the version before an edit and trims the file's
// history to keep entries.
func (h editHistory) record(e editHistoryEntry, original []byte, keep int) (string, error) {
	root, err := h.open(true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	e.Time = time.Now().UTC()
	id, err := newEditID()
	if err != nil {
		return "", err
	}
	e.ID = fmt.Sprintf("%d-%s", e.Time.UnixNano(), strings.TrimPrefix(id, "edit-")[:12])
	dirRel := filepath.Join(hashName(e.Server), hashName(e.ResolvedPath))
	if err := root.MkdirAll(dirRel, 0o700); err != nil {
		return "", err
	}
	meta, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := root.WriteFile(filepath.Join(dirRel, e.ID+".orig"), original, 0o600); err != nil {
		return "", err
	}
	if err := root.WriteFile(filepath.Join(dirRel, e.ID+".json"), meta, 0o600); err != nil {
		_ = root.Remove(filepath.Join(dirRel, e.ID+".orig"))
		return "", err
	}
	entries, err := h.readDir(root, dirRel)
	if err == nil {
		for _, old := range entries[min(keep, len(entries)):] {
			_ = h.removeIn(root, old)
		}
	}
	return e.ID, nil
}

// list returns the history for a file on a server, newest first. remotePath
// may be the path an edit was made through or the file it resolved to.
func (h editHistory) list(server, remotePath string) ([]editHistoryEntry, error) {
	root, err := h.open(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	serverDir := hashName(server)
	entries, err := h.readDir(root, filepath.Join(serverDir, hashName(remotePath)))
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		return entries, nil
	}
	// Not recorded under that name: the edit may have gone through another
	// path (a symlink) to the same file.
	dirs, err := fs.ReadDir(root.FS(), serverDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var matched []editHistoryEntry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		es, err := h.readDir(root, filepath.Join(serverDir, d.Name()))
		if err != nil {
			continue
		}
		for _, e := range es {
			if e.Path == remotePath || e.ResolvedPath == remotePath {
				matched = append(matched, es...)
				break
			}
		}
	}
	sortNewestFirst(matched)
	return matched, nil
}

func (h editHistory) readDir(root *os.Root, dirRel string) ([]editHistoryEntry, error) {
	names, err := fs.ReadDir(root.FS(), filepath.ToSlash(dirRel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []editHistoryEntry
	for _, n := range names {
		if n.IsDir() || !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		data, err := root.ReadFile(filepath.Join(dirRel, n.Name()))
		if err != nil {
			continue
		}
		var e editHistoryEntry
		if json.Unmarshal(data, &e) != nil || e.ID+".json" != n.Name() {
			continue
		}
		e.dirRel = dirRel
		out = append(out, e)
	}
	sortNewestFirst(out)
	return out, nil
}

func sortNewestFirst(es []editHistoryEntry) {
	sort.SliceStable(es, func(i, j int) bool { return es[i].Time.After(es[j].Time) })
}

func (h editHistory) content(e editHistoryEntry) ([]byte, error) {
	root, err := h.open(false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := root.ReadFile(filepath.Join(e.dirRel, e.ID+".orig"))
	if err != nil {
		return nil, fmt.Errorf("read the kept version: %w", err)
	}
	if sha256Hex(data) != e.OldSHA256 {
		return nil, fmt.Errorf("the kept version of %s is damaged (its sha256 does not match); not restoring it", e.ResolvedPath)
	}
	return data, nil
}

func (h editHistory) remove(e editHistoryEntry) error {
	root, err := h.open(false)
	if err != nil {
		return err
	}
	defer root.Close()
	return h.removeIn(root, e)
}

func (h editHistory) removeIn(root *os.Root, e editHistoryEntry) error {
	errMeta := root.Remove(filepath.Join(e.dirRel, e.ID+".json"))
	errOrig := root.Remove(filepath.Join(e.dirRel, e.ID+".orig"))
	if errMeta != nil && !errors.Is(errMeta, fs.ErrNotExist) {
		return errMeta
	}
	if errOrig != nil && !errors.Is(errOrig, fs.ErrNotExist) {
		return errOrig
	}
	return nil
}

// humanBytes formats a byte count for messages ("8.0 MiB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
