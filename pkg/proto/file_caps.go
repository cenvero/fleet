// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// File-transfer capabilities advertised in the agent hello. Each gates one
// optional protocol feature; a controller that does not see a capability keeps
// using the original RPCs, so any controller/agent version pair interoperates.
const (
	// CapabilityFileChunkDigests: the agent remembers the checksum of every
	// chunk it verified and wrote, so file.finalize can check the assembled
	// file against FileFinalizePayload.ChunkDigest instead of re-reading and
	// re-hashing it, file.open_write reports already-verified chunks for
	// resume (FileOpenWriteResult.Chunks), and file.finalize understands
	// Abort.
	CapabilityFileChunkDigests = "file.chunk-digests"
	// CapabilityFilePut: the agent implements file.put, a one-round-trip
	// upload for files that fit in one chunk.
	CapabilityFilePut = "file.put"
	// CapabilityFileReadStat: file.read honours FileReadPayload.Stat.
	CapabilityFileReadStat = "file.read-stat"
	// CapabilityFileCopy: the agent implements file.copy for same-server
	// copies that never leave the host.
	CapabilityFileCopy = "file.copy"
	// CapabilityFileTree: the agent implements file.tree, a bounded
	// recursive listing in one round trip.
	CapabilityFileTree = "file.tree"
)

// chunkListDigestDomain separates chunk-list digests from any other SHA-256 in
// the system: a chunk-list digest can never be mistaken for, or collide by
// construction with, the SHA-256 of a file's contents.
const chunkListDigestDomain = "cenvero-fleet chunk-list v1\n"

// ChunkListDigest returns the digest that identifies a file by its ordered
// chunk checksums: SHA-256 over a domain tag, the total size, and every
// chunk's offset, length and raw SHA-256. The chunks must be sorted by offset
// and tile [0,total) exactly — no gaps, no overlaps, no empty chunks — and
// every checksum must be a hex SHA-256; anything else is an error, so a digest
// can only be produced for a complete, unambiguous chunk plan.
//
// Because each chunk checksum is a cryptographic SHA-256 of that chunk's
// bytes, two files with the same chunk-list digest have identical contents.
// Both transfer peers already hash every chunk once, so comparing chunk-list
// digests verifies a whole transfer without either side re-reading the file.
func ChunkListDigest(total int64, chunks []FileRangeChecksum) (string, error) {
	if total < 0 {
		return "", fmt.Errorf("invalid total size %d", total)
	}
	h := sha256.New()
	var buf [8 + 8 + sha256.Size]byte
	_, _ = h.Write([]byte(chunkListDigestDomain))
	binary.BigEndian.PutUint64(buf[:8], uint64(total)) // #nosec G115 -- total >= 0 checked above
	_, _ = h.Write(buf[:8])
	var next int64
	for _, c := range chunks {
		if c.Offset != next {
			return "", fmt.Errorf("chunk at offset %d does not continue at %d", c.Offset, next)
		}
		if c.Length <= 0 || c.Length > total-c.Offset {
			return "", fmt.Errorf("chunk at offset %d has invalid length %d", c.Offset, c.Length)
		}
		sum, err := hex.DecodeString(c.SHA256)
		if err != nil || len(sum) != sha256.Size {
			return "", fmt.Errorf("chunk at offset %d has an invalid sha256", c.Offset)
		}
		binary.BigEndian.PutUint64(buf[:8], uint64(c.Offset))   // #nosec G115 -- non-negative by construction
		binary.BigEndian.PutUint64(buf[8:16], uint64(c.Length)) // #nosec G115 -- positive, checked above
		copy(buf[16:], sum)
		_, _ = h.Write(buf[:])
		next = c.Offset + c.Length
	}
	if next != total {
		return "", fmt.Errorf("chunks cover %d of %d bytes", next, total)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
