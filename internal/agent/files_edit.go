// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cenvero/fleet/internal/textdiff"
	"github.com/cenvero/fleet/internal/textedit"
	"github.com/cenvero/fleet/pkg/proto"
)

// fileEditor is implemented by file managers that support file.edit. It is a
// separate interface so FileManager implementations without it keep working
// (they answer file.edit with unsupported_action).
type fileEditor interface {
	Edit(context.Context, proto.FileEditPayload) (proto.FileEditResult, error)
}

// testHookBeforeEditInstall, when set by a test, runs just before installEdit
// checks that the original is unchanged. Production code never sets it.
var testHookBeforeEditInstall func()

// maxEditDiffBytes caps the diff returned with an edit.
const maxEditDiffBytes = 64 * 1024

// editLocks serialises file.edit calls for the same file on this agent, so two
// edits racing on one file cannot both read the old content and have the
// second silently undo the first. (Writers outside Fleet are caught by the
// before/after checks in installEdit instead.)
var editLocks = struct {
	mu sync.Mutex
	m  map[string]*editLock
}{m: map[string]*editLock{}}

type editLock struct {
	mu   sync.Mutex
	refs int
}

func lockEditPath(path string) func() {
	editLocks.mu.Lock()
	l := editLocks.m[path]
	if l == nil {
		l = &editLock{}
		editLocks.m[path] = l
	}
	l.refs++
	editLocks.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		editLocks.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(editLocks.m, path)
		}
		editLocks.mu.Unlock()
	}
}

// editResults remembers the results of recent edits by EditID, so a controller
// that lost a reply and sends the same request again gets the first result
// instead of the edit being applied twice. The previous content returned for
// the controller's undo history is kept too, within a memory budget.
var editResults = newEditResultCache(256, 32<<20, 15*time.Minute)

type editResultEntry struct {
	path   string
	result proto.FileEditResult
	at     time.Time
}

type editResultCache struct {
	mu        sync.Mutex
	max       int
	origMax   int
	origBytes int
	ttl       time.Duration
	entries   map[string]*editResultEntry
	order     []string
}

func newEditResultCache(maxEntries, maxOriginalBytes int, ttl time.Duration) *editResultCache {
	return &editResultCache{max: maxEntries, origMax: maxOriginalBytes, ttl: ttl, entries: map[string]*editResultEntry{}}
}

