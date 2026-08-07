// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// blockedTransferPrefixes lists OS virtual filesystems that must never be read
// from or written to through file transfer (or log.read), even by an
// authenticated controller. Reading /proc/N/mem leaks process memory; writing
// under /sys or /dev can damage the host. checkBlockedTransferPath enforces
// both this block list and the --file-root sandbox for every file.* op and for
// log.read.
var blockedTransferPrefixes = []string{"/proc/", "/sys/", "/dev/"}

// FileManager is the agent-side surface for browsing and transferring files.
// Each method validates and resolves its path before touching the filesystem.
type FileManager interface {
	List(context.Context, proto.FileListPayload) (proto.FileListResult, error)
	Stat(context.Context, proto.FileStatPayload) (proto.FileStatResult, error)
	Read(context.Context, proto.FileReadPayload) (proto.FileReadResult, error)
	OpenWrite(context.Context, proto.FileOpenWritePayload) (proto.FileOpenWriteResult, error)
	Write(context.Context, proto.FileWritePayload) (proto.FileWriteResult, error)
	Finalize(context.Context, proto.FileFinalizePayload) (proto.FileFinalizeResult, error)
	Probe(context.Context, proto.FileProbePayload) (proto.FileProbeResult, error)
	Mkdir(context.Context, proto.FileMkdirPayload) (proto.FileOpResult, error)
	Delete(context.Context, proto.FileDeletePayload) (proto.FileOpResult, error)
	Rename(context.Context, proto.FileRenamePayload) (proto.FileOpResult, error)
}

// activeUpload tracks one in-flight upload. Its temp file is opened once and
// shared across every parallel fleet-rpc channel. WriteAt with disjoint offsets
// is safe to call concurrently on one *os.File, so writers take mu.RLock (they
// run in parallel); Finalize takes mu.Lock so it cannot close the file while a
// write is mid-flight on another channel. `done` guards against a late write
// arriving after finalize.
type activeUpload struct {
	mu        sync.RWMutex
	f         *os.File
	root      *os.Root
	tempPath  string
	finalPath string
	tempRel   string
	finalRel  string
	totalSize int64
	mode      uint32
	done      bool
}

type fileManager struct {
	mu     sync.Mutex
	active map[string]*activeUpload
}

// The default file manager holds upload state (the `active` map) that must
// survive across separate RPC calls — open_write, the parallel writes, and
// finalize each arrive as independent envelopes, possibly on different
// channels. So the default is a process-wide singleton, unlike the stateless
// service/log/metrics managers.
var (
	defaultFileMgr     FileManager
	defaultFileMgrOnce sync.Once
)

func defaultFileManager() FileManager {
	defaultFileMgrOnce.Do(func() {
		defaultFileMgr = NewFileManager()
	})
	return defaultFileMgr
}

// NewFileManager returns a fresh, independent file manager. The agent uses a
// process-wide singleton (defaultFileManager); tests use this to get isolated
// upload state.
func NewFileManager() FileManager {
	return &fileManager{active: make(map[string]*activeUpload)}
}

// validateTransferPath resolves an EXISTING path (read/stat/list/delete/rename
// source). It requires an absolute path, resolves symlinks so a symlink to
// /proc/1/mem cannot bypass the block list, and rejects the OS pseudo
// filesystems. The resolved real path is returned and should be the one opened
// to eliminate the TOCTOU window — identical to fileLogReader.Read.
func validateTransferPath(path string) (string, *RPCError) {
	if path == "" {
		return "", &RPCError{Code: "missing_path", Message: "path is required"}
	}
	if !filepath.IsAbs(path) {
		return "", &RPCError{Code: "invalid_path", Message: "path must be absolute"}
	}
	real := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(real); err == nil {
		real = resolved
	}
	if err := checkBlockedTransferPath(real); err != nil {
		return "", err
	}
	return real, nil
}

