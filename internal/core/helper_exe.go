// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// On Windows a running executable cannot be replaced or deleted, and the
// helper is always running, so a winget upgrade or uninstall of fleet.exe
// would fail with "file in use". There the helper runs from a copy of the
// binary in data/helper-bin and watches the installed one, exiting when it
// changes, as it does everywhere else; the next command then starts it from
// a fresh copy. Elsewhere a running binary can be replaced, so it runs the
// installed one directly.
var helperRunsFromCopy = runtime.GOOS == "windows"

// HelperSourceExeEnv names the installed binary for a helper that runs from a
// copy, so it watches that file for an upgrade instead of its own copy. It
// only decides when the helper exits.
const HelperSourceExeEnv = "FLEET_HELPER_SOURCE_EXE"

const helperCopyPrefix = "fleet-helper-"

// helperExecutableCopy returns a copy of src in configDir/data/helper-bin,
// named by its content hash, making it if there is none, and removes older
// copies that are not in use.
func helperExecutableCopy(configDir, src string) (string, error) {
	dir := filepath.Join(configDir, "data", "helper-bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	sum, err := fileSHA256(src)
	if err != nil {
		return "", err
	}
	name := helperCopyPrefix + sum[:16] + filepath.Ext(src)
	dst := filepath.Join(dir, name)
	if have, err := fileSHA256(dst); err != nil || have != sum {
		if err := copyExecutable(src, dst, dir); err != nil {
			return "", err
		}
		if have, err := fileSHA256(dst); err != nil || have != sum {
			return "", fmt.Errorf("the helper's copy of %s does not match it", src)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != name && strings.HasPrefix(e.Name(), helperCopyPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name())) // fails, harmlessly, for one still running
		}
	}
	return dst, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the fleet binary, or the helper's copy of it in the config dir
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyExecutable(src, dst, dir string) error {
	in, err := os.Open(src) // #nosec G304 -- the fleet binary itself
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".helper-copy-*")
	if err != nil {
		return fmt.Errorf("copy the fleet binary for the helper: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy the fleet binary for the helper: %w", err)
	}
	if err := tmp.Chmod(0o700); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("copy the fleet binary for the helper: %w", err)
	}
	return nil
}
