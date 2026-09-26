// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
	Put(context.Context, proto.FilePutPayload) (proto.FileFinalizeResult, error)
	Copy(context.Context, proto.FileCopyPayload) (proto.FileFinalizeResult, error)
	Tree(context.Context, proto.FileTreePayload) (proto.FileTreeResult, error)
}

// fileCapabilities lists the optional file-transfer protocol features this
// agent implements. They are advertised in the hello so controllers only use
// them where supported.
func fileCapabilities() []string {
	return []string{
		proto.CapabilityFileChunkDigests,
		proto.CapabilityFilePut,
		proto.CapabilityFileReadStat,
		proto.CapabilityFileCopy,
		proto.CapabilityFileTree,
	}
}

// chunkRecord is a byte range of an upload's temp file whose content this
// process verified against a SHA-256 — either because it checked the chunk's
// checksum before writing it, or because it hashed the range from disk for a
// resume probe.
type chunkRecord struct {
	length int64
	sum    [sha256.Size]byte
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
	rawPath   string // the destination exactly as open_write received it
	tempRel   string
	finalRel  string
	totalSize int64
	mode      uint32
	done      bool
	// finalizing is set (under fileManager.mu) once a finalize or abort has
	// claimed the upload; open_write and a second finalize then answer
	// transfer_busy until it has finished.
	finalizing bool

	// records remembers every verified chunk (offset -> length/checksum) so
	// finalize can check the assembled file against the controller's chunk
	// list, and a resume can skip re-reading the prefix. Records never
	// overlap: a write drops any record it overlaps before touching the bytes.
	recMu   sync.Mutex
	records map[int64]chunkRecord
	// grid is the length of the first record. While every record is aligned
	// to it (the normal case: one controller, one chunk size) an overlapping
	// record can only live at the same offset, so the overlap check is O(1).
	grid    int64
	aligned bool

	// Writes whose byte ranges overlap are serialised (lockRange), so the
	// forget → WriteAt → record sequence of one can never interleave with
	// another's over the same bytes: a record always describes what the last
	// write to its range left on disk. Disjoint writes — every write a
	// well-behaved controller sends — still run in parallel.
	wmu      sync.Mutex
	wcond    *sync.Cond
	inflight []byteRange
}

type byteRange struct{ off, end int64 }

// lockRange waits until no other write to an overlapping range is in flight,
// then claims [off, off+n).
func (au *activeUpload) lockRange(off, n int64) {
	end := off + n
	au.wmu.Lock()
	defer au.wmu.Unlock()
	if au.wcond == nil {
		au.wcond = sync.NewCond(&au.wmu)
	}
	for au.overlapsInflightLocked(off, end) {
		au.wcond.Wait()
	}
	au.inflight = append(au.inflight, byteRange{off: off, end: end})
}

func (au *activeUpload) overlapsInflightLocked(off, end int64) bool {
	for _, r := range au.inflight {
		if off < r.end && r.off < end {
			return true
		}
	}
	return false
}

func (au *activeUpload) unlockRange(off, n int64) {
	end := off + n
	au.wmu.Lock()
	for i, r := range au.inflight {
		if r.off == off && r.end == end {
			au.inflight = append(au.inflight[:i], au.inflight[i+1:]...)
			break
		}
	}
	au.wmu.Unlock()
	au.wcond.Broadcast()
}

// testHookAfterWriteAt, when set by a test, runs between a chunk's WriteAt and
// the recording of its checksum.
var testHookAfterWriteAt func(offset int64)

func (au *activeUpload) dropOverlapsLocked(offset, length int64) {
	if len(au.records) == 0 {
		return
	}
	if au.aligned && au.grid > 0 && offset%au.grid == 0 && length <= au.grid {
		delete(au.records, offset)
		return
	}
	end := offset + length
	for off, rec := range au.records {
		if off < end && offset < off+rec.length {
			delete(au.records, off)
		}
	}
}

// record replaces whatever is known about [offset, offset+length) with a
// verified checksum.
func (au *activeUpload) record(offset, length int64, sum [sha256.Size]byte) {
	au.recMu.Lock()
	defer au.recMu.Unlock()
	au.dropOverlapsLocked(offset, length)
	if au.records == nil {
		au.records = make(map[int64]chunkRecord)
	}
	if len(au.records) == 0 {
		// The fast overlap check is only sound while every record starts on
		// a multiple of the grid, the first one included.
		au.grid, au.aligned = length, offset%length == 0
	} else if au.aligned && (au.grid <= 0 || offset%au.grid != 0 || length > au.grid) {
		au.aligned = false
	}
	au.records[offset] = chunkRecord{length: length, sum: sum}
}

// forget drops everything known about [offset, offset+length); used before
// bytes whose checksum was not verified are written there.
func (au *activeUpload) forget(offset, length int64) {
	au.recMu.Lock()
	au.dropOverlapsLocked(offset, length)
	au.recMu.Unlock()
}

func (au *activeUpload) lookup(offset, length int64) (string, bool) {
	au.recMu.Lock()
	defer au.recMu.Unlock()
	rec, ok := au.records[offset]
	if !ok || rec.length != length {
		return "", false
	}
	return hex.EncodeToString(rec.sum[:]), true
}

