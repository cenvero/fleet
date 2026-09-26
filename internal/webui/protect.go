// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/core"
)

// errProtectedPath is returned for any Local-source request that would read,
// list, write or delete inside a protected controller location: the
// configuration directory (SSH private keys, RBAC token hashes, the secret
// store, enrollment secrets, server records, automations and the audit log) and
// the key and known-hosts files the controller uses even when they live
// elsewhere. A browser tab must never be able to list or exfiltrate them (or
// plant files in them), even though the operator running `fleet file ui` could
// read them from a shell.
var errProtectedPath = errors.New("access denied: the controller's configuration directory and key files (keys, tokens, secrets) are not available in the web UI")

// errContainsProtected is returned when an operation would act on a whole tree
// (recursive copy, move, rename, delete, archive, extraction target) that
// contains a protected location. Acting on the ancestor would copy, archive,
// relocate or overwrite the protected files without ever naming them.
var errContainsProtected = errors.New("access denied: this location contains the controller's configuration directory or key files, so the web UI will not copy, move, rename, archive or delete it as a whole")

// isProtectedErr reports whether err is one of the guard's refusals.
func isProtectedErr(err error) bool {
	return errors.Is(err, errProtectedPath) || errors.Is(err, errContainsProtected)
}

// localGuard confines the Local source away from protected locations. It is
// deliberately conservative and compares paths three ways, refusing if any of
// them matches:
//
//   - lexically, as given (folding case on macOS and Windows);
//   - after resolving symlinks in the longest existing prefix, so a symlink
//     planted elsewhere cannot be used to reach a protected root;
//   - by file identity (os.SameFile) of every existing ancestor, so spellings
//     the filesystem treats as the same entry — Unicode NFC vs NFD names on
//     macOS, case variants on case-insensitive volumes, bind mounts, hard links
//     to a key file — cannot slip past the string comparisons.
type localGuard struct {
	roots []string
	fold  bool
	// ids holds, per root, the identity of every existing ancestor-or-self of
	// that root together with the path components below it.
	ids [][]pathLink
}

// pathLink is one existing ancestor-or-self of a path: its identity and the
// components of the original path beneath it.
type pathLink struct {
	info os.FileInfo
	rest []string
}

