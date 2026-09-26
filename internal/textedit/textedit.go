// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

// Package textedit applies the text operations of a file.edit request
// (replace an exact snippet, insert lines) to a file's content. The agent runs
// it against the live file; it has no I/O of its own.
//
// The rules are the ones that make edits safe to hand to an automated agent:
// a replacement must match the file exactly and — unless every occurrence is
// asked for — exactly once, so an edit can never land somewhere the caller did
// not intend; a request applies completely or not at all; and a file's line
// endings are kept (text written with "\n" is converted for a CRLF file).
package textedit

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cenvero/fleet/pkg/proto"
)

// Error codes reported for edits that cannot be applied. They double as the
// agent's RPC error codes.
const (
	CodeInvalidEdit  = "invalid_edit"
	CodeNotFound     = "old_not_found"
	CodeNotUnique    = "old_not_unique"
	CodeBinaryFile   = "binary_file"
	CodeFileTooLarge = "file_too_large"
)

// Error is an edit that cannot be applied, with a message written for the
// person or agent who asked for it.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsBinary reports whether content looks like binary data (it holds a NUL
// byte). Text operations are refused on binary files.
func IsBinary(content []byte) bool {
	return bytes.IndexByte(content, 0) >= 0
}

// UsesCRLF reports whether every line break in content is "\r\n".
func UsesCRLF(content []byte) bool {
	crlf := bytes.Count(content, []byte("\r\n"))
	return crlf > 0 && crlf == bytes.Count(content, []byte("\n"))
}

// CountLines returns the number of lines in content; a final line without a
// newline counts.
func CountLines(content []byte) int {
	n := bytes.Count(content, []byte("\n"))
	if len(content) > 0 && content[len(content)-1] != '\n' {
		n++
	}
	return n
}

// Apply runs ops in order against content and returns the new content and the
// number of individual edits made. maxBytes bounds the result (0 = no bound).
// content is not modified.
func Apply(content []byte, ops []proto.FileEditOp, maxBytes int64) ([]byte, int, error) {
	if len(ops) == 0 {
		return nil, 0, errorf(CodeInvalidEdit, "no edit operations given")
	}
	if len(ops) > proto.MaxEditOps {
		return nil, 0, errorf(CodeInvalidEdit, "too many edit operations (%d; the limit is %d)", len(ops), proto.MaxEditOps)
	}
	if IsBinary(content) {
		return nil, 0, errorf(CodeBinaryFile, "the file looks binary (it contains NUL bytes); text edits are refused — replace the whole file instead")
	}
	crlf := UsesCRLF(content)
	buf := bytes.Clone(content)
	edits := 0
	for i, op := range ops {
		var n int
		var err *Error
		switch op.Kind {
		case proto.FileEditOpReplace, "":
			buf, n, err = replace(buf, op, crlf, maxBytes)
		case proto.FileEditOpInsert:
			buf, n, err = insert(buf, op, crlf)
		default:
			err = errorf(CodeInvalidEdit, "unknown edit kind %q (use %q or %q)", op.Kind, proto.FileEditOpReplace, proto.FileEditOpInsert)
		}
		if err != nil {
			if len(ops) > 1 {
				err.Message = fmt.Sprintf("edit %d of %d: %s", i+1, len(ops), err.Message)
			}
			return nil, 0, err
		}
		if maxBytes > 0 && int64(len(buf)) > maxBytes {
			return nil, 0, errorf(CodeFileTooLarge, "the edited file would be %d bytes, over the %d-byte limit", len(buf), maxBytes)
		}
		edits += n
	}
	return buf, edits, nil
}

// withLineEndings converts "\n" line breaks in s to the file's style. Text that
// already carries "\r" is taken as written.
func withLineEndings(s string, crlf bool) string {
	if !crlf || strings.Contains(s, "\r") {
		return s
	}
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func replace(buf []byte, op proto.FileEditOp, crlf bool, maxBytes int64) ([]byte, int, *Error) {
	if op.Old == "" {
		return nil, 0, errorf(CodeInvalidEdit, "the text to replace is empty (use an insert to add text)")
	}
	if op.Old == op.New {
		return nil, 0, errorf(CodeInvalidEdit, "the replacement is identical to the text it replaces")
	}
	old := []byte(withLineEndings(op.Old, crlf))
	repl := []byte(withLineEndings(op.New, crlf))
	count := bytes.Count(buf, old)
	switch {
	case count == 0:
		msg := "the text to replace was not found; it must match the file exactly, including whitespace, indentation and line breaks — view the file again and copy the text from it"
		if looseMatch(buf, old) {
			msg = "the text to replace was not found exactly, but it does appear with different indentation or trailing whitespace — copy it from the file as it is"
		}
		return nil, 0, &Error{Code: CodeNotFound, Message: msg}
	case count > 1 && !op.All:
		return nil, 0, errorf(CodeNotUnique, "the text to replace occurs %d times; include more surrounding lines so it matches exactly once, or replace all occurrences", count)
	}
	n := 1
	if op.All {
		n = count
	}
	// Check the size before building the result: replacing many occurrences
	// of a short text with a long one could otherwise ask for far more memory
	// than any file may hold. (n ≤ len(buf) and the texts are bounded by the
	// request size, so this cannot overflow int64.)
	if size := int64(len(buf)) + int64(n)*(int64(len(repl))-int64(len(old))); maxBytes > 0 && size > maxBytes {
		return nil, 0, errorf(CodeFileTooLarge, "the edited file would be %d bytes, over the %d-byte limit", size, maxBytes)
	}
	if op.All {
		return bytes.ReplaceAll(buf, old, repl), count, nil
	}
	return bytes.Replace(buf, old, repl, 1), 1, nil
}

// looseMatch reports whether old occurs in buf once leading and trailing
// whitespace is ignored on every line — the usual reason an exact match fails.
func looseMatch(buf, old []byte) bool {
	squash := func(b []byte) string {
		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		for i, l := range lines {
			lines[i] = strings.TrimSpace(l)
		}
		return strings.Join(lines, "\n")
	}
	needle := strings.Trim(squash(old), "\n")
	return needle != "" && strings.Contains(squash(buf), needle)
}

func insert(buf []byte, op proto.FileEditOp, crlf bool) ([]byte, int, *Error) {
	if op.Text == "" {
		return nil, 0, errorf(CodeInvalidEdit, "the text to insert is empty")
	}
	lines := CountLines(buf)
	if op.Line < 0 || op.Line > lines {
		return nil, 0, errorf(CodeInvalidEdit, "cannot insert after line %d: the file has %d lines (use 0 to insert at the top)", op.Line, lines)
	}
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	text := withLineEndings(op.Text, crlf)
	if !strings.HasSuffix(text, "\n") {
		text += nl
	}
	// offset is where line op.Line ends (just past its newline).
	offset := 0
	for range op.Line {
		i := bytes.IndexByte(buf[offset:], '\n')
		if i < 0 {
			offset = len(buf)
			break
		}
		offset += i + 1
	}
	if offset == len(buf) && len(buf) > 0 && buf[len(buf)-1] != '\n' {
		// Appending after a last line that has no newline: break that line,
		// and keep the file's "no newline at the end" style.
		text = nl + strings.TrimSuffix(text, nl)
	}
	out := make([]byte, 0, len(buf)+len(text))
	out = append(out, buf[:offset]...)
	out = append(out, text...)
	out = append(out, buf[offset:]...)
	return out, 1, nil
}
