// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

// --- in-place editing (CapabilityFileEdit) ---
//
// file.edit changes one file on the agent host in a single round trip. The
// agent reads the file, applies the edit, and installs the result the way a
// careful editor saves: into a private temp file beside the original, fsynced
// and read back to verify its SHA-256, given the original's owner, group,
// permission bits and extended attributes, then renamed over the original in
// one atomic step. The request is only acted on once it has arrived whole, so
// a connection that drops mid-request changes nothing, and the file is never
// left half-written: readers see either the old bytes or the new ones.

// ActionFileEdit is the RPC action for in-place file edits.
const ActionFileEdit = "file.edit"

// CapabilityFileEdit is advertised by agents that implement file.edit.
const CapabilityFileEdit = "file.edit"

// MaxEditFileBytes bounds the size of a file file.edit loads. The agent holds
// the original and the edited copy in memory, and the original can travel back
// in the reply for the controller's undo history.
const MaxEditFileBytes = MaxRawChunkBytes

// MaxEditOps bounds how many operations one file.edit request may carry.
const MaxEditOps = 1000

// File edit operation kinds.
const (
	// FileEditOpReplace replaces Old with New. Old must occur exactly once
	// unless All is set, in which case every occurrence is replaced.
	FileEditOpReplace = "replace"
	// FileEditOpInsert inserts Text after line Line (1-based; 0 inserts at
	// the top of the file).
	FileEditOpInsert = "insert"
)

// FileEditOp is one text change. Operations are applied in order to the result
// of the previous one, and a request succeeds or fails as a whole.
type FileEditOp struct {
	Kind string `json:"kind"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
	All  bool   `json:"all,omitempty"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// FileEditPayload asks the agent to edit Path. Exactly one of Ops or Replace
// describes the change.
type FileEditPayload struct {
	Path string `json:"path"`
	// EditID makes the request idempotent: an agent that already applied an
	// edit with this id answers a retry with the first result instead of
	// applying it twice (a controller retries after losing a reply).
	EditID string `json:"edit_id,omitempty"`
	// BaseSHA256, when set, is the SHA-256 the file must have right now. The
	// edit is refused with code "edit_conflict" if the file changed since the
	// caller read it.
	BaseSHA256 string       `json:"base_sha256,omitempty"`
	Ops        []FileEditOp `json:"ops,omitempty"`
	// Replace installs Content as the file's whole new content.
	// ContentSHA256 (required) is checked against the bytes received.
	Replace       bool   `json:"replace,omitempty"`
	Content       []byte `json:"content,omitempty"` // empty when carried as a binary frame
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// Create (with Replace) creates a file that must not exist yet, with Mode
	// (default 0644) and the agent's own owner.
	Create bool   `json:"create,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	// DryRun computes the result and diff without writing anything.
	DryRun bool `json:"dry_run,omitempty"`
	// MaxBytes lowers the size limit below MaxEditFileBytes (0 = the maximum).
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// ReturnOriginal asks for the file's previous content in the reply (for
	// the controller's undo history).
	ReturnOriginal bool `json:"return_original,omitempty"`
	// Binary asks for the reply's bulk bytes (Original) as a binary frame.
	Binary bool `json:"binary,omitempty"`
}

// TakeBinary/PutBinary let Content ride as a binary frame attachment.
func (p *FileEditPayload) TakeBinary() []byte {
	data := p.Content
	p.Content = nil
	return data
}

func (p *FileEditPayload) PutBinary(b []byte) { p.Content = b }

// WantsBinary reports whether the reply's Original may be a binary frame.
func (p FileEditPayload) WantsBinary() bool { return p.Binary }

// FileEditResult reports what file.edit did.
type FileEditResult struct {
	// Path is the file that was edited. When the requested path is a
	// symlink, the edit goes to its target (the link itself is kept), and
	// Path names that target.
	Path    string `json:"path"`
	Changed bool   `json:"changed"`
	Created bool   `json:"created,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
	// Edits counts the individual changes applied (every replaced
	// occurrence and every insert).
	Edits     int    `json:"edits,omitempty"`
	OldSHA256 string `json:"old_sha256,omitempty"`
	NewSHA256 string `json:"new_sha256"`
	OldSize   int64  `json:"old_size"`
	NewSize   int64  `json:"new_size"`
	// Mode, Owner and Group describe the file as installed ("root", or a
	// numeric id when the name cannot be resolved; empty on Windows).
	Mode  uint32 `json:"mode"`
	Owner string `json:"owner,omitempty"`
	Group string `json:"group,omitempty"`
	// Preserved lists the metadata carried over from the original
	// ("owner", "mode", "xattrs", "acl", "symlink").
	Preserved []string `json:"preserved,omitempty"`
	// Verified reports that the bytes written were read back from disk and
	// matched NewSHA256 before they were installed.
	Verified      bool   `json:"verified,omitempty"`
	Diff          string `json:"diff,omitempty"`
	DiffTruncated bool   `json:"diff_truncated,omitempty"`
	// Original is the previous content, returned only when ReturnOriginal
	// was set and the file changed.
	Original []byte `json:"original,omitempty"`
}

// TakeBinary/PutBinary let Original ride as a binary frame attachment.
func (r *FileEditResult) TakeBinary() []byte {
	data := r.Original
	r.Original = nil
	return data
}

func (r *FileEditResult) PutBinary(b []byte) { r.Original = b }