func newLocalGuard(paths ...string) localGuard {
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
	for _, d := range paths {
		if strings.TrimSpace(d) == "" {
			continue
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			continue
		}
		add(abs)
		if resolved := resolveExistingPrefix(abs); resolved != "" {
			add(resolved)
		}
	}
	for _, root := range g.roots {
		if chain := existingChain(root); len(chain) > 0 {
			g.ids = append(g.ids, chain)
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

// hasPrefix reports whether the component list full starts with prefix.
func (g localGuard) hasPrefix(full, prefix []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if g.key(prefix[i]) != g.key(full[i]) {
			return false
		}
	}
	return true
}

// matches reports whether p lies inside (or is) a protected root, or — when
// tree is set — whether a protected root lies inside p.
func (g localGuard) matches(p string, tree bool) bool {
	forms := []string{p}
	if resolved := resolveExistingPrefix(p); resolved != "" && resolved != p {
		forms = append(forms, resolved)
	}
	for _, f := range forms {
		for _, root := range g.roots {
			if g.within(root, f) || (tree && g.within(f, root)) {
				return true
			}
		}
	}
	chain := existingChain(p)
	for _, rootChain := range g.ids {
		for _, q := range chain {
			for _, a := range rootChain {
				if !os.SameFile(q.info, a.info) {
					continue
				}
				// q and a are the same directory entry D: p = D/q.rest and the
				// root = D/a.rest.
				if g.hasPrefix(q.rest, a.rest) || (tree && g.hasPrefix(a.rest, q.rest)) {
					return true
				}
			}
		}
	}
	return false
}

// check returns errProtectedPath when the absolute, cleaned path p (or where it
// resolves to, or any spelling of it the filesystem treats as the same entry)
// is inside a protected root. Use it for single-entry operations: listing,
// reading, writing or creating one file or directory.
func (g localGuard) check(p string) error {
	if len(g.roots) == 0 {
		return nil
	}
	if g.matches(p, false) {
		return errProtectedPath
	}
	return nil
}

// checkTree is check plus a refusal when p contains a protected root (is an
// ancestor of it or equal to it). Use it for every operation that acts on a
// whole tree: copy, move, rename, delete, archive members and the target of a
// merge.
func (g localGuard) checkTree(p string) error {
	if len(g.roots) == 0 {
		return nil
	}
	if g.matches(p, false) {
		return errProtectedPath
	}
	if g.matches(p, true) {
		return errContainsProtected
	}
	return nil
}

// checkFollowedTree walks p the way a symlink-following archiver (`zip -r`)
// does and refuses if any symlink beneath it reaches, or contains, a protected
// root. Symlinked directories are walked too, once each.
func (g localGuard) checkFollowedTree(p string) error {
	if len(g.roots) == 0 {
		return nil
	}
	visited := map[string]bool{}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > 40 {
			return fmt.Errorf("too many nested symlinks under %s", dir)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || visited[real] {
			return nil // gone, dangling or already walked: nothing to follow
		}
		visited[real] = true
		return filepath.WalkDir(real, func(q string, d fs.DirEntry, err error) error { // #nosec G703 -- read-only walk of an operator-selected tree, only to find symlinks
			if err != nil {
				return nil // unreadable here means unreadable to the archiver too
			}
			if d.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			if err := g.checkTree(q); err != nil {
				return err
			}
			if info, err := os.Stat(q); err == nil && info.IsDir() {
				return walk(q, depth+1)
			}
			return nil
		})
	}
	return walk(p, 0)
}

// existingChain returns the identity of every existing ancestor-or-self of the
// cleaned path p (following symlinks), deepest first, each with the components
// of p beneath it.
//
// Security: p is the request path being checked; this is the guard itself,
// and it only stats p and its ancestors to decide whether to refuse it.
func existingChain(p string) []pathLink {
	p = filepath.Clean(p)
	var below []string // components under cur, innermost last-in
	var out []pathLink
	cur := p
	for i := 0; i < 512; i++ {
		// codeql[go/path-injection] read-only stat performed by the protected-path guard itself to decide whether to refuse the request
		if info, err := os.Stat(cur); err == nil {
			rest := make([]string, len(below))
			for j := range below {
				rest[j] = below[len(below)-1-j]
			}
			out = append(out, pathLink{info: info, rest: rest})
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		below = append(below, filepath.Base(cur))
		cur = parent
	}
	return out
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

// guardTTL bounds how long a protected-path snapshot is reused. It only
// collapses bursts of requests (a pane repaint lists several directories);
// server key paths are re-read from the server records after it expires.
const guardTTL = time.Second

// pathGuard returns the current protected-path guard: the controller's config dir
// plus every key, known-hosts, data and log location the controller is
// configured to use, including each managed server's SSH key.
func (s *Server) pathGuard() localGuard {
	if s.app == nil {
		return localGuard{}
	}
	s.guardMu.Lock()
	defer s.guardMu.Unlock()
	if !s.guardAt.IsZero() && time.Since(s.guardAt) < guardTTL {
		return s.guardSnap
	}
	s.guardSnap = newLocalGuard(s.protectedPaths()...)
	s.guardAt = time.Now()
	return s.guardSnap
}

// protectedPaths lists the controller locations the Local source must never
// expose. Most live inside the config dir by default, but each can be
// configured elsewhere (and server key paths usually are). Caller holds guardMu.
func (s *Server) protectedPaths() []string {
	cfg := s.app.Config
	paths := []string{
		s.app.ConfigDir,
		cfg.ConfigDir,
		cfg.Crypto.KnownHostsPath,
		cfg.Crypto.RotationDirectory,
		cfg.Runtime.DataDir,
		cfg.Runtime.LogDir,
		cfg.Runtime.AggregatedLogDir,
	}
	if filepath.IsAbs(cfg.Crypto.PrimaryKey) {
		paths = append(paths, cfg.Crypto.PrimaryKey)
	}
	for _, db := range []string{cfg.Database.SQLite.StatePath, cfg.Database.SQLite.MetricsPath, cfg.Database.SQLite.EventsPath} {
		if db != "" {
			paths = append(paths, db, db+"-wal", db+"-shm", db+"-journal")
		}
	}
	if servers, err := s.app.ListServers(); err == nil {
		keys := make([]string, 0, 2*len(servers))
		for _, rec := range servers {
			keys = append(keys, rec.KeyPath, rec.Agent.LoginKey)
		}
		s.serverKeys = keys
	}
	// On a read error the last known key paths stay protected.
	return append(paths, s.serverKeys...)
}

// CheckLocalPath applies the Local source's protected-location guard to a
// controller-local path for callers outside the web UI (the CLI refuses a
// scoped RBAC token the same locations). It returns an error when p is, lies
// inside, resolves into or — with tree set — contains the controller's config
// directory or any key, known-hosts, data, log or database location.
func CheckLocalPath(app *core.App, p string, tree bool) error {
	if app == nil {
		return nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	g := (&Server{app: app}).pathGuard()
	if tree {
		return g.checkTree(abs)
	}
	return g.check(abs)
}

// cleanLocal validates a controller-side path exactly like cleanLocalPath and
// additionally refuses protected locations. Every Local-source handler that
// touches a single entry goes through it.
//
// Security: the Local source deliberately accepts any absolute path the
// operator picks — it is the operator's own file manager, reachable only over
// loopback with the per-process token, a loopback Host header and (for every
// mutation) a same-origin POST. The trust boundary is therefore the protected
// controller locations, not the path itself: cleanLocal, cleanLocalWrite and
// cleanLocalTree are the checks every Local handler applies before touching
// the filesystem, which is why CodeQL go/path-injection alerts on those
// handlers (and on the core helpers they call) are false positives.
func (s *Server) cleanLocal(p string) (string, error) {
	clean, err := cleanLocalPath(p)
	if err != nil {
		return "", err
	}
	if err := s.pathGuard().check(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// cleanLocalWrite is cleanLocal for a single-entry mutation (write, create,
// chmod, archive output, extraction target): the raw path must be non-empty
// and free of "." and ".." components before it is cleaned.
func (s *Server) cleanLocalWrite(p string) (string, error) {
	if err := validateLocalMutationPath(p); err != nil {
		return "", err
	}
	return s.cleanLocal(p)
}

// cleanLocalTree is cleanLocalWrite for operations that act on a whole tree
// (copy, move, rename, delete, duplicate): it also refuses a path that
// contains a protected location.
func (s *Server) cleanLocalTree(p string) (string, error) {
	if err := validateLocalMutationPath(p); err != nil {
		return "", err
	}
	clean, err := cleanLocalPath(p)
	if err != nil {
		return "", err
	}
	if err := s.pathGuard().checkTree(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// initialLocalRoot is the Local pane's starting directory: the working
// directory, unless that is itself protected, in which case the directory
// containing the protected tree is used instead.
func (s *Server) initialLocalRoot() string {
	start := initialLocalPath()
	g := s.pathGuard()
	for i := 0; i < 64 && g.check(start) != nil; i++ {
		parent := filepath.Dir(start)
		if parent == start {
			break
		}
		start = parent
	}
	return start
}

// checkLocalCompress refuses a local archive that would include a protected
// location (a selected entry that is, contains or links to one) or whose output
// file would land on one. zip follows symlinks while archiving, so for zip the
// selected trees are also walked for symlinks that lead to a protected root.
func (s *Server) checkLocalCompress(dir string, names []string, archive, format string) error {
	g := s.pathGuard()
	for _, name := range names {
		member := filepath.Join(dir, filepath.Base(name))
		if err := g.checkTree(member); err != nil {
			return err
		}
		if format == "zip" {
			if err := g.checkFollowedTree(member); err != nil {
				return err
			}
		}
	}
	return g.check(filepath.Join(dir, filepath.Base(archive)))
}

// extractLocalGuarded extracts a controller-local archive into its own
// directory without ever letting a member land inside a protected location.
//
// When the destination directory contains no protected root, core's extractor
// is already sufficient: it refuses link members and confines every write to
// the destination through os.Root, so no member can reach a protected root.
//
// When the destination does contain one (an archive sitting in $HOME next to
// the config dir), the archive is copied into a private staging directory
// inside the controller's own tmp dir — which the Local source can never reach,
// so neither the copy nor its extracted tree can be swapped by a concurrent
// request — extracted there, and every extracted member's final destination is
// checked against the guard before anything is merged into place.
func (s *Server) extractLocalGuarded(archive string) error {
	g := s.pathGuard()
	destDir := filepath.Dir(archive)
	if err := g.checkTree(destDir); err == nil {
		return s.app.ExtractArchive("", archive)
	} else if !errors.Is(err, errContainsProtected) {
		return err
	}

	stageParent := filepath.Join(s.app.ConfigDir, "tmp")
	if err := os.MkdirAll(stageParent, 0o700); err != nil {
		return fmt.Errorf("prepare private extraction directory: %w", err)
	}
	stage, err := os.MkdirTemp(stageParent, "webui-extract-*")
	if err != nil {
		return fmt.Errorf("prepare private extraction directory: %w", err)
	}
	defer os.RemoveAll(stage)
	members := filepath.Join(stage, "members")
	if err := os.Mkdir(members, 0o700); err != nil {
		return err
	}
	tag, err := randomToken()
	if err != nil {
		return err
	}
	// Keep the original base name so core picks the same format; the random
	// prefix keeps the copy from colliding with any member.
	//
	// Security: staged is a single random-prefixed entry (filepath.Base of the
	// already-guarded archive path) inside the private 0700 staging directory,
	// which the Local source can never reach.
	staged := filepath.Join(members, ".fleet-extract-"+tag[:16]+"-"+filepath.Base(archive))
	if err := copyArchiveForStaging(archive, staged); err != nil {
		return err
	}
	if err := s.app.ExtractArchive("", staged); err != nil {
		return err
	}
	// codeql[go/path-injection] generated Base() name inside a freshly created private 0700 staging directory
	if err := os.Remove(staged); err != nil {
		return err
	}
	if err := filepath.WalkDir(members, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(members, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink archive member %s", rel)
		}
		return g.check(filepath.Join(destDir, rel))
	}); err != nil {
		return err
	}
	if err := core.CopyLocalTreeAtomic(members, destDir); err != nil {
		return err
	}
	s.audit("file.extract", "local", fmt.Sprintf("%s -> %s (web UI; extracted privately and every member checked because the folder contains protected controller files)", archive, destDir))
	return nil
}

// copyArchiveForStaging copies the archive into the private staging dir.
func copyArchiveForStaging(src, dst string) error {
	// codeql[go/path-injection] web file manager: loopback-only, per-process token, same-origin POST; protected controller paths are refused by cleanLocal/cleanLocalWrite before this, and acting on operator-chosen local paths is its purpose
	in, err := os.Open(src) // #nosec G304,G703 -- operator-selected local archive; the caller's guard refused protected paths
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", filepath.Base(src))
	}
	// codeql[go/path-injection] generated Base() name inside a freshly created private 0700 staging directory
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304,G703 -- generated name inside a private 0700 staging dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