// intact reports whether the upload's temp file is still exactly what the agent
// created or vetted: owned by it, with a single link, and still reachable under
// its name. Returns the descriptor's current metadata.
func (au *activeUpload) intact() (os.FileInfo, bool) {
	info, err := au.f.Stat()
	if err != nil || !exclusivelyOwned(info) {
		return nil, false
	}
	cur, err := au.root.Lstat(au.tempRel)
	if err != nil || !sameInode(cur, info) {
		return nil, false
	}
	return info, true
}

// maxReportedChunks bounds FileOpenWriteResult.Chunks so the reply always fits
// in one envelope. Ranges beyond it are simply verified with file.probe.
const maxReportedChunks = 65536

// sortedRecords returns the records ordered by offset.
func (au *activeUpload) sortedRecords(limit int) []proto.FileRangeChecksum {
	au.recMu.Lock()
	defer au.recMu.Unlock()
	out := make([]proto.FileRangeChecksum, 0, min(len(au.records), max(limit, 0)))
	for off, rec := range au.records {
		out = append(out, proto.FileRangeChecksum{Offset: off, Length: rec.length, SHA256: hex.EncodeToString(rec.sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Offset < out[j].Offset })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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

// ---- buffers ----

// transferBufSize is the largest chunk the default controller moves in one
// frame. Read buffers up to this size are recycled; larger (legacy 8 MiB)
// requests fall back to a one-off allocation.
const transferBufSize = 2 * 1024 * 1024

var transferBufPool = sync.Pool{New: func() any {
	b := make([]byte, transferBufSize)
	return &b
}}

// ioBufSize is the buffer used for streaming hashes and copies. The 32 KiB
// io.Copy default costs a syscall per 32 KiB, which dominated re-hashing large
// files.
const ioBufSize = 1024 * 1024

var ioBufPool = sync.Pool{New: func() any {
	b := make([]byte, ioBufSize)
	return &b
}}

// hashRange streams length bytes at offset through SHA-256 with a pooled
// buffer. It never allocates a buffer the size of the range.
func hashRange(r io.ReaderAt, offset, length int64) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	bufp := ioBufPool.Get().(*[]byte)
	defer ioBufPool.Put(bufp)
	h := sha256.New()
	n, err := io.CopyBuffer(h, io.NewSectionReader(r, offset, length), *bufp)
	if err != nil {
		return sum, err
	}
	if n != length {
		return sum, io.ErrUnexpectedEOF
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// hashWorkers bounds how many ranges one probe hashes concurrently. Re-hashing
// a resumed prefix used to run on a single core.
func hashWorkers(n int) int {
	return max(1, min(n, runtime.GOMAXPROCS(0), 8))
}

// ---- path validation ----

// hasDotComponent reports whether a raw path names a "." or ".." component.
// Such paths are refused for every mutating operation BEFORE the path is
// cleaned: `rm -r /srv/app/../..` must never quietly become `rm -r /`. GNU rm
// refuses these operands for the same reason.
func hasDotComponent(p string) bool {
	start := 0
	for i := 0; i <= len(p); i++ {
		if i < len(p) && !os.IsPathSeparator(p[i]) && p[i] != '/' {
			continue
		}
		if part := p[start:i]; part == "." || part == ".." {
			return true
		}
		start = i + 1
	}
	return false
}

func dotComponentError() *RPCError {
	return &RPCError{Code: "invalid_path", Message: `path must not contain "." or ".." components`}
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
	if hasDotComponent(path) {
		return "", dotComponentError()
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
// itself, not on the file or directory it points to. It is used for every
// operation that creates, replaces or removes an entry, so it also refuses "."
// and ".." components before cleaning.
func validateTransferEntryPath(name string) (string, *RPCError) {
	if name == "" {
		return "", &RPCError{Code: "missing_path", Message: "path is required"}
	}
	if !filepath.IsAbs(name) {
		return "", &RPCError{Code: "invalid_path", Message: "path must be absolute"}
	}
	if hasDotComponent(name) {
		return "", dotComponentError()
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
func pathWithinFileRoot(root, real string) bool {
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func openTransferRoot(real string) (*os.Root, string, *RPCError) {
	var rootPath string
	for _, candidate := range allowedFileRoots {
		if pathWithinFileRoot(candidate, real) {
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

// transferBusy reports a transfer whose finalize is still running; the caller
// should retry shortly.
func transferBusy() *RPCError {
	return &RPCError{Code: "transfer_busy", Message: "a finalize for this transfer is in progress; retry shortly"}
}

func invalidTransferID() *RPCError {
	return &RPCError{Code: "invalid_transfer_id", Message: "transfer_id must be a bounded safe path component"}
}

// randomTempID names a one-shot temp file (file.put, file.copy) so concurrent
// writers to the same destination can never share one.
func randomTempID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
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

// firstAllowedFileRoot returns the first normalized sandbox root for
// advertisement in the agent hello payload. An empty result means file access
// is unrestricted and callers should advertise the native system root.
func firstAllowedFileRoot() string {
	if len(allowedFileRoots) == 0 {
		return ""
	}
	return allowedFileRoots[0]
}

// withinAllowedRoots reports whether real is inside the configured sandbox (or
// no sandbox is configured).
func withinAllowedRoots(real string) bool {
	if len(allowedFileRoots) == 0 {
		return true
	}
	for _, root := range allowedFileRoots {
		if pathWithinFileRoot(root, real) {
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

// reapInterval spaces out stale-temp sweeps of one directory. Every upload
// used to list its whole destination directory first, which made uploading
// many small files into one directory quadratic.
const reapInterval = 5 * time.Minute

var (
	reapMu   sync.Mutex
	reapLast = map[string]time.Time{}
)

// maybeReapStalePartsRoot runs reapStalePartsRoot at most once per
// reapInterval for a directory.
func maybeReapStalePartsRoot(root *os.Root, dirRel, keepName string, now time.Time) {
	key := filepath.Join(root.Name(), dirRel)
	reapMu.Lock()
	if last, ok := reapLast[key]; ok && now.Sub(last) < reapInterval {
		reapMu.Unlock()
		return
	}
	if len(reapLast) >= 4096 {
		for k, t := range reapLast {
			if now.Sub(t) >= reapInterval {
				delete(reapLast, k)
			}
		}
	}
	reapLast[key] = now
	reapMu.Unlock()
	reapStalePartsRoot(root, dirRel, keepName, now)
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

func fileEntryType(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return proto.FileEntryTypeSymlink
	case mode.IsDir():
		return proto.FileEntryTypeDirectory
	case mode.IsRegular():
		return proto.FileEntryTypeRegular
	default:
		return proto.FileEntryTypeOther
	}
}

func fileEntryFromInfo(name, path string, info os.FileInfo) proto.FileEntry {
	return proto.FileEntry{
		Name:      name,
		Path:      path,
		Size:      info.Size(),
		Mode:      uint32(info.Mode()),
		Type:      fileEntryType(info.Mode()),
		IsDir:     info.IsDir(),
		IsSymlink: info.Mode()&os.ModeSymlink != 0,
		ModTime:   info.ModTime().UTC(),
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
			fe.Mode = uint32(info.Mode())
			fe.ModTime = info.ModTime().UTC()
			fe.IsSymlink = info.Mode()&os.ModeSymlink != 0
			fe.Type = fileEntryType(info.Mode())
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

// Tree bounds for file.tree. The byte budget keeps the reply well inside
// proto.MaxEnvelopeSize however long the paths are; a tree that does not fit
// is reported as truncated and the controller lists it directory by directory.
const (
	maxTreeEntries    = 50_000
	maxTreeDepth      = 64
	maxTreeReplyBytes = 10 * 1024 * 1024
	treeEntryOverhead = 192 // JSON field names, mode, size and timestamp
)

func (m *fileManager) Tree(_ context.Context, p proto.FileTreePayload) (proto.FileTreeResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileTreeResult{}, rerr
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileTreeResult{}, rerr
	}
	defer root.Close()
	maxEntries := maxTreeEntries
	if p.MaxEntries > 0 && p.MaxEntries < maxEntries {
		maxEntries = p.MaxEntries
	}
	maxDepth := maxTreeDepth
	if p.MaxDepth > 0 && p.MaxDepth < maxDepth {
		maxDepth = p.MaxDepth
	}

	type dirItem struct {
		rel, abs string
		depth    int
	}
	result := proto.FileTreeResult{Path: real}
	truncated := func() (proto.FileTreeResult, error) {
		return proto.FileTreeResult{Path: real, Truncated: true}, nil
	}
	budget := maxTreeReplyBytes
	stack := []dirItem{{rel: rel, abs: real}}
	for len(stack) > 0 {
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dir, err := root.Open(item.rel)
		if err != nil {
			return proto.FileTreeResult{}, &RPCError{Code: "list_failed", Message: err.Error()}
		}
		entries, err := dir.ReadDir(-1)
		_ = dir.Close()
		if err != nil {
			return proto.FileTreeResult{}, &RPCError{Code: "list_failed", Message: err.Error()}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		var subdirs []dirItem
		for _, entry := range entries {
			name := entry.Name()
			if !p.ShowHidden && strings.HasPrefix(name, ".") {
				continue
			}
			abs := filepath.Join(item.abs, name)
			info, err := entry.Info() // lstat semantics: never follows a symlink
			if err != nil {
				return proto.FileTreeResult{}, &RPCError{Code: "list_failed", Message: err.Error()}
			}
			budget -= len(abs) + len(name) + treeEntryOverhead
			if len(result.Entries) >= maxEntries || budget < 0 {
				return truncated()
			}
			result.Entries = append(result.Entries, fileEntryFromInfo(name, abs, info))
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if item.depth+1 > maxDepth {
					return truncated()
				}
				// Descending must honour exactly what a file.list of this
				// directory would: pseudo filesystems stay off limits.
				if rerr := checkBlockedTransferPath(abs); rerr != nil {
					return proto.FileTreeResult{}, rerr
				}
				subdirs = append(subdirs, dirItem{rel: filepath.Join(item.rel, name), abs: abs, depth: item.depth + 1})
			}
		}
		// Push in reverse so directories are visited in name order.
		for i := len(subdirs) - 1; i >= 0; i-- {
			stack = append(stack, subdirs[i])
		}
	}
	if result.Entries == nil {
		result.Entries = []proto.FileEntry{}
	}
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
	info, err := root.Lstat(rel)
	if err != nil {
		return proto.FileStatResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	return proto.FileStatResult{Entry: fileEntryFromInfo(info.Name(), real, info)}, nil
}

func (m *fileManager) Read(ctx context.Context, p proto.FileReadPayload) (proto.FileReadResult, error) {
	res, _, err := m.read(ctx, p, false)
	return res, err
}

// readPooled is Read with a recycled buffer: the caller must invoke release
// once the result's Data has been encoded (and never touch Data afterwards).
func (m *fileManager) readPooled(ctx context.Context, p proto.FileReadPayload) (proto.FileReadResult, func(), error) {
	return m.read(ctx, p, true)
}

func noRelease() {}

// pooledFileReader is implemented by the default file manager: its reads reuse
// chunk buffers, which must be handed back once the reply has been encoded.
type pooledFileReader interface {
	readPooled(context.Context, proto.FileReadPayload) (proto.FileReadResult, func(), error)
}

// handleFileRead serves file.read, recycling the chunk buffer after the reply
// is on the wire. Other FileManager implementations are served unchanged.
func handleFileRead(encode func(proto.Envelope) error, request proto.Envelope, fm FileManager) {
	pr, ok := fm.(pooledFileReader)
	if !ok {
		handleFileRPC(encode, request, fm.Read)
		return
	}
	release := noRelease
	handleFileRPC(encode, request, func(ctx context.Context, p proto.FileReadPayload) (proto.FileReadResult, error) {
		res, rel, err := pr.readPooled(ctx, p)
		release = rel
		return res, err
	})
	// handleFileRPC encodes synchronously, so nothing references the buffer now.
	release()
}

func (m *fileManager) read(_ context.Context, p proto.FileReadPayload, pooled bool) (proto.FileReadResult, func(), error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileReadResult{}, noRelease, rerr
	}
	if p.Offset < 0 {
		return proto.FileReadResult{}, noRelease, &RPCError{Code: "invalid_offset", Message: "offset must be non-negative"}
	}
	length := p.Length
	if length <= 0 || length > proto.MaxRawChunkBytes {
		length = proto.MaxRawChunkBytes
	}
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileReadResult{}, noRelease, rerr
	}
	defer root.Close()
	var entry *proto.FileEntry
	if p.Stat {
		info, err := root.Lstat(rel)
		if err != nil {
			return proto.FileReadResult{}, noRelease, &RPCError{Code: "stat_failed", Message: err.Error()}
		}
		fe := fileEntryFromInfo(info.Name(), real, info)
		entry = &fe
		if !info.Mode().IsRegular() {
			// Report what the path is and let the controller refuse it; there
			// are no bytes to return for a directory or special file.
			return proto.FileReadResult{Offset: p.Offset, EOF: true, Entry: entry}, noRelease, nil
		}
	}
	file, err := root.Open(rel)
	if err != nil {
		return proto.FileReadResult{}, noRelease, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return proto.FileReadResult{}, noRelease, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	if p.Offset >= info.Size() {
		return proto.FileReadResult{Offset: p.Offset, Length: 0, EOF: true, Entry: entry}, noRelease, nil
	}
	// Overflow-safe clamp (p.Offset < info.Size() here, so the subtraction is
	// non-negative and p.Offset+length can't be relied upon — it may overflow).
	if length > info.Size()-p.Offset {
		length = info.Size() - p.Offset
	}
	var buf []byte
	release := noRelease
	if pooled && length <= transferBufSize {
		bufp := transferBufPool.Get().(*[]byte)
		buf = (*bufp)[:length]
		release = func() { transferBufPool.Put(bufp) }
	} else {
		buf = make([]byte, length)
	}
	n, err := file.ReadAt(buf, p.Offset)
	if err != nil && err != io.EOF {
		release()
		return proto.FileReadResult{}, noRelease, &RPCError{Code: "read_failed", Message: err.Error()}
	}
	buf = buf[:n]
	sum := sha256.Sum256(buf)
	return proto.FileReadResult{
		Offset: p.Offset,
		Length: int64(n),
		Data:   buf,
		SHA256: hex.EncodeToString(sum[:]),
		EOF:    p.Offset+int64(n) >= info.Size(),
		Entry:  entry,
	}, release, nil
}

// targetIsDirectory refuses to write a file over an existing directory. It runs
// before any temp file is created, so a mistaken destination costs one round
// trip and leaves nothing behind (the controller then retries inside the
// directory, like cp/scp).
func targetIsDirectory(root *os.Root, rel string) *RPCError {
	if info, err := root.Lstat(rel); err == nil && info.IsDir() {
		return &RPCError{Code: "target_is_directory", Message: "destination is an existing directory"}
	}
	return nil
}

// openUploadSidecar opens the temp file an upload is assembled in.
//
// Its name, <dest>.fleet-<transfer id>.part, is predictable — the id is derived
// from the destination and the source's size and mtime — so that an upload can
// resume after an agent restart. In a directory other local users can write to
// (/tmp, shared upload directories) anyone could pre-create a file of that
// name, or a hard link to one, and then own the installed file or change its
// bytes after a resume probe checked them. So an existing file is resumed only
// when the open descriptor proves it is ours alone (privateToAgent). Anything
// else is left untouched and the upload starts in a new temp file created
// exclusively under an unpredictable name.
func openUploadSidecar(root *os.Root, finalRel, tempRel string) (*os.File, string, os.FileInfo, *RPCError) {
	f, err := root.OpenFile(tempRel, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err == nil {
		return statSidecar(f, tempRel)
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, "", nil, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	if named, err := root.Lstat(tempRel); err == nil && privateToAgent(named) {
		if f, err := root.OpenFile(tempRel, os.O_RDWR|oNoFollow|oNonBlock, 0); err == nil {
			// Decide on what was opened, not on the name looked up before.
			if info, err := f.Stat(); err == nil && privateToAgent(info) && sameInode(named, info) {
				return f, tempRel, info, nil
			}
			_ = f.Close()
		}
	}
	id, err := randomTempID()
	if err != nil {
		return nil, "", nil, &RPCError{Code: "internal_error", Message: err.Error()}
	}
	fresh := finalRel + ".fleet-" + id + ".part"
	if filepath.Dir(fresh) != filepath.Dir(finalRel) {
		return nil, "", nil, invalidTransferID()
	}
	f, err = root.OpenFile(fresh, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return nil, "", nil, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	return statSidecar(f, fresh)
}

func statSidecar(f *os.File, rel string) (*os.File, string, os.FileInfo, *RPCError) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("upload sidecar is not a regular file")
		}
		return nil, "", nil, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	return f, rel, info, nil
}

// removeIfSame removes rel only while it still names the file described by
// info, so cleaning up after a failure can never delete a file another local
// user put in its place.
func removeIfSame(root *os.Root, rel string, info os.FileInfo) {
	if cur, err := root.Lstat(rel); err == nil && sameInode(cur, info) {
		_ = root.Remove(rel)
	}
}

// installTemp renames a finished temp file over the destination. info is the
// fstat of the temp file taken while it was still open. The name must still
// refer to that very file — in a directory other users can write to, the name
// could have been swapped for theirs — and after the rename the destination
// must be it; otherwise nothing of theirs is left installed and the upload
// fails.
func installTemp(root *os.Root, tempRel, finalRel string, info os.FileInfo) *RPCError {
	cur, err := root.Lstat(tempRel)
	if err != nil || !sameInode(cur, info) {
		return &RPCError{Code: "temp_replaced", Message: "upload temp file was replaced before it could be installed"}
	}
	if err := root.Rename(tempRel, finalRel); err != nil {
		removeIfSame(root, tempRel, info)
		return &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	if got, err := root.Lstat(finalRel); err != nil || !sameInode(got, info) {
		if err == nil {
			_ = root.Remove(finalRel)
		}
		return &RPCError{Code: "temp_replaced", Message: "upload temp file was replaced while it was being installed"}
	}
	return nil
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
		if existing.finalizing {
			return proto.FileOpenWriteResult{}, transferBusy()
		}
		if existing.finalPath != real || existing.totalSize != p.TotalSize {
			return proto.FileOpenWriteResult{}, &RPCError{Code: "transfer_conflict", Message: "transfer_id is already bound to a different destination or size"}
		}
		if info, ok := existing.intact(); ok {
			return proto.FileOpenWriteResult{TempPath: existing.tempPath, ResumeOffset: info.Size(), Chunks: existing.sortedRecords(maxReportedChunks)}, nil
		}
		// The temp file was unlinked, replaced or linked elsewhere since the
		// last attempt; it could never be installed. Forget it (without
		// touching whatever now has its name) and start afresh.
		existing.mu.Lock()
		existing.done = true
		_ = existing.f.Close()
		_ = existing.root.Close()
		existing.mu.Unlock()
		delete(m.active, p.TransferID)
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
	if rerr := targetIsDirectory(root, finalRel); rerr != nil {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, rerr
	}
	if err := checkBlockedTransferPath(filepath.Join(root.Name(), tempRel)); err != nil {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, err
	}
	maybeReapStalePartsRoot(root, filepath.Dir(finalRel), filepath.Base(tempRel), time.Now())
	f, tempRel, info, rerr := openUploadSidecar(root, finalRel, tempRel)
	if rerr != nil {
		_ = root.Close()
		return proto.FileOpenWriteResult{}, rerr
	}
	temp := filepath.Join(root.Name(), tempRel)
	mode := p.Mode
	if mode == 0 {
		mode = 0o644
	}
	m.active[p.TransferID] = &activeUpload{
		f: f, root: root, tempPath: temp, finalPath: real, rawPath: p.Path,
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
	// The path only binds this write to the upload the transfer id was opened
	// for; the bytes go to the already-open temp descriptor. The same string
	// open_write validated needs no second symlink walk per chunk.
	if p.Path != au.rawPath {
		real, rerr := validateTransferEntryPath(p.Path)
		if rerr != nil {
			return proto.FileWriteResult{}, rerr
		}
		if real != au.finalPath {
			return proto.FileWriteResult{}, &RPCError{Code: "transfer_conflict", Message: "transfer path does not match open upload"}
		}
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
	var sum [sha256.Size]byte
	verified := false
	if p.SHA256 != "" {
		sum = sha256.Sum256(p.Data)
		if hex.EncodeToString(sum[:]) != p.SHA256 {
			return proto.FileWriteResult{}, &RPCError{Code: "checksum_mismatch", Message: "chunk checksum mismatch"}
		}
		verified = true
	}
	// RLock lets parallel writers run concurrently (their ranges are disjoint)
	// while excluding Finalize, which closes the file under a full Lock.
	au.mu.RLock()
	defer au.mu.RUnlock()
	if au.done {
		return proto.FileWriteResult{}, &RPCError{Code: "upload_finalized", Message: "upload has already been finalized"}
	}
	au.lockRange(p.Offset, dataLen)
	defer au.unlockRange(p.Offset, dataLen)
	// Forget the old checksum of these bytes before changing them, so a
	// concurrent reader of the records can never see a stale claim.
	au.forget(p.Offset, dataLen)
	n, err := au.f.WriteAt(p.Data, p.Offset)
	if err != nil {
		return proto.FileWriteResult{}, &RPCError{Code: "write_failed", Message: err.Error()}
	}
	if testHookAfterWriteAt != nil {
		testHookAfterWriteAt(p.Offset)
	}
	if verified && dataLen > 0 {
		au.record(p.Offset, dataLen, sum)
	}
	return proto.FileWriteResult{Offset: p.Offset, BytesWritten: int64(n)}, nil
}

// discardUpload closes and removes an upload's temp file. Called with au.mu
// held for writing and the upload already removed from the active map.
func discardUpload(au *activeUpload) {
	au.done = true
	if info, err := au.f.Stat(); err == nil {
		removeIfSame(au.root, au.tempRel, info)
	}
	_ = au.f.Close()
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
		if p.Abort {
			return m.abortInactive(real, p.TransferID)
		}
		return proto.FileFinalizeResult{}, &RPCError{Code: "unknown_transfer", Message: "no open upload for transfer id"}
	}
	if real != au.finalPath || (!p.Abort && p.TotalSize != au.totalSize) {
		m.mu.Unlock()
		return proto.FileFinalizeResult{}, &RPCError{Code: "transfer_conflict", Message: "finalize path or size does not match open upload"}
	}
	if au.finalizing {
		m.mu.Unlock()
		return proto.FileFinalizeResult{}, transferBusy()
	}
	// Stay registered (as finalizing) until the temp file has been renamed or
	// removed. Dropping the entry first let an open_write for the same
	// transfer — a controller retrying after losing the finalize reply —
	// reopen the temp file just before this finalize renamed it away, and
	// that second upload then failed to install.
	au.finalizing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.active[p.TransferID] == au {
			delete(m.active, p.TransferID)
		}
		m.mu.Unlock()
	}()
	defer au.root.Close()

	au.mu.Lock()
	defer au.mu.Unlock()
	if p.Abort {
		discardUpload(au)
		return proto.FileFinalizeResult{Path: au.finalPath}, nil
	}
	// Block any straggler write that already grabbed this au before we deleted
	// it from the map: once finalize owns the lock, the file is being closed.
	// Every failure below is terminal for this transfer, so the temp file is
	// removed rather than left beside the destination.
	fail := func(code, message string) (proto.FileFinalizeResult, error) {
		discardUpload(au)
		return proto.FileFinalizeResult{}, &RPCError{Code: code, Message: message}
	}
	au.done = true

	if err := au.f.Sync(); err != nil {
		return fail("sync_failed", err.Error())
	}
	info, err := au.f.Stat()
	if err != nil {
		return fail("stat_failed", err.Error())
	}
	if info.Size() != p.TotalSize {
		return fail("size_mismatch", fmt.Sprintf("expected %d bytes, assembled %d", p.TotalSize, info.Size()))
	}

	sum := p.WholeSHA256
	var chunkDigest string
	switch {
	case p.ChunkDigest != "":
		// Every byte was checked against its chunk checksum when it was
		// written (or hashed from disk for a resume probe), and records never
		// overlap. If the ordered records tile the file and their digest is
		// the controller's, the assembled file is exactly the bytes the
		// controller hashed — without reading it back.
		got, err := proto.ChunkListDigest(p.TotalSize, au.sortedRecords(-1))
		if err != nil {
			return fail("chunk_digest_mismatch", "assembled file is not fully covered by verified chunks: "+err.Error())
		}
		if got != p.ChunkDigest {
			return fail("chunk_digest_mismatch", "assembled chunk checksums do not match source")
		}
		chunkDigest = got
	case p.WholeSHA256 != "":
		h := sha256.New()
		bufp := ioBufPool.Get().(*[]byte)
		_, err := io.CopyBuffer(h, io.NewSectionReader(au.f, 0, info.Size()), *bufp)
		ioBufPool.Put(bufp)
		if err != nil {
			return fail("read_failed", err.Error())
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != p.WholeSHA256 {
			return fail("whole_checksum_mismatch", "assembled file checksum does not match source")
		}
	}

	// Whatever vouched for the bytes above, the file must still be ours
	// alone when it is installed: owned by the agent and with no second name
	// through which another local user could reach it (a hard link made while
	// the upload was in progress).
	if !exclusivelyOwned(info) {
		return fail("temp_not_private", "upload temp file is no longer private to the agent (owner or link count changed)")
	}

	mode := au.mode
	if p.Mode != 0 {
		mode = p.Mode
	}
	_ = au.f.Chmod(os.FileMode(mode)) // best effort; some filesystems disallow
	if err := au.f.Close(); err != nil {
		removeIfSame(au.root, au.tempRel, info)
		return proto.FileFinalizeResult{}, &RPCError{Code: "close_failed", Message: err.Error()}
	}
	if rerr := installTemp(au.root, au.tempRel, au.finalRel, info); rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	return proto.FileFinalizeResult{Path: au.finalPath, Size: info.Size(), SHA256: sum, ChunkDigest: chunkDigest}, nil
}

// abortInactive removes the temp file of an upload this process has no open
// state for (for example after an agent restart).
func (m *fileManager) abortInactive(real, transferID string) (proto.FileFinalizeResult, error) {
	root, finalRel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	defer root.Close()
	tempRel := finalRel + ".fleet-" + transferID + ".part"
	if filepath.Dir(tempRel) != filepath.Dir(finalRel) {
		return proto.FileFinalizeResult{}, invalidTransferID()
	}
	// Only ever remove a file that is provably the agent's own: the name is
	// predictable, and a controller must not be able to delete another local
	// user's file through it.
	if info, err := root.Lstat(tempRel); err == nil && privateToAgent(info) {
		removeIfSame(root, tempRel, info)
	}
	return proto.FileFinalizeResult{Path: real}, nil
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
	var au *activeUpload
	if p.TransferID != "" {
		tempRel := rel + ".fleet-" + p.TransferID + ".part"
		if filepath.Dir(tempRel) != filepath.Dir(rel) {
			return proto.FileProbeResult{}, invalidTransferID()
		}
		m.mu.Lock()
		if candidate, ok := m.active[p.TransferID]; ok && candidate.finalPath == real {
			au = candidate
		}
		m.mu.Unlock()
		if au == nil {
			// Without an open upload there is nothing to resume into; only
			// report whether a temp file exists.
			if info, err := root.Lstat(tempRel); err == nil && info.Mode().IsRegular() {
				return proto.FileProbeResult{Exists: true, CurrentSize: info.Size()}, nil
			} else if err != nil && !os.IsNotExist(err) {
				return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
			}
			return proto.FileProbeResult{Exists: false}, nil
		}
	}

	var file io.ReaderAt
	var info os.FileInfo
	if au != nil {
		// Hash the upload's own descriptor — never whatever its name refers
		// to now, which another local user may have swapped — and hold off
		// writers so each digest recorded describes bytes no concurrent write
		// is changing. Probes run for a resume, before those ranges are
		// rewritten.
		au.mu.Lock()
		defer au.mu.Unlock()
		if au.done {
			return proto.FileProbeResult{}, &RPCError{Code: "upload_finalized", Message: "upload has already been finalized"}
		}
		st, err := au.f.Stat()
		if err != nil {
			return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
		}
		file, info = au.f, st
	} else {
		st, err := root.Stat(rel)
		if err != nil {
			if os.IsNotExist(err) {
				return proto.FileProbeResult{Exists: false}, nil
			}
			return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
		}
		info = st
	}
	result := proto.FileProbeResult{Exists: true, CurrentSize: info.Size()}
	if len(p.Ranges) == 0 {
		return result, nil
	}
	const maxProbeRanges = 65536
	ranges := p.Ranges
	if len(ranges) > maxProbeRanges {
		ranges = ranges[:maxProbeRanges]
	}
	if file == nil {
		opened, err := root.Open(rel)
		if err != nil {
			return proto.FileProbeResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
		}
		defer opened.Close()
		file = opened
	}

	sums := make([]string, len(ranges))
	var todo []int
	for i, r := range ranges {
		if r.Length <= 0 || r.Length > proto.MaxRawChunkBytes || r.Offset < 0 || r.Length > info.Size() || r.Offset > info.Size()-r.Length {
			continue
		}
		if au != nil {
			// A range this process verified needs no second read: the
			// record is dropped before any write to those bytes.
			if sum, ok := au.lookup(r.Offset, r.Length); ok {
				sums[i] = sum
				continue
			}
		}
		todo = append(todo, i)
	}
	// Hash the rest from disk, spread across cores.
	var wg sync.WaitGroup
	next := make(chan int)
	for range hashWorkers(len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				r := ranges[i]
				sum, err := hashRange(file, r.Offset, r.Length)
				if err != nil {
					continue
				}
				sums[i] = hex.EncodeToString(sum[:])
				if au != nil {
					au.record(r.Offset, r.Length, sum)
				}
			}
		}()
	}
	for _, i := range todo {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, r := range ranges {
		if sums[i] == "" {
			continue
		}
		result.RangeChecksums = append(result.RangeChecksums, proto.FileRangeChecksum{
			Offset: r.Offset, Length: r.Length, SHA256: sums[i],
		})
	}
	return result, nil
}

// Put writes a whole small file in one round trip: private temp file beside the
// destination, checksum verified, fsync, atomic rename — exactly what
// open_write/write/finalize guarantee for larger files.
func (m *fileManager) Put(_ context.Context, p proto.FilePutPayload) (proto.FileFinalizeResult, error) {
	if len(p.Data) > proto.MaxRawChunkBytes {
		return proto.FileFinalizeResult{}, &RPCError{
			Code:    "chunk_too_large",
			Message: fmt.Sprintf("file of %d bytes exceeds max %d for file.put", len(p.Data), proto.MaxRawChunkBytes),
		}
	}
	real, rerr := validateTransferEntryPath(p.Path)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	sum := sha256.Sum256(p.Data)
	digest := hex.EncodeToString(sum[:])
	if p.SHA256 == "" || p.SHA256 != digest {
		return proto.FileFinalizeResult{}, &RPCError{Code: "checksum_mismatch", Message: "file checksum mismatch"}
	}
	root, finalRel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	defer root.Close()
	mode := p.Mode
	if mode == 0 {
		mode = 0o644
	}
	size, err := writeAtomically(root, finalRel, os.FileMode(mode), func(f *os.File) (int64, error) {
		n, err := f.Write(p.Data)
		return int64(n), err
	})
	if err != nil {
		return proto.FileFinalizeResult{}, err
	}
	return proto.FileFinalizeResult{Path: real, Size: size, SHA256: digest}, nil
}

// writeAtomically creates a fresh, private temp file next to finalRel, fills it
// with fill, syncs it, applies mode and renames it over finalRel. A planted
// symlink at the destination is replaced, never followed. On any failure the
// temp file is removed.
func writeAtomically(root *os.Root, finalRel string, mode os.FileMode, fill func(*os.File) (int64, error)) (int64, *RPCError) {
	if rerr := targetIsDirectory(root, finalRel); rerr != nil {
		return 0, rerr
	}
	id, err := randomTempID()
	if err != nil {
		return 0, &RPCError{Code: "internal_error", Message: err.Error()}
	}
	tempRel := finalRel + ".fleet-" + id + ".part"
	if filepath.Dir(tempRel) != filepath.Dir(finalRel) {
		return 0, invalidTransferID()
	}
	if rerr := checkBlockedTransferPath(filepath.Join(root.Name(), tempRel)); rerr != nil {
		return 0, rerr
	}
	maybeReapStalePartsRoot(root, filepath.Dir(finalRel), filepath.Base(tempRel), time.Now())
	f, err := root.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return 0, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	created, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	cleanup := func(rerr *RPCError) (int64, *RPCError) {
		_ = f.Close()
		removeIfSame(root, tempRel, created)
		return 0, rerr
	}
	n, err := fill(f)
	if err != nil {
		if e, ok := err.(*RPCError); ok {
			return cleanup(e)
		}
		return cleanup(&RPCError{Code: "write_failed", Message: err.Error()})
	}
	if err := f.Sync(); err != nil {
		return cleanup(&RPCError{Code: "sync_failed", Message: err.Error()})
	}
	_ = f.Chmod(mode) // best effort; some filesystems disallow
	info, err := f.Stat()
	if err != nil {
		return cleanup(&RPCError{Code: "stat_failed", Message: err.Error()})
	}
	if !exclusivelyOwned(info) {
		return cleanup(&RPCError{Code: "temp_not_private", Message: "temp file is no longer private to the agent (owner or link count changed)"})
	}
	if err := f.Close(); err != nil {
		removeIfSame(root, tempRel, info)
		return 0, &RPCError{Code: "close_failed", Message: err.Error()}
	}
	if rerr := installTemp(root, tempRel, finalRel, info); rerr != nil {
		return 0, rerr
	}
	return n, nil
}

// Copy duplicates a regular file on this host without the bytes leaving it.
// The source is resolved like a read (symlinks followed, sandbox enforced); the
// destination is written like an upload.
func (m *fileManager) Copy(_ context.Context, p proto.FileCopyPayload) (proto.FileFinalizeResult, error) {
	src, rerr := validateTransferPath(p.From)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	dst, rerr := validateTransferEntryPath(p.To)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	srcRoot, srcRel, rerr := openTransferRoot(src)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	defer srcRoot.Close()
	in, err := srcRoot.Open(srcRel)
	if err != nil {
		return proto.FileFinalizeResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	defer in.Close()
	srcInfo, err := in.Stat()
	if err != nil {
		return proto.FileFinalizeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	if !srcInfo.Mode().IsRegular() {
		return proto.FileFinalizeResult{}, &RPCError{Code: "not_regular", Message: "copy source is not a regular file"}
	}
	dstRoot, dstRel, rerr := openTransferRoot(dst)
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	defer dstRoot.Close()
	if info, err := dstRoot.Stat(dstRel); err == nil && os.SameFile(info, srcInfo) {
		return proto.FileFinalizeResult{}, &RPCError{Code: "same_file", Message: "source and destination are the same file"}
	}
	mode := os.FileMode(p.Mode)
	if mode == 0 {
		mode = srcInfo.Mode().Perm()
	}
	want := srcInfo.Size()
	h := sha256.New()
	size, rerr := writeAtomically(dstRoot, dstRel, mode, func(f *os.File) (int64, error) {
		bufp := ioBufPool.Get().(*[]byte)
		defer ioBufPool.Put(bufp)
		// Copy exactly the size seen at open: a file that shrinks underneath
		// us is an error, one that grows is copied as it was.
		n, err := io.CopyBuffer(io.MultiWriter(f, h), io.NewSectionReader(in, 0, want), *bufp)
		if err == nil && n != want {
			err = &RPCError{Code: "source_changed", Message: fmt.Sprintf("source shrank during copy: copied %d of %d bytes", n, want)}
		}
		return n, err
	})
	if rerr != nil {
		return proto.FileFinalizeResult{}, rerr
	}
	return proto.FileFinalizeResult{Path: dst, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
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
	if rel == "." {
		// Never remove a whole file root (or the filesystem root) as one
		// operand; delete its contents individually instead.
		return proto.FileOpResult{}, &RPCError{Code: "invalid_path", Message: "refusing to delete a file root"}
	}
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
	if fromRel == "." || toRel == "." {
		return proto.FileOpResult{}, &RPCError{Code: "invalid_path", Message: "refusing to rename a file root"}
	}
	if err := fromRoot.Rename(fromRel, toRel); err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: to}, nil
}
