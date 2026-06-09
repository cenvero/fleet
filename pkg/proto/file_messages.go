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
)

// FileEntry describes a single directory entry on a managed node.
type FileEntry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Mode      uint32    `json:"mode"`
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
	Path    string      `json:"path"`
	Entries []FileEntry `json:"entries"`
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
}

type FileReadResult struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Data   []byte `json:"data"`   // JSON base64-encodes []byte transparently
	SHA256 string `json:"sha256"` // checksum of this chunk's raw bytes
	EOF    bool   `json:"eof,omitempty"`
}

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
}

type FileWritePayload struct {
	TransferID string `json:"transfer_id"`
	Path       string `json:"path"`
	Offset     int64  `json:"offset"`
	Data       []byte `json:"data"`
	SHA256     string `json:"sha256"` // chunk checksum, verified before WriteAt
}

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
}

type FileFinalizeResult struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
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
