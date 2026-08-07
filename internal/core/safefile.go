// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AtomicLocalFile is a file being assembled under an opened destination root.
// Commit atomically replaces the destination directory entry, so a symlink
// planted at the final path is replaced rather than followed.
type AtomicLocalFile struct {
	root     *os.Root
	file     *os.File
	tempRel  string
	finalRel string
	ownsRoot bool
	closed   bool
}

// File returns the descriptor used to assemble the destination.
func (f *AtomicLocalFile) File() *os.File { return f.file }

// Commit syncs and closes the sidecar, then atomically installs it at the final
// name. os.Root.Rename acts on the destination entry and never opens it.
func (f *AtomicLocalFile) Commit() error {
	if f == nil || f.closed {
		return fmt.Errorf("atomic destination is already closed")
	}
	if err := f.file.Sync(); err != nil {
		_ = f.close(false)
		return err
	}
	if err := f.file.Close(); err != nil {
		f.closed = true
		if f.ownsRoot {
			_ = f.root.Close()
		}
		return err
	}
	f.file = nil
	if err := f.root.Rename(f.tempRel, f.finalRel); err != nil {
		f.closed = true
		if f.ownsRoot {
			_ = f.root.Close()
		}
		return err
	}
	f.closed = true
	if f.ownsRoot {
		return f.root.Close()
	}
	return nil
}

// CloseKeep closes an incomplete sidecar but leaves it available for a resumed
// download. It always joins descriptor lifetime before returning.
func (f *AtomicLocalFile) CloseKeep() error { return f.close(false) }

// Abort closes and removes an incomplete sidecar. It is used for non-resumable
// local copy/archive writes so failed operations do not leave debris.
func (f *AtomicLocalFile) Abort() error { return f.close(true) }

func (f *AtomicLocalFile) close(remove bool) error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	var first error
	if f.file != nil {
		if err := f.file.Close(); err != nil {
			first = err
		}
		f.file = nil
	}
	if remove {
		if err := f.root.Remove(f.tempRel); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	if f.ownsRoot {
		if err := f.root.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// SafeTransferID reports whether id is a bounded, single ASCII component. IDs
// are used in resumable sidecar names and must never contribute path syntax.
func SafeTransferID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func randomLocalTransferID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// openVerifiedLocalDir opens path as a descriptor-backed root and verifies that
// the entry opened is the same non-symlink directory that was inspected. This
// closes the final-component lstat/open race for destination roots.
func openVerifiedLocalDir(path string, perm os.FileMode) (*os.Root, error) {
	clean := filepath.Clean(path)
	parent, base := filepath.Dir(clean), filepath.Base(clean)
	if clean == string(filepath.Separator) || base == "." {
		return os.OpenRoot(clean)
	}
	if err := os.MkdirAll(parent, perm); err != nil {
		return nil, err
	}
	pr, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	info, err := pr.Lstat(base)
	if os.IsNotExist(err) {
		if err := pr.Mkdir(base, perm); err != nil && !os.IsExist(err) {
			return nil, err
		}
		info, err = pr.Lstat(base)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("refusing non-directory or symlink destination root %q", clean)
	}
	r, err := pr.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	opened, err := r.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = r.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("destination root changed while opening %q", clean)
	}
	return r, nil
}

// OpenAtomicLocalFile opens a resumable atomic sidecar beside path.
func OpenAtomicLocalFile(path, transferID string, perm os.FileMode) (*AtomicLocalFile, error) {
	clean := filepath.Clean(path)
	root, err := openVerifiedLocalDir(filepath.Dir(clean), 0o750)
	if err != nil {
		return nil, err
	}
	f, err := openAtomicLocalFileInRoot(root, filepath.Base(clean), transferID, perm, true)
	if err != nil {
		_ = root.Close()
	}
	return f, err
}

// OpenAtomicLocalFileUnder opens a resumable atomic sidecar at rel beneath base.
// Every intermediate lookup is performed by os.Root, which rejects symlink
// traversal outside base even if entries are swapped concurrently.
func OpenAtomicLocalFileUnder(base, rel, transferID string, perm os.FileMode) (*AtomicLocalFile, error) {
	if !safeRel(rel) {
		return nil, fmt.Errorf("refusing unsafe destination path %q", rel)
	}
	root, err := openVerifiedLocalDir(base, 0o750)
	if err != nil {
		return nil, err
	}
	f, err := openAtomicLocalFileInRoot(root, filepath.Clean(rel), transferID, perm, true)
	if err != nil {
		_ = root.Close()
	}
	return f, err
}

func openAtomicLocalFileInRoot(root *os.Root, rel, transferID string, perm os.FileMode, ownsRoot bool) (*AtomicLocalFile, error) {
	if !safeRel(rel) || !SafeTransferID(transferID) {
		return nil, fmt.Errorf("invalid atomic destination or transfer id")
	}
	rel = filepath.Clean(rel)
	dir, base := filepath.Dir(rel), filepath.Base(rel)
	if !SafeComponent(base) {
		return nil, fmt.Errorf("invalid destination component %q", base)
	}
	if dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create destination directory: %w", err)
		}
	}
	tempBase := "." + base + ".fleet-" + transferID + ".part"
	if !SafeComponent(tempBase) {
		return nil, fmt.Errorf("invalid composed transfer path")
	}
	tempRel := filepath.Join(dir, tempBase)
	if info, err := root.Lstat(tempRel); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing unsafe resumable sidecar %q", tempRel)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := root.OpenFile(tempRel, os.O_RDWR|os.O_CREATE|oNoFollow, perm)
	if err != nil {
		return nil, fmt.Errorf("open atomic sidecar: %w", err)
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("atomic sidecar is not a regular file")
	}
	return &AtomicLocalFile{root: root, file: file, tempRel: tempRel, finalRel: rel, ownsRoot: ownsRoot}, nil
}

