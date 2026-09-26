// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Preview limits. Text previews read only a bounded head of the file; images
// are refused above maxPreviewImageBytes so a preview can never become a bulk
// transfer.
const (
	maxPreviewTextBytes  = 256 << 10 // 256 KiB
	maxPreviewImageBytes = 25 << 20  // 25 MiB
)

// previewImageTypes is the allowlist of raster formats rendered inline. SVG is
// deliberately absent: it is a document format that can carry script, so it
// is only ever previewed as escaped text. Nothing else is served inline.
var previewImageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",
	".avif": "image/avif",
}

// errPreviewStop ends a bounded remote read once enough bytes arrived.
var errPreviewStop = errors.New("preview limit reached")

// headWriter keeps the first limit bytes and then stops the reader.
type headWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (h *headWriter) Write(p []byte) (int, error) {
	room := h.limit - h.buf.Len()
	if len(p) > room {
		h.buf.Write(p[:max(room, 0)])
		h.truncated = true
		return 0, errPreviewStop
	}
	h.buf.Write(p)
	return len(p), nil
}

// handlePreview serves a size-capped preview:
//
//	kind=text  → JSON {content, size, truncated, binary}; never HTML.
//	kind=image → raw bytes of an allowlisted raster image with its exact
//	             Content-Type (plus the global nosniff + strict CSP).
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	server, p, kind := q.Get("server"), q.Get("path"), q.Get("kind")
	if p == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	switch kind {
	case "text", "":
		s.previewText(w, server, p)
	case "image":
		s.previewImage(w, r, server, p)
	default:
		http.Error(w, "kind must be text or image", http.StatusBadRequest)
	}
}

func (s *Server) previewText(w http.ResponseWriter, server, p string) {
	var (
		head      []byte
		size      int64
		truncated bool
	)
	if server == "" {
		clean, err := s.cleanLocal(p)
		if err != nil {
			writeError(w, err)
			return
		}
		// codeql[go/path-injection] web file manager: loopback-only, per-process token, same-origin POST; protected controller paths are refused by cleanLocal/cleanLocalWrite before this, and acting on operator-chosen local paths is its purpose
		f, err := os.Open(clean) // #nosec G304,G703 -- operator-selected local path; config dir refused by cleanLocal
		if err != nil {
			writeError(w, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			writeError(w, err)
			return
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
			return
		}
		size = info.Size()
		head, err = io.ReadAll(io.LimitReader(f, maxPreviewTextBytes+1))
		if err != nil {
			writeError(w, err)
			return
		}
	} else {
		stat, err := s.files.StatRemoteFile(server, p)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := requireRegularEntry(stat.Entry, p); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		size = stat.Entry.Size
		hw := &headWriter{limit: maxPreviewTextBytes + 1}
		if _, err := s.files.CatRemoteFile(server, p, hw); err != nil && !errors.Is(err, errPreviewStop) {
			writeError(w, err)
			return
		}
		head = hw.buf.Bytes()
	}
	if len(head) > maxPreviewTextBytes {
		head = head[:maxPreviewTextBytes]
		truncated = true
	}
	if int64(len(head)) < size {
		truncated = true
	}
	if looksBinary(head) {
		writeJSON(w, map[string]any{"binary": true, "size": size, "content": "", "truncated": truncated})
		return
	}
	// Never split a multi-byte rune at the cut: drop an incomplete final rune.
	if truncated {
		i := len(head) - 1
		for i > 0 && len(head)-i < utf8.UTFMax && !utf8.RuneStart(head[i]) {
			i--
		}
		if i >= 0 && !utf8.FullRune(head[i:]) {
			head = head[:i]
		}
	}
	writeJSON(w, map[string]any{
		"binary":    false,
		"size":      size,
		"content":   strings.ToValidUTF8(string(head), "�"),
		"truncated": truncated,
	})
}

func (s *Server) previewImage(w http.ResponseWriter, r *http.Request, server, p string) {
	base := p
	if server == "" {
		base = filepath.Base(p)
	} else if style, err := s.targetPathStyle(server); err == nil {
		base = style.Base(p)
	}
	ctype, ok := previewImageTypes[strings.ToLower(filepath.Ext(base))]
	if !ok {
		writeJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "not a previewable image type"})
		return
	}
	setHeaders := func(h http.Header) {
		h.Set("Content-Type", ctype)
		h.Set("Content-Disposition", contentDisposition("inline", base))
		// Belt and braces on top of the global CSP: even if this URL is opened
		// directly, nothing in it may run or load anything.
		h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; sandbox")
	}
	if server == "" {
		clean, err := s.cleanLocal(p)
		if err != nil {
			writeError(w, err)
			return
		}
		// codeql[go/path-injection] web file manager: loopback-only, per-process token, same-origin POST; protected controller paths are refused by cleanLocal/cleanLocalWrite before this, and acting on operator-chosen local paths is its purpose
		f, err := os.Open(clean) // #nosec G304,G703 -- operator-selected local path; config dir refused by cleanLocal
		if err != nil {
			writeError(w, err)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			writeError(w, err)
			return
		}
		if !info.Mode().IsRegular() {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "not a regular file"})
			return
		}
		if info.Size() > maxPreviewImageBytes {
			writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("image too large to preview (limit %d MiB)", maxPreviewImageBytes>>20)})
			return
		}
		setHeaders(w.Header())
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		_, _ = io.Copy(w, io.LimitReader(f, info.Size()))
		return
	}
	stat, err := s.files.StatRemoteFile(server, p)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := requireRegularEntry(stat.Entry, p); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if stat.Entry.Size > maxPreviewImageBytes {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("image too large to preview (limit %d MiB)", maxPreviewImageBytes>>20)})
		return
	}
	if _, err := s.streamRemote(r.Context(), w, server, p, stat.Entry, setHeaders, nil, false); err != nil {
		var abort *streamAbort
		if errors.As(err, &abort) {
			panic(http.ErrAbortHandler)
		}
		writeError(w, err)
	}
}