// validateWriteTarget resolves a path that may NOT exist yet (upload
// destination, mkdir, rename target). The final component can't always be
// resolved, so we resolve the parent directory's symlinks and rejoin — that
// prevents a symlinked parent (e.g. /tmp/x -> /proc) from escaping the block
// list.
//
// Crucially we also lstat the FINAL component: if an attacker has pre-planted a
// symlink AT the destination (e.g. /root/upload -> /etc/cron.d/evil), the join
// above would validate the symlink's own path (inside the sandbox) while the
// subsequent write/mkdir/rename follows the link to an arbitrary location. So
// when the final component is itself a symlink we fully resolve it and
// re-validate the real target; callers that perform the actual write should
// also use O_NOFOLLOW where they can to close the residual TOCTOU window.
func validateWriteTarget(path string) (string, *RPCError) {
	if path == "" {
		return "", &RPCError{Code: "missing_path", Message: "path is required"}
	}
	if !filepath.IsAbs(path) {
		return "", &RPCError{Code: "invalid_path", Message: "path must be absolute"}
	}
	clean := filepath.Clean(path)
	dir := filepath.Dir(clean)
	realDir := dir
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		realDir = resolved
	}
	real := filepath.Join(realDir, filepath.Base(clean))
	if err := checkBlockedTransferPath(real); err != nil {
		return "", err
	}
	// If the final component already exists and is a symlink, resolve it and
	// re-validate the real target — a planted symlink must not let a write escape
	// the sandbox even though `real` itself sits inside it.
	if info, err := os.Lstat(real); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(real)
		if err != nil {
			return "", &RPCError{Code: "invalid_path", Message: "target symlink could not be resolved"}
		}
		if err := checkBlockedTransferPath(resolved); err != nil {
			return "", err
		}
	}
	return real, nil
}

// recheckFinalComponent re-validates a path's final component immediately before
// a destructive/creating syscall that itself follows symlinks (os.MkdirAll,
// os.Rename, os.Remove*). validateWriteTarget/validateTransferPath already Lstat
// the final component, but a local attacker can swap a benign target for a
// symlink in the window between that check and the syscall (a TOCTOU). The
// upload write path closes this with O_NOFOLLOW; MkdirAll/Rename/RemoveAll have
// no such flag, so we re-Lstat here and refuse if the final component is now a
// symlink that resolves outside the allowed roots (or into a blocked prefix).
//
// This shrinks the TOCTOU window to the few instructions between this Lstat and
// the syscall; it does not eliminate it, but it removes the practical, planted-
// symlink-at-rest escape that the one-time validate misses. A symlink that
// stays inside the sandbox is left alone (legitimate intra-sandbox links).
func recheckFinalComponent(real string) *RPCError {
	info, err := os.Lstat(real)
	if err != nil {
		// Doesn't exist (normal for a fresh mkdir/rename target) or not
		// stat-able — nothing planted to follow, let the syscall proceed/fail.
		return nil
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil // not a symlink — no escape via the final component
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		return &RPCError{Code: "invalid_path", Message: "target symlink could not be resolved"}
	}
	return checkBlockedTransferPath(resolved)
}

// validateTransferEntryPath validates an existing directory entry without
// dereferencing its final component. Delete and rename must act on a symlink
// itself, not on the file or directory it points to.
func validateTransferEntryPath(name string) (string, *RPCError) {
	if name == "" {
		return "", &RPCError{Code: "missing_path", Message: "path is required"}
	}
	if !filepath.IsAbs(name) {
		return "", &RPCError{Code: "invalid_path", Message: "path must be absolute"}
	}
	clean := filepath.Clean(name)
	parent := filepath.Dir(clean)
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	real := filepath.Join(parent, filepath.Base(clean))
	if err := checkBlockedTransferPath(real); err != nil {
		return "", err
	}
	return real, nil
}

// openTransferRoot returns a descriptor-backed root and a relative path beneath
// it. With --file-root configured, opening from the allowed root closes the
// validate/open TOCTOU for every intermediate component.
func openTransferRoot(real string) (*os.Root, string, *RPCError) {
	var rootPath string
	for _, candidate := range allowedFileRoots {
		if real == candidate || strings.HasPrefix(real, candidate+string(filepath.Separator)) {
			if len(candidate) > len(rootPath) {
				rootPath = candidate
			}
		}
	}
	if rootPath == "" {
		rootPath = filepath.VolumeName(real) + string(filepath.Separator)
	}
	rel, err := filepath.Rel(rootPath, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", &RPCError{Code: "invalid_path", Message: "path is outside the selected file root"}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", &RPCError{Code: "open_failed", Message: err.Error()}
	}
	return root, rel, nil
}