func (c *editResultCache) get(id, path string, now time.Time) (proto.FileEditResult, bool) {
	if id == "" {
		return proto.FileEditResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	if !ok || e.path != path || now.Sub(e.at) > c.ttl {
		return proto.FileEditResult{}, false
	}
	return e.result, true
}

func (c *editResultCache) put(id, path string, res proto.FileEditResult, now time.Time) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[id]; ok {
		c.origBytes -= len(old.result.Original)
	} else {
		c.order = append(c.order, id)
	}
	if len(res.Original) > c.origMax {
		res.Original = nil
	}
	c.entries[id] = &editResultEntry{path: path, result: res, at: now}
	c.origBytes += len(res.Original)
	for len(c.order) > c.max {
		c.origBytes -= len(c.entries[c.order[0]].result.Original)
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	// Over the memory budget: drop the oldest kept originals first. Their
	// results stay, so a retry still never applies an edit twice.
	for _, oid := range c.order {
		if c.origBytes <= c.origMax {
			break
		}
		if e := c.entries[oid]; len(e.result.Original) > 0 {
			c.origBytes -= len(e.result.Original)
			e.result.Original = nil
		}
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// editRPCError converts an edit-engine error to the RPC error the controller
// sees, keeping its code.
func editRPCError(err error) *RPCError {
	var te *textedit.Error
	if errors.As(err, &te) {
		return &RPCError{Code: te.Code, Message: te.Message}
	}
	var re *RPCError
	if errors.As(err, &re) {
		return re
	}
	return &RPCError{Code: "edit_failed", Message: err.Error()}
}

// Edit implements file.edit: read the file, apply the change, and install the
// result atomically with the original's metadata. See proto.FileEditPayload.
func (m *fileManager) Edit(_ context.Context, p proto.FileEditPayload) (proto.FileEditResult, error) {
	if p.EditID != "" && !validTransferID(p.EditID) {
		return proto.FileEditResult{}, &RPCError{Code: "invalid_edit", Message: "edit_id must be a bounded safe token"}
	}
	if cached, ok := editResults.get(p.EditID, p.Path, time.Now()); ok {
		return cached, nil
	}
	if p.Replace == (len(p.Ops) > 0) {
		return proto.FileEditResult{}, &RPCError{Code: "invalid_edit", Message: "give either edit operations or replacement content"}
	}
	if p.Create && !p.Replace {
		return proto.FileEditResult{}, &RPCError{Code: "invalid_edit", Message: "creating a file needs its content"}
	}
	if hasDotComponent(p.Path) {
		return proto.FileEditResult{}, dotComponentError()
	}
	limit := int64(proto.MaxEditFileBytes)
	if p.MaxBytes > 0 && p.MaxBytes < limit {
		limit = p.MaxBytes
	}
	if p.Replace {
		if int64(len(p.Content)) > limit {
			return proto.FileEditResult{}, &RPCError{Code: textedit.CodeFileTooLarge, Message: fmt.Sprintf("new content is %d bytes, over the %d-byte edit limit", len(p.Content), limit)}
		}
		if p.ContentSHA256 == "" || !strings.EqualFold(p.ContentSHA256, sha256Hex(p.Content)) {
			return proto.FileEditResult{}, &RPCError{Code: "checksum_mismatch", Message: "the content received does not match its SHA-256; nothing was changed"}
		}
	}
	var res proto.FileEditResult
	var rerr *RPCError
	if p.Create {
		res, rerr = m.editCreate(p)
	} else {
		res, rerr = m.editExisting(p, limit)
	}
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	if res.Changed && !res.DryRun {
		editResults.put(p.EditID, p.Path, res, time.Now())
	}
	return res, nil
}

func (m *fileManager) editExisting(p proto.FileEditPayload, limit int64) (proto.FileEditResult, *RPCError) {
	// Resolve symlinks: like a text editor, an edit through a symlink changes
	// the file it points to and leaves the link in place. The resolved target
	// is what the block list and --file-root sandbox are checked against.
	real, rerr := validateTransferPath(p.Path)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	unlock := lockEditPath(real)
	defer unlock()
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	defer root.Close()

	// The path was fully resolved above, so a symlink at the final name now
	// was put there since; O_NOFOLLOW refuses it rather than following it.
	f, err := root.OpenFile(rel, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return proto.FileEditResult{}, &RPCError{Code: "not_found", Message: "the file does not exist (create it by giving its full content with create)"}
		}
		return proto.FileEditResult{}, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return proto.FileEditResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	if !info.Mode().IsRegular() {
		return proto.FileEditResult{}, &RPCError{Code: "not_regular", Message: "only regular files can be edited"}
	}
	if info.Size() > limit {
		return proto.FileEditResult{}, &RPCError{Code: textedit.CodeFileTooLarge, Message: fmt.Sprintf("the file is %d bytes, over the %d-byte edit limit; transfer it with file upload/download instead", info.Size(), limit)}
	}
	if n := linkCount(info); n > 1 {
		return proto.FileEditResult{}, &RPCError{Code: "hard_linked", Message: fmt.Sprintf("the file has %d hard links; replacing it would split them apart, so it is left unchanged", n)}
	}
	if rerr := checkEditWritable(root, rel); rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return proto.FileEditResult{}, &RPCError{Code: "read_failed", Message: err.Error()}
	}
	if int64(len(content)) > limit {
		return proto.FileEditResult{}, &RPCError{Code: textedit.CodeFileTooLarge, Message: fmt.Sprintf("the file grew past the %d-byte edit limit while it was read", limit)}
	}
	oldSum := sha256Hex(content)
	if p.BaseSHA256 != "" && !strings.EqualFold(p.BaseSHA256, oldSum) {
		return proto.FileEditResult{}, &RPCError{Code: "edit_conflict", Message: fmt.Sprintf("the file changed since it was read (expected sha256 %s, it is now %s); read it again and redo the edit", p.BaseSHA256, oldSum)}
	}

	var next []byte
	edits := 1
	if p.Replace {
		next = p.Content
	} else {
		next, edits, err = textedit.Apply(content, p.Ops, limit)
		if err != nil {
			return proto.FileEditResult{}, editRPCError(err)
		}
	}
	newSum := sha256Hex(next)
	res := proto.FileEditResult{
		Path:      real,
		Changed:   newSum != oldSum,
		DryRun:    p.DryRun,
		OldSHA256: oldSum,
		NewSHA256: newSum,
		OldSize:   int64(len(content)),
		NewSize:   int64(len(next)),
	}
	if res.Changed {
		res.Edits = edits
		res.Diff, res.DiffTruncated = editDiff(real, content, next)
	}
	if !res.Changed || p.DryRun {
		describeFile(&res, info)
		return res, nil
	}

	preserved, rerr := installEdit(root, rel, f, info, next, newSum)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	res.Verified = true
	if lst, err := os.Lstat(filepath.Clean(p.Path)); err == nil && lst.Mode()&os.ModeSymlink != 0 {
		preserved = append(preserved, "symlink")
	}
	res.Preserved = preserved
	if installed, err := root.Lstat(rel); err == nil {
		describeFile(&res, installed)
	} else {
		describeFile(&res, info)
	}
	if p.ReturnOriginal {
		res.Original = content
	}
	return res, nil
}

// editCreate creates a new file with the given content. It never replaces an
// existing file: the finished temp file is hard-linked into place, which fails
// if anything appeared at the name in the meantime.
func (m *fileManager) editCreate(p proto.FileEditPayload) (proto.FileEditResult, *RPCError) {
	real, rerr := validateWriteTarget(p.Path)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	unlock := lockEditPath(real)
	defer unlock()
	root, rel, rerr := openTransferRoot(real)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	defer root.Close()
	if _, err := root.Lstat(rel); err == nil {
		return proto.FileEditResult{}, &RPCError{Code: "exists", Message: "the file already exists; edit it, or replace its content given its current sha256"}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return proto.FileEditResult{}, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	mode := os.FileMode(p.Mode) & os.ModePerm
	if p.Mode == 0 {
		mode = 0o644
	}
	sum := sha256Hex(p.Content)
	res := proto.FileEditResult{Path: real, Changed: true, Created: true, DryRun: p.DryRun, Edits: 1, NewSHA256: sum, NewSize: int64(len(p.Content)), Mode: unixModeBits(mode)}
	res.Diff, res.DiffTruncated = editDiff(real, nil, p.Content)
	if p.DryRun {
		return res, nil
	}
	tf, tempRel, created, rerr := writeVerifiedTemp(root, rel, p.Content, sum)
	if rerr != nil {
		return proto.FileEditResult{}, rerr
	}
	if err := tf.Chmod(mode); err != nil {
		_ = tf.Close()
		removeIfSame(root, tempRel, created)
		return proto.FileEditResult{}, &RPCError{Code: "chmod_failed", Message: err.Error()}
	}
	if err := tf.Close(); err != nil {
		removeIfSame(root, tempRel, created)
		return proto.FileEditResult{}, &RPCError{Code: "close_failed", Message: err.Error()}
	}
	if err := root.Link(tempRel, rel); err != nil {
		removeIfSame(root, tempRel, created)
		if errors.Is(err, fs.ErrExist) {
			return proto.FileEditResult{}, &RPCError{Code: "exists", Message: "the file was created by something else while this edit ran; nothing was changed"}
		}
		return proto.FileEditResult{}, &RPCError{Code: "create_failed", Message: err.Error()}
	}
	removeIfSame(root, tempRel, created)
	syncParentDir(root, rel)
	res.Verified = true
	if installed, err := root.Lstat(rel); err == nil {
		describeFile(&res, installed)
	}
	return res, nil
}

// writeVerifiedTemp writes data to a new private temp file beside rel, fsyncs
// it and reads it back to check its SHA-256 against sum. The open temp file is
// returned for the caller to finish (metadata, close, install); on error it has
// already been removed.
func writeVerifiedTemp(root *os.Root, rel string, data []byte, sum string) (*os.File, string, os.FileInfo, *RPCError) {
	id, err := randomTempID()
	if err != nil {
		return nil, "", nil, &RPCError{Code: "internal_error", Message: err.Error()}
	}
	// Named like an upload's temp file, so an agent that dies mid-edit leaves
	// nothing a later transfer's stale-part sweep does not clean up.
	tempRel := rel + ".fleet-" + id + ".part"
	if filepath.Dir(tempRel) != filepath.Dir(rel) {
		return nil, "", nil, invalidTransferID()
	}
	if rerr := checkBlockedTransferPath(filepath.Join(root.Name(), tempRel)); rerr != nil {
		return nil, "", nil, rerr
	}
	maybeReapStalePartsRoot(root, filepath.Dir(rel), filepath.Base(tempRel), time.Now())
	tf, err := root.OpenFile(tempRel, os.O_RDWR|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return nil, "", nil, &RPCError{Code: "open_failed", Message: err.Error()}
	}
	created, err := tf.Stat()
	if err != nil {
		_ = tf.Close()
		return nil, "", nil, &RPCError{Code: "stat_failed", Message: err.Error()}
	}
	fail := func(code string, err error) (*os.File, string, os.FileInfo, *RPCError) {
		_ = tf.Close()
		removeIfSame(root, tempRel, created)
		return nil, "", nil, &RPCError{Code: code, Message: err.Error()}
	}
	if _, err := tf.Write(data); err != nil {
		return fail("write_failed", err)
	}
	if err := tf.Sync(); err != nil {
		return fail("sync_failed", err)
	}
	// Read the bytes back from the file — not from memory — and check them.
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(tf, 0, int64(len(data)))); err != nil {
		return fail("read_failed", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fail("verify_failed", fmt.Errorf("the temp file read back with sha256 %s, expected %s; nothing was changed", got, sum))
	}
	if st, err := tf.Stat(); err != nil || st.Size() != int64(len(data)) {
		return fail("verify_failed", fmt.Errorf("the temp file has the wrong size after writing; nothing was changed"))
	}
	return tf, tempRel, created, nil
}

// installEdit writes data (whose SHA-256 is sum) over the file rel, which is
// open as orig and was described by origInfo when it was read. The new file
// gets the original's owner, group, mode and extended attributes before it is
// renamed into place, and it is installed only if the original is still the
// file that was read. It returns the metadata it preserved.
func installEdit(root *os.Root, rel string, orig *os.File, origInfo os.FileInfo, data []byte, sum string) ([]string, *RPCError) {
	tf, tempRel, created, rerr := writeVerifiedTemp(root, rel, data, sum)
	if rerr != nil {
		return nil, rerr
	}
	fail := func(rerr *RPCError) ([]string, *RPCError) {
		_ = tf.Close()
		removeIfSame(root, tempRel, created)
		return nil, rerr
	}
	preserved, rerr := preserveMetadata(tf, orig, origInfo)
	if rerr != nil {
		return fail(rerr)
	}
	if err := tf.Sync(); err != nil {
		return fail(&RPCError{Code: "sync_failed", Message: err.Error()})
	}
	tempInfo, err := tf.Stat()
	if err != nil {
		return fail(&RPCError{Code: "stat_failed", Message: err.Error()})
	}
	if testHookBeforeEditInstall != nil {
		testHookBeforeEditInstall()
	}
	// Something outside Fleet may have written the file since it was read;
	// installing now would throw that change away.
	if cur, err := root.Lstat(rel); err != nil || !sameInode(cur, origInfo) || cur.Size() != origInfo.Size() || !cur.ModTime().Equal(origInfo.ModTime()) {
		return fail(&RPCError{Code: "edit_conflict", Message: "the file changed while the edit was being applied; nothing was changed — read it again and redo the edit"})
	}
	if err := tf.Close(); err != nil {
		removeIfSame(root, tempRel, created)
		return nil, &RPCError{Code: "close_failed", Message: err.Error()}
	}
	more, rerr := installEditedTemp(root, tempRel, rel, tempInfo)
	if rerr != nil {
		return nil, rerr
	}
	syncParentDir(root, rel)
	return append(preserved, more...), nil
}

// syncParentDir flushes the directory entry change of a rename to disk. It is
// best effort: some platforms and filesystems cannot fsync a directory.
func syncParentDir(root *os.Root, rel string) {
	if d, err := root.Open(filepath.Dir(rel)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// unixModeBits returns a file mode as the familiar octal permission bits,
// including set-uid (04000), set-gid (02000) and sticky (01000).
func unixModeBits(m os.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// editDiff renders the diff reported with an edit, or "" for binary content.
func editDiff(path string, before, after []byte) (string, bool) {
	if textedit.IsBinary(before) || textedit.IsBinary(after) {
		return "", false
	}
	label := strings.TrimPrefix(filepath.ToSlash(path), "/")
	a, b := "a/"+label, "b/"+label
	if before == nil {
		a = "/dev/null"
	}
	return textdiff.UnifiedLimit(a, b, string(before), string(after), maxEditDiffBytes)
}
