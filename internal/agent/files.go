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

	"github.com/cenvero/fleet/pkg/proto"
)

// blockedTransferPrefixes mirrors blockedLogPrefixes: OS virtual filesystems
// that must never be read from or written to through file transfer, even by an
// authenticated controller. Reading /proc/N/mem leaks process memory; writing
// under /sys or /dev can damage the host.
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
	tempPath  string
	finalPath string
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
// destination, mkdir, rename target). The final component can't be resolved,
// so we resolve the parent directory's symlinks and rejoin — that still
// prevents a symlinked parent (e.g. /tmp/x -> /proc) from escaping the block
// list.
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
	return real, nil
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
	return nil
}

func (m *fileManager) List(_ context.Context, p proto.FileListPayload) (proto.FileListResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileListResult{}, rerr
	}
	entries, err := os.ReadDir(real)
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
	info, err := os.Stat(real)
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
	file, err := os.Open(real)
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
	if p.TransferID == "" {
		return proto.FileOpenWriteResult{}, &RPCError{Code: "missing_transfer_id", Message: "transfer_id is required"}
	}
	real, rerr := validateWriteTarget(p.Path)
	if rerr != nil {
		return proto.FileOpenWriteResult{}, rerr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.active[p.TransferID]; ok {
		var size int64
		if info, err := existing.f.Stat(); err == nil {
			size = info.Size()
		}
		return proto.FileOpenWriteResult{TempPath: existing.tempPath, ResumeOffset: size}, nil
	}

	// The temp file is keyed by TransferID, not just the destination path, so two
	// concurrent transfers to the same remote path can't corrupt one shared temp.
	// TransferID is deterministic from (path,size,content-hash), so the SAME
	// content still maps to the SAME temp and resumes; different content does not.
	temp := real + ".fleet-" + p.TransferID + ".part"
	// O_CREATE without O_TRUNC: a temp left by a previous interrupted transfer
	// is reopened so its bytes can be reused for resume.
	f, err := os.OpenFile(temp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return proto.FileOpenWriteResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return proto.FileOpenWriteResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	mode := p.Mode
	if mode == 0 {
		mode = 0o644
	}
	m.active[p.TransferID] = &activeUpload{
		f:         f,
		tempPath:  temp,
		finalPath: real,
		totalSize: p.TotalSize,
		mode:      mode,
	}
	return proto.FileOpenWriteResult{TempPath: temp, ResumeOffset: info.Size()}, nil
}

func (m *fileManager) Write(_ context.Context, p proto.FileWritePayload) (proto.FileWriteResult, error) {
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
	// Bound the write to the size declared at open_write. Without this a client
	// could write at an arbitrary offset (e.g. 1 PiB) and allocate an enormous
	// sparse file; with TotalSize=0 it also blocks any non-empty write.
	if p.Offset+int64(len(p.Data)) > au.totalSize {
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
	m.mu.Lock()
	au, ok := m.active[p.TransferID]
	if ok {
		delete(m.active, p.TransferID)
	}
	m.mu.Unlock()
	if !ok {
		return proto.FileFinalizeResult{}, &RPCError{Code: "unknown_transfer", Message: "no open upload for transfer id"}
	}

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
	if p.TotalSize > 0 && info.Size() != p.TotalSize {
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
	if err := os.Rename(au.tempPath, au.finalPath); err != nil {
		return proto.FileFinalizeResult{}, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	return proto.FileFinalizeResult{Path: au.finalPath, Size: info.Size(), SHA256: sum}, nil
}

func (m *fileManager) Probe(_ context.Context, p proto.FileProbePayload) (proto.FileProbeResult, error) {
	probePath := ""
	if p.TransferID != "" {
		m.mu.Lock()
		au, ok := m.active[p.TransferID]
		m.mu.Unlock()
		if ok {
			probePath = au.tempPath
		}
	}
	if probePath == "" {
		real, rerr := validateWriteTarget(p.Path)
		if rerr != nil {
			return proto.FileProbeResult{}, rerr
		}
		// Prefer an upload temp left by an interrupted transfer; otherwise fall
		// back to the real path (download source probe).
		if temp := real + ".fleetpart"; fileExists(temp) {
			probePath = temp
		} else {
			probePath = real
		}
	}

	info, err := os.Stat(probePath)
	if err != nil {
		if os.IsNotExist(err) {
			return proto.FileProbeResult{Exists: false}, nil
		}
		return proto.FileProbeResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	result := proto.FileProbeResult{Exists: true, CurrentSize: info.Size()}
	if len(p.Ranges) > 0 {
		file, err := os.Open(probePath)
		if err != nil {
			return proto.FileProbeResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
		}
		defer file.Close()
		for _, r := range p.Ranges {
			// Overflow-safe: avoid r.Offset+r.Length which can wrap for hostile
			// near-MaxInt64 values. Equivalent to r.Offset+r.Length > size.
			if r.Length <= 0 || r.Offset < 0 || r.Length > info.Size() || r.Offset > info.Size()-r.Length {
				continue // range not fully present yet — leave it out
			}
			buf := make([]byte, r.Length)
			if _, err := file.ReadAt(buf, r.Offset); err != nil {
				continue
			}
			sum := sha256.Sum256(buf)
			result.RangeChecksums = append(result.RangeChecksums, proto.FileRangeChecksum{
				Offset: r.Offset,
				Length: r.Length,
				SHA256: hex.EncodeToString(sum[:]),
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
	if err := os.MkdirAll(real, os.FileMode(mode)); err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "mkdir_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: real}, nil
}

func (m *fileManager) Delete(_ context.Context, p proto.FileDeletePayload) (proto.FileOpResult, error) {
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	var err error
	if p.Recursive {
		err = os.RemoveAll(real)
	} else {
		err = os.Remove(real)
	}
	if err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "delete_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: real}, nil
}

func (m *fileManager) Rename(_ context.Context, p proto.FileRenamePayload) (proto.FileOpResult, error) {
	from, rerr := validateTransferPath(p.From)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	to, rerr := validateWriteTarget(p.To)
	if rerr != nil {
		return proto.FileOpResult{}, rerr
	}
	if err := os.Rename(from, to); err != nil {
		return proto.FileOpResult{}, &RPCError{Code: "rename_failed", Message: err.Error()}
	}
	return proto.FileOpResult{Path: to}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