// openLocalRegular opens path through a verified parent descriptor, refuses
// symlinks and special files, and verifies the entry did not change between
// inspection and open.
func openLocalRegular(path string) (*os.File, os.FileInfo, error) {
	clean := filepath.Clean(path)
	root, err := openVerifiedLocalDir(filepath.Dir(clean), 0o750)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	rel := filepath.Base(clean)
	before, err := root.Lstat(rel)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("refusing to copy non-regular or symlink source %q", path)
	}
	file, err := root.OpenFile(rel, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("source changed while opening %q", path)
	}
	return file, after, nil
}

// CopyLocalFileAtomic copies a regular file and atomically replaces dst. Source
// symlinks are refused and the source is held by descriptor for the whole copy.
func CopyLocalFileAtomic(src, dst string, mode os.FileMode) error {
	in, info, err := openLocalRegular(src)
	if err != nil {
		return err
	}
	defer in.Close()
	id, err := randomLocalTransferID()
	if err != nil {
		return err
	}
	out, err := OpenAtomicLocalFile(dst, id, mode.Perm())
	if err != nil {
		return err
	}
	defer out.Abort()
	if _, err := io.Copy(out.File(), in); err != nil {
		return err
	}
	if err := out.File().Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	return out.Commit()
}

// CopyLocalTreeAtomic recursively merges src into dst. Both source and
// destination traversal are rooted at verified directory descriptors and every
// destination file is installed atomically.
func CopyLocalTreeAtomic(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.IsDir() {
		return fmt.Errorf("refusing non-directory or symlink source %q", src)
	}
	srcRoot, err := openVerifiedLocalDir(src, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	dstRoot, err := openVerifiedLocalDir(dst, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	return copyLocalTreeRoot(srcRoot, dstRoot, ".")
}

func copyLocalTreeRoot(srcRoot, dstRoot *os.Root, dir string) error {
	dirFile, err := srcRoot.Open(dir)
	if err != nil {
		return err
	}
	entries, err := dirFile.ReadDir(-1)
	closeErr := dirFile.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		rel := entry.Name()
		if dir != "." {
			rel = filepath.Join(dir, rel)
		}
		info, err := srcRoot.Lstat(rel)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in local copy tree: %s", rel)
		}
		if info.IsDir() {
			if err := dstRoot.MkdirAll(rel, info.Mode().Perm()); err != nil {
				return err
			}
			if err := copyLocalTreeRoot(srcRoot, dstRoot, rel); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular file in local copy tree: %s", rel)
		}
		in, err := srcRoot.OpenFile(rel, os.O_RDONLY|oNoFollow, 0)
		if err != nil {
			return err
		}
		opened, err := in.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			_ = in.Close()
			if err != nil {
				return err
			}
			return fmt.Errorf("source changed while opening %q", rel)
		}
		id, err := randomLocalTransferID()
		if err != nil {
			_ = in.Close()
			return err
		}
		out, err := openAtomicLocalFileInRoot(dstRoot, rel, id, opened.Mode().Perm(), false)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out.File(), in)
		closeErr := in.Close()
		if copyErr != nil || closeErr != nil {
			_ = out.Abort()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		if err := out.File().Chmod(opened.Mode().Perm()); err != nil {
			_ = out.Abort()
			return err
		}
		if err := out.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// RemoveLocalUnder removes rel beneath base using a descriptor-backed root.
// Removing a symlink acts on the link entry and never dereferences its target.
func RemoveLocalUnder(base, rel string, recursive bool) error {
	if !safeRel(rel) {
		return fmt.Errorf("refusing unsafe removal path %q", rel)
	}
	root, err := openVerifiedLocalDir(base, 0o750)
	if err != nil {
		return err
	}
	defer root.Close()
	if recursive {
		return root.RemoveAll(filepath.Clean(rel))
	}
	return root.Remove(filepath.Clean(rel))
}

// CreateAtomicLocalFile opens a one-shot atomic destination with a random
// sidecar identifier. Callers should Commit on success or Abort on failure.
func CreateAtomicLocalFile(path string, perm os.FileMode) (*AtomicLocalFile, error) {
	id, err := randomLocalTransferID()
	if err != nil {
		return nil, err
	}
	return OpenAtomicLocalFile(path, id, perm)
}
