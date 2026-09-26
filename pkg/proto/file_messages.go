// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import "time"

// MaxRawChunkBytes bounds the raw (pre-base64) size of a single file chunk
// carried in a file.write / file.read envelope. JSON base64-encodes []byte
// fields (~+33%), so an 8 MiB raw chunk becomes ~11 MiB on the wire — safely
// under MaxEnvelopeSize (16 MiB) with room for the surrounding envelope JSON.
// Both the agent and the controller transfer engine enforce this ceiling.
const MaxRawChunkBytes = 8 * 1024 * 1024

// File transfer / browsing RPC action names. All lowercase to match the
// agent dispatch switch (strings.ToLower(request.Action)).
const (
	ActionFileList      = "file.list"
	ActionFileStat      = "file.stat"
	ActionFileRead      = "file.read"
	ActionFileOpenWrite = "file.open_write"
	ActionFileWrite     = "file.write"
	ActionFileFinalize  = "file.finalize"
	ActionFileProbe     = "file.probe"
	ActionFileMkdir     = "file.mkdir"
	ActionFileDelete    = "file.delete"
	ActionFileRename    = "file.rename"
	ActionFilePut       = "file.put"
	ActionFileCopy      = "file.copy"
	ActionFileTree      = "file.tree"
)

// FileEntryType values classify directory entries independently of permission
// bits. Older agents omit Type; current agents always populate it.
const (
	FileEntryTypeRegular   = "file"
	FileEntryTypeDirectory = "directory"
	FileEntryTypeSymlink   = "symlink"
	FileEntryTypeOther     = "other"
)

// FileEntry describes a single directory entry on a managed node.
type FileEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Mode      uint32    `json:"mode"`
	Type      string    `json:"type,omitempty"`
	IsDir     bool      `json:"is_dir"`
	IsSymlink bool      `json:"is_symlink,omitempty"`
	ModTime   time.Time `json:"mod_time"`
}

// --- listing / stat ---

type FileListPayload struct {
	Path       string `json:"path"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
}

type FileListResult struct {
	Path            string      `json:"path"`
	Entries         []FileEntry `json:"entries"`
	CaseInsensitive *bool       `json:"case_insensitive,omitempty"`
}

type FileStatPayload struct {
	Path string `json:"path"`
}

type FileStatResult struct {
	Entry FileEntry `json:"entry"`
}

// --- download (agent reads a byte range and ships it to the controller) ---

type FileReadPayload struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	// Binary asks the agent to return the chunk as a binary frame attachment
	// rather than base64 inside the JSON envelope. The controller sets it only
	// for agents advertising CapabilityBinaryFrames, so older agents — which
	// ignore the unknown field — keep replying in the original encoding.
	Binary bool `json:"binary,omitempty"`
	// Stat asks the agent to also return the path's metadata (exactly what
	// file.stat reports), so a download can plan from — and a small file can be
	// fetched in — a single round trip. Sent only to agents advertising
	// CapabilityFileReadStat; older agents ignore it and omit Entry.
	Stat bool `json:"stat,omitempty"`
}

// WantsBinary implements proto.BinaryRequester.
func (p FileReadPayload) WantsBinary() bool { return p.Binary }

type FileReadResult struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Data   []byte `json:"data,omitempty"` // empty when the bytes travel as a binary frame
	SHA256 string `json:"sha256"`         // checksum of this chunk's raw bytes
	EOF    bool   `json:"eof,omitempty"`
	// Entry is the file's metadata, present only when the request set Stat.
	Entry *FileEntry `json:"entry,omitempty"`
}

// TakeBinary/PutBinary let a chunk's bytes ride as a binary frame attachment
// instead of base64 inside the JSON envelope. See proto.BinaryCarrier.
func (r *FileReadResult) TakeBinary() []byte {
	data := r.Data
	r.Data = nil
	return data
}

func (r *FileReadResult) PutBinary(b []byte) { r.Data = b }

// --- upload (controller ships byte ranges down to the agent) ---

type FileOpenWritePayload struct {
	Path       string `json:"path"`
	TotalSize  int64  `json:"total_size"`
	Mode       uint32 `json:"mode,omitempty"` // final mode; default 0o644
	TransferID string `json:"transfer_id"`    // ties the temp file across channels
}

type FileOpenWriteResult struct {
	TempPath     string `json:"temp_path"`
	ResumeOffset int64  `json:"resume_offset"` // current size of the temp file
	// Chunks lists byte ranges of the temp file whose content the agent has
	// already verified against a chunk checksum (written and checked by this
	// agent process, or made durable before a restart). A resuming controller
	// compares them with its own chunk digests instead of asking the agent to
	// re-read and re-hash the prefix. Omitted by agents without
	// CapabilityFileChunkDigests; ranges it does not cover are still verified
	// with file.probe.
	Chunks []FileRangeChecksum `json:"chunks,omitempty"`
}

type FileWritePayload struct {
	TransferID string `json:"transfer_id"`
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	Data       []byte `json:"data,omitempty"` // empty when the bytes travel as a binary frame
	SHA256     string `json:"sha256"`         // chunk checksum, verified before WriteAt
}

// TakeBinary/PutBinary let a chunk's bytes ride as a binary frame attachment
// instead of base64 inside the JSON envelope. See proto.BinaryCarrier.
func (p *FileWritePayload) TakeBinary() []byte {
	data := p.Data
	p.Data = nil
	return data
}

func (p *FileWritePayload) PutBinary(b []byte) { p.Data = b }

type FileWriteResult struct {
	Offset       int64 `json:"offset"`
	BytesWritten int64 `json:"bytes_written"`
}

type FileFinalizePayload struct {
	TransferID  string `json:"transfer_id"`
	Path        string `json:"path"`
	Mode        uint32 `json:"mode,omitempty"`
	WholeSHA256 string `json:"whole_sha256"`
	TotalSize   int64  `json:"total_size"`
	// ChunkDigest is ChunkListDigest over the transfer's ordered chunk plan.
	// An agent advertising CapabilityFileChunkDigests verifies the assembled
	// file against the chunk checksums it already checked on every write, so
	// it does not re-read and re-hash the whole file here. Older agents ignore
	// it and verify WholeSHA256 by re-reading the file, exactly as before.
	ChunkDigest string `json:"chunk_digest,omitempty"`
	// Abort discards the upload: the agent closes and removes the temp file
	// (and its resume sidecar) instead of installing it. Only sent to agents
	// advertising CapabilityFileChunkDigests.
	Abort bool `json:"abort,omitempty"`
}

type FileFinalizeResult struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// ChunkDigest is set when the agent verified the upload through its chunk
	// checksums (see FileFinalizePayload.ChunkDigest) instead of re-hashing the
	// file; SHA256 then echoes the controller-supplied WholeSHA256.
	ChunkDigest string `json:"chunk_digest,omitempty"`
}

// --- resume probe (works for an in-flight upload temp or an existing file) ---

type FileRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type FileRangeChecksum struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	SHA256 string `json:"sha256"`
}

type FileProbePayload struct {
	Path       string      `json:"path"`
	TransferID string      `json:"transfer_id,omitempty"`
	Ranges     []FileRange `json:"ranges,omitempty"`
}

type FileProbeResult struct {
	Exists         bool                `json:"exists"`
	CurrentSize    int64               `json:"current_size"`
	RangeChecksums []FileRangeChecksum `json:"range_checksums,omitempty"`
}

// --- mkdir / delete / rename ---

type FileMkdirPayload struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode,omitempty"`
}