func validTransferID(id string) bool {
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

func invalidTransferID() *RPCError {
	return &RPCError{Code: "invalid_transfer_id", Message: "transfer_id must be a bounded safe path component"}
}

func checkBlockedTransferPath(real string) *RPCError {
	for _, blocked := range blockedTransferPrefixes {
		if real == strings.TrimSuffix(blocked, "/") || strings.HasPrefix(real, blocked) {
			return &RPCError{
				Code:    "invalid_path",
				Message: fmt.Sprintf("access to %s is not permitted", blocked),
			}
		}
	}
	if !withinAllowedRoots(real) {
		return &RPCError{
			Code:    "invalid_path",
			Message: "path is outside the agent's allowed file roots",
		}
	}
	return nil
}

// allowedFileRoots, when non-empty, confines every file operation to paths
// within one of these resolved roots — a defense-in-depth sandbox so a
// controller (or a stolen controller key) cannot read or write arbitrary paths
// on the agent host. Set once at startup via SetAllowedFileRoots before serving;
// read-only afterwards.
var allowedFileRoots []string

// SetAllowedFileRoots configures the file-operation sandbox. Each root is made
// absolute and symlink-resolved. An empty list (the default) imposes no limit.
// Call before the agent begins serving.
func SetAllowedFileRoots(roots []string) {
	resolved := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs := filepath.Clean(r)
		if !filepath.IsAbs(abs) {
			if a, err := filepath.Abs(abs); err == nil {
				abs = a
			}
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		resolved = append(resolved, abs)
	}
	allowedFileRoots = resolved
}

// withinAllowedRoots reports whether real is inside the configured sandbox (or
// no sandbox is configured).
func withinAllowedRoots(real string) bool {
	if len(allowedFileRoots) == 0 {
		return true
	}
	for _, root := range allowedFileRoots {
		if real == root || strings.HasPrefix(real, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// stalePartMaxAge is how old a leftover upload temp must be before a new upload
// to the same directory reaps it.
const stalePartMaxAge = 24 * time.Hour

// reapStaleParts removes abandoned upload temp files (<name>.fleet-<id>.part) in
// dir older than stalePartMaxAge, except keepName (the current transfer). It is
// best-effort and runs lazily when a new upload to dir begins, so orphaned temps
// from interrupted transfers don't accumulate.
func reapStaleParts(dir, keepName string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == keepName || !strings.Contains(name, ".fleet-") || !strings.HasSuffix(name, ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > stalePartMaxAge {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func reapStalePartsRoot(root *os.Root, dirRel, keepName string, now time.Time) {
	dir, err := root.Open(dirRel)
	if err != nil {
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == keepName || !strings.Contains(name, ".fleet-") || !strings.HasSuffix(name, ".part") {
			continue
		}
		info, err := entry.Info()
		if err == nil && now.Sub(info.ModTime()) > stalePartMaxAge {
			_ = root.Remove(filepath.Join(dirRel, name))
		}
	}
}

func (m *fileManager) List(_ context.Context, p proto.FileListPayload) (proto.FileListResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileListResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileListResult{}, rerr
	}
	defer root.Close()
	dir, err := root.Open(rel)
	if err != nil {
		return proto.FileListResult{}, &RPCError{Code: "list_failed", Message: err.Error()}
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return proto.FileListResult{}, &RPCError{Code: "list_failed", Message: err.Error()}
	}
	result := proto.FileListResult{Path: real, Entries: make([]proto.FileEntry, 0, len(entries))}
	for _, entry := range entries {
		name := entry.Name()
		if !p.ShowHidden && strings.HasPrefix(name, ".") {
			continue
		}
		fe := proto.FileEntry{
			Name:  name,
			Path:  filepath.Join(real, name),
			IsDir: entry.IsDir(),
		}
		if info, err := entry.Info(); err == nil {
			fe.Size = info.Size()
			fe.Mode = uint32(info.Mode().Perm())
			fe.ModTime = info.ModTime().UTC()
			fe.IsSymlink = info.Mode()&os.ModeSymlink != 0
		}
		result.Entries = append(result.Entries, fe)
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // directories first
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return result, nil
}

func (m *fileManager) Stat(_ context.Context, p proto.FileStatPayload) (proto.FileStatResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileStatResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileStatResult{}, rerr
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil {
		return proto.FileStatResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	return proto.FileStatResult{Entry: proto.FileEntry{
		Name:    info.Name(),
		Path:    real,
		Size:    info.Size(),
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   info.IsDir(),
		ModTime: info.ModTime().UTC(),
	}}, nil
}

func (m *fileManager) Read(_ context.Context, p proto.FileReadPayload) (proto.FileReadResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileReadResult{}, rerr
	}
	if p.Offset < 0 {
		return proto.FileReadResult{}, &RPCError{Code: "invalid_offset", Message: "offset must be non-negative"}
	}
	length := p.Length
	if length <= 0 || length > proto.MaxRawChunkBytes {
		length = proto.MaxRawChunkBytes
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileReadResult{}, rerr
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return proto.FileReadResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return proto.FileReadResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	if p.Offset >= info.Size() {
		return proto.FileReadResult{Offset: p.Offset, Length: 0, EOF: true}, nil
	}
	// Overflow-safe clamp (p.Offset < info.Size() here, so the subtraction is
	// non-negative and p.Offset+length can't be relied upon — it may overflow).
	if length > info.Size()-p.Offset {
		length = info.Size() - p.Offset
	}
	buf := make([]byte, length)
	n, err := file.ReadAt(buf, p.Offset)
	if err != nil && err != io.EOF {
		return proto.FileReadResult{}, &RPCError{Code: "read_failed", Message: err.Error()}
	}
	buf = buf[:n]
	sum := sha256.Sum256(buf)
	return proto.FileReadResult{
		Offset: p.Offset,
		Length: int64(n),
		Data:   buf,
		SHA256: hex.EncodeToString(sum[:]),
		EOF:    p.Offset+int64(n) >= info.Size(),
	}, nil
}

func (m *fileManager) OpenWrite(_ context.Context, p proto.FileOpenWritePayload) (proto.FileOpenWriteResult, error) {
	if !validTransferID(p.TransferID) {
		return proto.FileOpenWriteResult{}, invalidTransferID()
	}
	if p.TotalSize < 0 {
		return proto.FileOpenWriteResult{}, &RPCError{Code: "invalid_size", Message: "total size must be non-negative"}
	}
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileOpenWriteResult{}, rerr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.active[p.TransferID]; ok {
		if existing.finalPath != real || existing.totalSize != p.TotalSize {
			return proto.FileOpenWriteResult{}, &RPCError{Code: "transfer_conflict", Message: "transfer_id is already bound to a different destination or size"}
		}
		var size int64
		if info, err := existing.f.Stat(); err == nil {
			size = info.Size()
		}
		return proto.FileOpenWriteResult{TempPath: existing.tempPath, ResumeOffset: size}, nil
	}

	root, finalRel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileOpenWriteResult{}, rerr
	}
	tempRel := finalRel + ".fleet-" + p.TransferID + ".part"
	if filepath.Dir(tempRel) != filepath.Dir(finalRel) || !validTransferID(p.TransferID) {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, invalidTransferID()
	}
	temp := filepath.Join(root.Name(), tempRel)
	if err := checkBlockedTransferPath(temp); err != nil {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, err
	}
	reapStalePartsRoot(root, filepath.Dir(finalRel), filepath.Base(tempRel), time.Now())
	if info, err := root.Lstat(tempRel); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			_ = root.Close()
			return proto.FileOpenWriteResult{}, &RPCError{Code: "invalid_path", Message: "upload sidecar is not a regular file"}
		}
	} else if !os.IsNotExist(err) {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	f, err := root.OpenFile(tempRel, os.O_RDWR|os.O_CREATE|oNoFollow, 0o600)
	if err != nil {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		_ = root.Close()
		if err == nil {
			err = fmt.Errorf("upload sidecar is not a regular file")
		}
		return proto.FileOpenWriteResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	mode := p.Mode
	if mode == 0 {
		mode = 0o644
	}
	m.active[p.TransferID] = &activeUpload{
		f: f, root: root, tempPath: temp, finalPath: real,
		tempRel: tempRel, finalRel: finalRel,
		totalSize: p.TotalSize, mode: mode,
	}
	return proto.FileOpenWriteResult{TempPath: temp, ResumeOffset: info.Size()}, nil
}

func (m *fileManager) Write(_ context.Context, p proto.FileWritePayload) (proto.FileWriteResult, error) {
	if !validTransferID(p.TransferID) {
		return proto.FileWriteResult{}, invalidTransferID()
	}
	if len(p.Data) > proto.MaxRawChunkBytes {
		return proto.FileWriteResult{}, &RPCError{
			Code:    "chunk_too_large",
			Message: fmt.Sprintf("chunk of %d bytes exceeds max %d", len(p.Data), proto.MaxRawChunkBytes),
		}
	}
	if p.Offset < 0 {
		return proto.FileWriteResult{}, &RPCError{Code: "invalid_offset", Message: "offset must be non-negative"}
	}
	m.mu.Lock()
	au, ok := m.active[p.TransferID]
	m.mu.Unlock()
	if !ok {
		return proto.FileWriteResult{}, &RPCError{Code: "unknown_transfer", Message: "no open upload for transfer id; call file.open_write first"}
	}
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileWriteResult{}, rerr
	}
	if real != au.finalPath {
		return proto.FileWriteResult{}, &RPCError{Code: "transfer_conflict", Message: "transfer path does not match open upload"}
	}
	// Bound the write to the size declared at open_write. Without this a client
	// could write at an arbitrary offset (e.g. 1 PiB) and allocate an enormous
	// sparse file; with TotalSize=0 it also blocks any non-empty write.
	//
	// Overflow-safe: a hostile Offset near MaxInt64 makes Offset+len wrap negative,
	// which would slip past a naive `Offset+len > totalSize` check. len(p.Data) is
	// already bounded by MaxRawChunkBytes above and Offset>=0 was checked, so
	// totalSize-len can't underflow; compare Offset against (totalSize - len)
	// instead of forming the sum. Equivalent to Offset+len > totalSize when no
	// overflow.
	dataLen := int64(len(p.Data))
	if dataLen < 0 || p.Offset > au.totalSize-dataLen {
		return proto.FileWriteResult{}, &RPCError{Code: "offset_out_of_range", Message: "write extends beyond the declared file size"}
	}
	if p.SHA256 != "" {
		sum := sha256.Sum256(p.Data)
		if hex.EncodeToString(sum[:]) != p.SHA256 {
			return proto.FileWriteResult{}, &RPCError{Code: "checksum_mismatch", Message: "chunk checksum mismatch"}
		}
	}
	// RLock lets parallel writers run concurrently (their ranges are disjoint)
	// while excluding Finalize, which closes the file under a full Lock.
	au.mu.RLock()
	defer au.mu.RUnlock()
	if au.done {
		return proto.FileWriteResult{}, &RPCError{Code: "upload_finalized", Message: "upload has already been finalized"}
	}
	n, err := au.f.WriteAt(p.Data, p.Offset)
	if err != nil {
		return proto.FileWriteResult{}, &RPCError{Code: "write_failed", Message: err.Error()}
	}
	return proto.FileWriteResult{Offset: p.Offset, BytesWritten: int64(n)}, nil
}

func (m *fileManager) Finalize(_ context.Context, p proto.FileFinalizePayload) (proto.FileFinalizeResult, error) {
	if !validTransferID(p.TransferID) {
		return proto.FileFinalizeResult{}, invalidTransferID()
	}
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	m.mu.Lock()
	au, ok := m.active[p.TransferID]
	if !ok {
		m.mu.Unlock()
		return proto.FileFinalizeResult{}, &RPCError{Code: "unknown_transfer", Message: "no open upload for transfer id"}
	}
	if real != au.finalPath || p.TotalSize != au.totalSize {
		m.mu.Unlock()
		return proto.FileFinalizeResult{}, &RPCError{Code: "transfer_conflict", Message: "finalize path or size does not match open upload"}
	}
	delete(m.active, p.TransferID)
	m.mu.Unlock()
	defer au.root.Close()

	au.mu.Lock()
	defer au.mu.Unlock()
	// Block any straggler write that already grabbed this au before we deleted
	// it from the map: once finalize owns the lock, the file is being closed.
	au.done = true

	if err := au.f.Sync(); err != nil {
		_ = au.f.Close()
		return proto.FileFinalizeResult{}, &RPCError{Code: "sync_failed", Message: err.Error()}
	}
	info, err := au.f.Stat()
	if err != nil {
		_ = au.f.Close()
		return proto.FileFinalizeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	if info.Size() != p.TotalSize {
		_ = au.f.Close()
		return proto.FileFinalizeResult{}, &RPCError{
			Code:    "size_mismatch",
			Message: fmt.Sprintf("expected %d bytes, assembled %d", p.TotalSize, info.Size()),
		}
	}

	sum := p.WholeSHA256
	if p.WholeSHA256 != "" {
		if _, err := au.f.Seek(0, io.SeekStart); err != nil {
			_ = au.f.Close()
			return proto.FileFinalizeResult{}, &RPCError{Code: "seek_failed", Message: err.Error()}
		}
		h := sha256.New()
		if _, err := io.Copy(h, au.f); err != nil {
			_ = au.f.Close()
			return proto.FileFinalizeResult{}, &RPCError{Code: "read_failed", Message: err.Error()}
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != p.WholeSHA256 {
			_ = au.f.Close()
			return proto.FileFinalizeResult{}, &RPCError{Code: "whole_checksum_mismatch", Message: "assembled file checksum does not match source"}
		}
	}

	mode := au.mode
	if p.Mode != 0 {
		mode = p.Mode
	}
	_ = au.f.Chmod(os.FileMode(mode)) // best effort; some filesystems disallow
	if err := au.f.Close(); err != nil {
		return proto.FileFinalizeResult{}, &RPCError{Code: "close_failed", Message: err.Error()}
	}
	if err := au.root.Rename(au.tempRel, au.finalRel); err != nil {
		return proto.FileFinalizeResult{}, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	return proto.FileFinalizeResult{Path: au.finalPath, Size: info.Size(), SHA256: sum}, nil
}

func (m *fileManager) Probe(_ context.Context, p proto.FileProbePayload) (proto.FileProbeResult, error) {
	if p.TransferID != "" && !validTransferID(p.TransferID) {
		return proto.FileProbeResult{}, invalidTransferID()
	}
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileProbeResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileProbeResult{}, rerr
	}
	defer root.Close()
	probeRel := rel
	if p.TransferID != "" {
		tempRel := rel + ".fleet-" + p.TransferID + ".part"
		if filepath.Dir(tempRel) != filepath.Dir(rel) {
			return proto.FileProbeResult{}, invalidTransferID()
		}
		if info, err := root.Lstat(tempRel); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			probeRel = tempRel
		} else if err != nil && !os.IsNotExist(err) {
			return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
		}
	}

	info, err := root.Stat(probeRel)
	if err != nil {
		if os.IsNotExist(err) {
			return proto.FileProbeResult{Exists: false}, nil
		}
		return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	result := proto.FileProbeResult{Exists: true, CurrentSize: info.Size()}
	if len(p.Ranges) > 0 {
		const maxProbeRanges = 65536
		ranges := p.Ranges
		if len(ranges) > maxProbeRanges {
			ranges = ranges[:maxProbeRanges]
		}
		file, err := root.Open(probeRel)
		if err != nil {
			return proto.FileProbeResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
		}
		defer file.Close()
		for _, r := range ranges {
			if r.Length <= 0 || r.Length > proto.MaxRawChunkBytes || r.Offset < 0 || r.Length > info.Size() || r.Offset > info.Size()-r.Length {
				continue
			}
			buf := make([]byte, r.Length)
			if _, err := file.ReadAt(buf, r.Offset); err != nil {
				continue
			}
			sum := sha256.Sum256(buf)
			result.RangeChecksums = append(result.RangeChecksums, proto.FileRangeChecksum{
				Offset: r.Offset, Length: r.Length, SHA256: hex.EncodeToString(sum[:]),
			})
		}
	}
	return result, nil
}

func (m *fileManager) Mkdir(_ context.Context, p proto.FileMkdirPayload) (proto.FileOpResult, error) {
	real, rerr := validateWriteTarget(p.Path)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	mode := p.Mode
	if mode == 0 {
		mode = 0o755
	}
	// Re-validate the final component right before MkdirAll. validateWriteTarget
	// resolved the parent's symlinks, but MkdirAll follows a symlink planted AT
	// `real` (or swapped in after that check) and would create/return a directory
	// outside the sandbox. recheckFinalComponent refuses a final symlink that
	// escapes the allowed roots, shrinking the TOCTOU window MkdirAll can't close
	// itself (it has no O_NOFOLLOW equivalent).
	if rerr := recheckFinalComponent(real); rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	defer root.Close()
	if err := root.MkdirAll(rel, os.FileMode(mode)); err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "mkdir_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: real}, nil
}

func (m *fileManager) Delete(_ context.Context, p proto.FileDeletePayload) (proto.FileOpResult, error) {
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	defer root.Close()
	var err error
	if p.Recursive {
		err = root.RemoveAll(rel)
	} else {
		err = root.Remove(rel)
	}
	if err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "delete_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: real}, nil
}

func (m *fileManager) Rename(_ context.Context, p proto.FileRenamePayload) (proto.FileOpResult, error) {
	from, rerr := validateTransferEntryPath(p.From)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	to, rerr := validateTransferEntryPath(p.To)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	fromRoot, fromRel, rerr := openTransferRoot(from)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	defer fromRoot.Close()
	toRoot, toRel, rerr := openTransferRoot(to)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	defer toRoot.Close()
	if fromRoot.Name() != toRoot.Name() {
		return proto.FileOpResult{}, &RPCError{Code: "rename_failed", Message: "cross-root rename is not permitted"}
	}
	if err := fromRoot.Rename(fromRel, toRel); err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: to}, nil
}
