// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/pkg/proto"
)

// The pane's listing is not a safe answer to "is this name free?": it leaves
// out dotfiles while hidden files are off, and a name filter narrows it
// further. New file, new folder, rename and duplicate therefore ask the
// directory itself, off the UI thread, right before they act, and refuse to
// replace anything that is there.

// remoteLookup is the part of *core.App the name checks use.
type remoteLookup interface {
	StatRemoteFile(serverName, remotePath string) (proto.FileStatResult, error)
	ListRemoteDirHidden(serverName, remotePath string, showHidden bool) (proto.FileListResult, error)
}

// lookupFor returns what the name checks query for a remote pane. Tests
// replace it to stand in for an agent.
var lookupFor = func(app *core.App) remoteLookup { return app }

// nameTakenError is a create, rename or duplicate refused because its target
// name already exists.
type nameTakenError struct{ name string }

func (e *nameTakenError) Error() string { return e.name + " already exists here" }

// listDirNames returns every entry name in dir, hidden ones included, and
// whether the target matches names regardless of case. server "" is local.
func listDirNames(look remoteLookup, server string, style core.TargetPathStyle, dir string) ([]string, bool, error) {
	if server == "" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, false, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names, style.IsWindows(), nil
	}
	res, err := look.ListRemoteDirHidden(server, dir, true)
	if err != nil {
		return nil, false, err
	}
	names := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		names = append(names, e.Name)
	}
	fold := style.IsWindows() || (res.CaseInsensitive != nil && *res.CaseInsensitive)
	return names, fold, nil
}

// nameTaken reports whether name exists in dir. except is an entry that may
// match: the source of a rename, which on a case-insensitive filesystem is
// what its case-only new name resolves to.
//
// Locally an Lstat answers, so the filesystem's own case and normalisation
// rules apply. Remotely a successful stat proves the name exists; the agent
// reports every stat failure alike, so absence is taken only from a listing
// with hidden entries, and a listing that fails is an error, not a yes.
func nameTaken(look remoteLookup, server string, style core.TargetPathStyle, dir, name, except string) (bool, error) {
	target := style.Join(dir, name)
	if server == "" {
		fi, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if except != "" {
			if src, err := os.Lstat(style.Join(dir, except)); err == nil && os.SameFile(fi, src) {
				return false, nil
			}
		}
		return true, nil
	}
	caseOnly := except != "" && strings.EqualFold(name, except)
	if _, err := look.StatRemoteFile(server, target); err == nil && !caseOnly {
		return true, nil
	}
	names, fold, err := listDirNames(look, server, style, dir)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == except {
			continue
		}
		if n == name || (fold && strings.EqualFold(n, name)) {
			return true, nil
		}
	}
	return false, nil
}

// ensureNameFree returns nil when nothing named name exists in dir, a
// *nameTakenError when something does, or why it could not be checked.
func ensureNameFree(look remoteLookup, server string, style core.TargetPathStyle, dir, name, except string) error {
	taken, err := nameTaken(look, server, style, dir, name, except)
	if err != nil {
		return fmt.Errorf("could not check whether %s already exists: %w", name, err)
	}
	if taken {
		return &nameTakenError{name: name}
	}
	return nil
}

// pickDuplicateName chooses a free "<base> copy.<ext>" name for a copy of
// name in dir, checked against the directory itself.
func pickDuplicateName(look remoteLookup, server string, style core.TargetPathStyle, dir, name string) (string, error) {
	names, _, err := listDirNames(look, server, style, dir)
	if err != nil {
		return "", fmt.Errorf("could not list the folder: %w", err)
	}
	existing := make(map[string]bool, len(names))
	for _, n := range names {
		existing[n] = true
	}
	// The listing settles almost every case; the check catches names the
	// target treats as equal to a listed one (case, Unicode normalisation).
	for range 16 {
		candidate := duplicateName(name, existing, style)
		err := ensureNameFree(look, server, style, dir, candidate, "")
		var taken *nameTakenError
		if errors.As(err, &taken) {
			existing[candidate] = true
			continue
		}
		return candidate, err
	}
	return "", fmt.Errorf("could not find a free name for a copy of %s", name)
}
