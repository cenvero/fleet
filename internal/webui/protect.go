// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// errProtectedPath is returned for any Local-source request that would read,
// list, write or delete inside the controller's own configuration directory.
// That directory holds the controller's SSH private keys, RBAC token hashes,
// the secret store, enrollment secrets and the audit log; a browser tab must
// never be able to list or exfiltrate it (or plant files in it), even though
// the operator running `fleet file ui` could read it from a shell.
var errProtectedPath = errors.New("access denied: the controller's configuration directory (keys, tokens, secrets) is not available in the web UI")

// localGuard confines the Local source away from protected directories. It is
// deliberately conservative: a path is refused when either its lexical form or
// its symlink-resolved form lies inside a protected root, so a symlink planted
// elsewhere cannot be used to reach the config dir through the UI.
type localGuard struct {
	roots []string
	fold  bool
}

func newLocalGuard(dirs ...string) localGuard {
	g := localGuard{fold: runtime.GOOS == "windows" || runtime.GOOS == "darwin"}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		p = filepath.Clean(p)
		key := g.key(p)
		if seen[key] {
			return
		}
		seen[key] = true
		g.roots = append(g.roots, p)
	}
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		add(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			add(resolved)
		}
	}
	return g
}

func (g localGuard) key(p string) string {
	if g.fold {
		return strings.ToLower(p)
	}
	return p
}

// within reports whether p equals root or lies beneath it.
func (g localGuard) within(root, p string) bool {
	root, p = g.key(root), g.key(p)
	if p == root {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(p, prefix)
}

func (g localGuard) contains(p string) bool {
	for _, root := range g.roots {
		if g.within(root, p) {
			return true
		}
	}
	return false
}

// check returns errProtectedPath when the absolute, cleaned path p (or where
// it resolves to through symlinks) is inside a protected root.
func (g localGuard) check(p string) error {
	if len(g.roots) == 0 {
		return nil
	}
	if g.contains(p) {
		return errProtectedPath
	}
	if resolved := resolveExistingPrefix(p); resolved != "" && g.contains(resolved) {
		return errProtectedPath
	}
	return nil
}

// resolveExistingPrefix resolves symlinks in the longest existing ancestor of
// p and re-appends the not-yet-existing tail, so a create/write target such as
// "<symlink-to-config>/new.txt" is attributed to where it would really land.
func resolveExistingPrefix(p string) string {
	p = filepath.Clean(p)
	var tail []string
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// cleanLocal validates a controller-side path exactly like cleanLocalPath and
// additionally refuses protected locations. Every Local-source handler goes
// through it.
func (s *Server) cleanLocal(p string) (string, error) {
	clean, err := cleanLocalPath(p)
	if err != nil {
		return "", err
	}
	if err := s.localGuard.check(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// initialLocalRoot is the Local pane's starting directory: the working
// directory, unless that is itself protected, in which case the directory
// containing the protected tree is used instead.
func (s *Server) initialLocalRoot() string {
	start := initialLocalPath()
	for i := 0; i < 64 && s.localGuard.check(start) != nil; i++ {
		parent := filepath.Dir(start)
		if parent == start {
			break
		}
		start = parent
	}
	return start
}