type FileDeletePayload struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

type FileRenamePayload struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type FileOpResult struct {
	Path string `json:"path"`
}

// --- one-round-trip small-file upload (CapabilityFilePut) ---

// FilePutPayload uploads a whole file that fits in one chunk. The agent writes
// it to a private temp file beside the destination, verifies SHA256, syncs it
// and atomically renames it into place — the same guarantees as
// open_write/write/finalize, in one round trip instead of three. The result is
// a FileFinalizeResult.
type FilePutPayload struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode,omitempty"` // final mode; default 0o644
	Data   []byte `json:"data,omitempty"` // empty when the bytes travel as a binary frame
	SHA256 string `json:"sha256"`         // required: SHA-256 of Data
}

// TakeBinary/PutBinary let the file ride as a binary frame attachment.
func (p *FilePutPayload) TakeBinary() []byte {
	data := p.Data
	p.Data = nil
	return data
}

func (p *FilePutPayload) PutBinary(b []byte) { p.Data = b }

// --- same-server copy (CapabilityFileCopy) ---

// FileCopyPayload copies a regular file to another path on the same agent
// without relaying the bytes through the controller. The destination is
// written like an upload (private temp file, fsync, atomic rename) and the
// result is a FileFinalizeResult whose SHA256 is the digest of the bytes
// copied.
type FileCopyPayload struct {
	From string `json:"from"`
	To   string `json:"to"`
	Mode uint32 `json:"mode,omitempty"` // final mode; default: the source's permissions
}

// --- recursive listing (CapabilityFileTree) ---

// FileTreePayload asks for every entry beneath Path in one round trip. The
// agent never follows symlinks while walking and stops at MaxEntries/MaxDepth,
// reporting Truncated so the caller can fall back to per-directory listing.
type FileTreePayload struct {
	Path       string `json:"path"`
	ShowHidden bool   `json:"show_hidden,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
	MaxDepth   int    `json:"max_depth,omitempty"`
}

// FileTreeResult carries entries in the same form as FileListResult (absolute
// agent-side paths), parents before children.
type FileTreeResult struct {
	Path      string      `json:"path"` // the resolved root, as file.list reports it
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}
