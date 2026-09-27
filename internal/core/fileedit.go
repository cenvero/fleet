// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/textedit"
	"github.com/cenvero/fleet/pkg/proto"
)

// DefaultEditBackups is how many previous versions of each edited file the
// controller keeps for `fleet file edit --undo` when the config does not say.
const DefaultEditBackups = 10

// maxEditBackups bounds the edit-backups setting.
const maxEditBackups = 100

// FileEditSettings configures `fleet file edit` / `fleet file view` and the
// editors in the file managers. Zero values mean the defaults.
type FileEditSettings struct {
	// Backups is how many previous versions of each edited file are kept on
	// the controller for undo; nil means DefaultEditBackups and 0 turns the
	// history off.
	Backups *int `toml:"backups,omitempty" json:"backups,omitempty"`
	// MaxBytes lowers the largest file that can be viewed or edited below
	// proto.MaxEditFileBytes (0 = that maximum).
	MaxBytes int64 `toml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// RequireHash makes every non-interactive edit of an existing file state
	// the SHA-256 it expects the file to have (--expect-sha256), so an edit
	// can only apply to the exact version its author looked at.
	RequireHash bool `toml:"require_hash,omitempty" json:"require_hash,omitempty"`
}

// EffectiveBackups returns the number of versions kept per file.
func (s FileEditSettings) EffectiveBackups() int {
	if s.Backups == nil {
		return DefaultEditBackups
	}
	return min(max(*s.Backups, 0), maxEditBackups)
}

// EffectiveMaxBytes returns the size limit for viewing and editing.
func (s FileEditSettings) EffectiveMaxBytes() int64 {
	if s.MaxBytes > 0 && s.MaxBytes < proto.MaxEditFileBytes {
		return s.MaxBytes
	}
	return proto.MaxEditFileBytes
}

// Validate checks the settings.
func (s FileEditSettings) Validate() error {
	if s.Backups != nil && (*s.Backups < 0 || *s.Backups > maxEditBackups) {
		return fmt.Errorf("file edit backups must be between 0 and %d", maxEditBackups)
	}
	if s.MaxBytes < 0 || s.MaxBytes > proto.MaxEditFileBytes {
		return fmt.Errorf("file edit max size must be at most %s", humanBytes(proto.MaxEditFileBytes))
	}
	return nil
}

// FileEditSettings returns the controller's editing settings.
func (a *App) FileEditSettings() FileEditSettings { return a.Config.Runtime.FileEdit }

// EditRequest is one edit of a file on a managed server. Either Ops, or
// Replace with Content, describes the change.
type EditRequest struct {
	Path    string
	Ops     []proto.FileEditOp
	Replace bool
	Content []byte
	// Create (with Replace) creates a file that must not exist yet.
	Create bool
	Mode   uint32
	// BaseSHA256 is the SHA-256 the file must have for the edit to apply.
	BaseSHA256 string
	DryRun     bool
	// NoBackup skips the undo history for this edit (used by undo itself).
	NoBackup bool
	// InPlace writes the new content into the existing file instead of
	// replacing it with a new file — for files that cannot be replaced (the
	// agent answers "cannot_replace"): one bind-mounted into a container, one
	// in a folder the agent may not write, or one whose owner the agent
	// cannot give a new file. It is not atomic, so the file's current content
	// is saved on the controller before anything is written (see editInPlace).
	InPlace bool
}

// EditResult is what an edit did. BackupID names the undo-history entry
// recorded for it ("" when none was kept).
type EditResult struct {
	proto.FileEditResult
	Server    string `json:"server"`
	ModeOctal string `json:"mode_octal"`
	BackupID  string `json:"backup_id,omitempty"`
	// BackupWarning explains why no undo entry was kept for a change.
	BackupWarning string `json:"backup_warning,omitempty"`
}

// ErrEditUnsupported reports an agent too old to edit files in place.
var ErrEditUnsupported = errors.New("this server's agent does not support safe in-place editing yet")

// EditErrorCode returns the agent's error code for a failed edit (for example
// "edit_conflict", "old_not_found", "old_not_unique"), or "".
func EditErrorCode(err error) string { return remoteErrorCode(err) }

// EditRemoteFile applies req to a file on serverName through the agent's
// file.edit RPC: the agent applies the change and installs it atomically with
// the file's owner, group, mode and extended attributes, after checking the
// file still has req.BaseSHA256 (when given). A lost reply is retried with
// the same edit id, so an edit is never applied twice.
func (a *App) EditRemoteFile(serverName string, req EditRequest) (EditResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return EditResult{}, err
	}
	style := TargetPathStyleForServer(server)
	if err := rejectDotComponents(style, req.Path); err != nil {
		return EditResult{}, err
	}
	if err := ValidateTargetPath(style, req.Path); err != nil {
		return EditResult{}, err
	}
	settings := a.FileEditSettings()
	limit := settings.EffectiveMaxBytes()
	if req.Replace && int64(len(req.Content)) > limit {
		return EditResult{}, fmt.Errorf("the new content is %s, over the %s edit limit", humanBytes(int64(len(req.Content))), humanBytes(limit))
	}
	if req.InPlace && req.Create {
		return EditResult{}, fmt.Errorf("an in-place edit writes a file that exists; --create makes a new one")
	}
	// (Undo — NoBackup — restores a version this controller saved itself.)
	if settings.RequireHash && !req.Create && !req.DryRun && !req.NoBackup && req.BaseSHA256 == "" {
		return EditResult{}, fmt.Errorf("this controller requires the file's expected sha256 for every edit (runtime.file_edit.require_hash); run `fleet file view %s %s` and pass --expect-sha256", serverName, req.Path)
	}
	editID, err := newEditID()
	if err != nil {
		return EditResult{}, err
	}
	backups := settings.EffectiveBackups()
	if req.NoBackup {
		backups = 0
	}

	conn, err := a.openTransferConn(server, 1)
	if err != nil {
		return EditResult{}, err
	}
	defer conn.closeFn()
	payload := &proto.FileEditPayload{
		Path:           req.Path,
		EditID:         editID,
		BaseSHA256:     strings.ToLower(req.BaseSHA256),
		Ops:            req.Ops,
		Replace:        req.Replace,
		Create:         req.Create,
		Mode:           req.Mode,
		DryRun:         req.DryRun,
		MaxBytes:       limit,
		ReturnOriginal: backups > 0 && !req.DryRun && !req.Create,
		InPlace:        req.InPlace,
		Binary:         conn.binaryFrames,
	}
	var contentSum string
	if req.Replace {
		contentSum = sha256Hex(req.Content)
		payload.Content = req.Content
		payload.ContentSHA256 = contentSum
	}
	if req.InPlace && !req.DryRun {
		return a.editInPlace(serverName, conn, req, payload, backups)
	}
	res, err := a.sendEdit(serverName, conn, payload)
	if err != nil {
		if !req.DryRun {
			a.auditEdit(serverName, req, EditResult{}, err)
		}
		return EditResult{}, err
	}
	if req.Replace && res.NewSHA256 != contentSum {
		err := fmt.Errorf("the agent reported sha256 %s for the new content, but %s was sent", res.NewSHA256, contentSum)
		a.auditEdit(serverName, req, EditResult{}, err)
		return EditResult{}, err
	}
	out := EditResult{FileEditResult: res, Server: serverName, ModeOctal: fmt.Sprintf("%04o", res.Mode&0o7777)}
	out.Original = nil
	if res.Changed && !res.DryRun {
		switch {
		case backups == 0 || req.Create:
		case res.Original == nil:
			out.BackupWarning = "the previous version could not be kept for undo (the agent answered a retried request)"
		default:
			id, err := a.editHistory().record(editHistoryEntry{
				Server:       serverName,
				Path:         req.Path,
				ResolvedPath: res.Path,
				Operator:     a.operator(),
				OldSHA256:    res.OldSHA256,
				NewSHA256:    res.NewSHA256,
				OldSize:      res.OldSize,
			}, res.Original, backups)
			if err != nil {
				out.BackupWarning = "the previous version could not be kept for undo: " + err.Error()
			} else {
				out.BackupID = id
			}
		}
		a.auditEdit(serverName, req, out, nil)
	}
	return out, nil
}

// sendEdit sends one file.edit request (a copy of payload) and decodes the
// agent's answer; a lost reply is retried with the same edit id.
func (a *App) sendEdit(serverName string, conn *transferConn, payload *proto.FileEditPayload) (proto.FileEditResult, error) {
	p := *payload
	env := proto.Envelope{Action: proto.ActionFileEdit, Payload: &p}
	if conn.binaryFrames {
		env = proto.DetachBinary(env)
	}
	res, err := decodeResult[proto.FileEditResult](conn.callRetry(0, env))
	if err != nil && remoteErrorCode(err) == errCodeUnsupportedAction {
		err = fmt.Errorf("%w: update it with `fleet agent update %s`", ErrEditUnsupported, serverName)
	}
	return res, err
}

// editInPlace makes an InPlace edit. Writing into the file is not atomic, so
// the file's current content is saved on the controller first: a dry run
// returns it together with what the edit will produce, it is recorded in the
// undo history, and only then is the write sent — pinned (BaseSHA256) to
// exactly the content that was saved. The agent writes the original back if
// its write fails; if even that fails, or the agent is lost mid-write, the
// saved version stays for `fleet file edit --undo --force`.
func (a *App) editInPlace(serverName string, conn *transferConn, req EditRequest, payload *proto.FileEditPayload, backups int) (EditResult, error) {
	dry := *payload
	dry.DryRun = true
	dry.ReturnOriginal = !req.NoBackup
	plan, err := a.sendEdit(serverName, conn, &dry)
	if err != nil {
		a.auditEdit(serverName, req, EditResult{}, err)
		return EditResult{}, err
	}
	result := func(r proto.FileEditResult) EditResult {
		r.Original = nil
		return EditResult{FileEditResult: r, Server: serverName, ModeOctal: fmt.Sprintf("%04o", r.Mode&0o7777)}
	}
	if !plan.Changed {
		out := result(plan)
		out.DryRun = false
		return out, nil
	}

	h := a.editHistory()
	var saved string
	if !req.NoBackup {
		if plan.Original == nil || sha256Hex(plan.Original) != plan.OldSHA256 {
			err := fmt.Errorf("the agent did not return the file's current content, so it could not be saved before writing in place; nothing was changed")
			a.auditEdit(serverName, req, EditResult{}, err)
			return EditResult{}, err
		}
		// With the history off, keep the other entries (an earlier
		// interrupted in-place write may still need its saved version);
		// this one is dropped again once the write succeeds.
		keep := backups
		if keep < 1 {
			keep = maxEditBackups
		}
		saved, err = h.record(editHistoryEntry{
			Server:       serverName,
			Path:         req.Path,
			ResolvedPath: plan.Path,
			Operator:     a.operator(),
			OldSHA256:    plan.OldSHA256,
			NewSHA256:    plan.NewSHA256,
			OldSize:      plan.OldSize,
			InPlace:      true,
		}, plan.Original, keep)
		if err != nil {
			err = fmt.Errorf("could not save the file's current version before writing it in place (%v); nothing was changed", err)
			a.auditEdit(serverName, req, EditResult{}, err)
			return EditResult{}, err
		}
	}
	forget := func() {
		if saved != "" {
			_ = h.forget(serverName, plan.Path, saved)
		}
	}

	write := *payload
	write.BaseSHA256 = plan.OldSHA256
	write.ReturnOriginal = false
	res, err := a.sendEdit(serverName, conn, &write)
	var out EditResult
	switch code := remoteErrorCode(err); {
	case err == nil:
		out = result(res)
	case code == "write_incomplete":
		// The agent could not put the original back: keep the saved copy.
		a.auditEdit(serverName, req, EditResult{}, err)
		return EditResult{}, err
	case code != "" || errors.Is(err, ErrEditUnsupported):
		// The agent answered: nothing was written.
		forget()
		a.auditEdit(serverName, req, EditResult{}, err)
		return EditResult{}, err
	default:
		// No answer: find out what the file holds now.
		cur, perr := a.remoteFileSHA256(serverName, plan.Path)
		switch {
		case perr == nil && cur == plan.NewSHA256:
			out = result(plan)
			out.DryRun, out.InPlace, out.Verified = false, true, true
			out.BackupWarning = "the agent's reply was lost, but the file was read back and holds the new content"
		case perr == nil && cur == plan.OldSHA256:
			forget()
			err = fmt.Errorf("%w; the file is unchanged", err)
			a.auditEdit(serverName, req, EditResult{}, err)
			return EditResult{}, err
		default:
			state := "its content could not be read back"
			if perr == nil {
				state = "it now matches neither the old nor the new content (sha256 " + cur + ")"
			}
			err = fmt.Errorf("the connection was lost while the file was being written in place (%v), and %s. The version from before the edit is saved on this controller: once the server is reachable, check the file with `fleet file view %s %s`, and if the edit did not complete, restore it with `fleet file edit %s %s --undo --force`", err, state, serverName, req.Path, serverName, req.Path)
			a.auditEdit(serverName, req, EditResult{}, err)
			return EditResult{}, err
		}
	}
	if backups > 0 && saved != "" {
		out.BackupID = saved
	} else {
		forget()
	}
	a.auditEdit(serverName, req, out, nil)
	return out, nil
}

func (a *App) auditEdit(serverName string, req EditRequest, res EditResult, err error) {
	action := "file.edit"
	details := req.Path
	if req.Create {
		details += " create=true"
	}
	if req.InPlace {
		details += " in_place=true"
	}
	if err != nil {
		action += ".failed"
		details += " error=" + err.Error()
	} else {
		details += fmt.Sprintf(" sha256=%s->%s edits=%d", shortSum(res.OldSHA256), shortSum(res.NewSHA256), res.Edits)
	}
	_ = a.AuditLog.Append(logs.AuditEntry{Action: action, Target: serverName, Operator: a.operator(), Details: details})
}

func shortSum(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func newEditID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "edit-" + hex.EncodeToString(b[:]), nil
}

// FileView is a text file read for viewing or editing.
type FileView struct {
	Server string `json:"server"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	// ModeOctal is Mode as octal permission bits, e.g. "0644".
	ModeOctal string          `json:"mode_octal"`
	Lines     int             `json:"lines"`
	CRLF      bool            `json:"crlf,omitempty"`
	Content   []byte          `json:"-"`
	Entry     proto.FileEntry `json:"-"`
}

// permBits converts a mode as agents report it (Go's os.FileMode bits) to
// octal permission bits, including set-uid, set-gid and sticky.
func permBits(mode uint32) uint32 {
	m := os.FileMode(mode)
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

// errViewTooLarge stops a view that would exceed its size limit.
var errViewTooLarge = errors.New("view limit exceeded")

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if int64(l.buf.Len())+int64(len(p)) > l.limit {
		return 0, errViewTooLarge
	}
	return l.buf.Write(p)
}

// ViewRemoteFile reads a text file from serverName (each chunk
// checksum-verified) and returns its content with the SHA-256 an edit can
// name as its expected base. Files over the edit size limit and binary files
// are refused.
func (a *App) ViewRemoteFile(serverName, remotePath string) (FileView, error) {
	limit := a.FileEditSettings().EffectiveMaxBytes()
	var lb limitedBuffer
	lb.limit = limit
	_, stat, err := a.catRemoteFile(serverName, remotePath, &lb)
	if errors.Is(err, errViewTooLarge) {
		return FileView{}, fmt.Errorf("%s is larger than the %s view/edit limit; use `fleet file tail` or `fleet file download`", remotePath, humanBytes(limit))
	}
	if err != nil {
		return FileView{}, err
	}
	content := lb.buf.Bytes()
	if textedit.IsBinary(content) {
		return FileView{}, fmt.Errorf("%s looks like a binary file; use `fleet file download` instead", remotePath)
	}
	return FileView{
		Server:    serverName,
		Path:      remotePath,
		SHA256:    sha256Hex(content),
		Size:      int64(len(content)),
		Mode:      permBits(stat.Entry.Mode),
		ModeOctal: fmt.Sprintf("%04o", permBits(stat.Entry.Mode)),
		Lines:     textedit.CountLines(content),
		CRLF:      textedit.UsesCRLF(content),
		Content:   content,
		Entry:     stat.Entry,
	}, nil
}

// EditHistory lists the undo entries kept for a file, newest first.
func (a *App) EditHistory(serverName, remotePath string) ([]EditHistoryItem, error) {
	entries, err := a.editHistory().list(serverName, remotePath)
	if err != nil {
		return nil, err
	}
	items := make([]EditHistoryItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, e.item())
	}
	return items, nil
}

// UndoRemoteEdit restores the version of remotePath from before its most recent
// edit through Fleet. It only applies while the file is still exactly what that
// edit produced, so it can never discard a later change.
//
// force restores the saved version whatever the file holds now — for an
// in-place edit that was interrupted and may have left the file part-written.
func (a *App) UndoRemoteEdit(serverName, remotePath string, force bool) (EditResult, error) {
	h := a.editHistory()
	entries, err := h.list(serverName, remotePath)
	if err != nil {
		return EditResult{}, err
	}
	if len(entries) == 0 {
		return EditResult{}, fmt.Errorf("no edit of %s:%s is recorded on this controller to undo", serverName, remotePath)
	}
	latest := entries[0]
	original, err := h.content(latest)
	if err != nil {
		return EditResult{}, err
	}
	req := EditRequest{
		Path:     latest.ResolvedPath,
		Replace:  true,
		Content:  original,
		NoBackup: true,
		// A file that had to be written in place is restored the same way.
		InPlace: latest.InPlace,
	}
	if !force {
		req.BaseSHA256 = latest.NewSHA256
	}
	res, err := a.EditRemoteFile(serverName, req)
	if err != nil {
		if EditErrorCode(err) == "edit_conflict" {
			return EditResult{}, fmt.Errorf("%s:%s changed after that edit, so undoing it would discard the newer change; view the file and edit it instead, or restore the saved version anyway with --undo --force (%v)", serverName, remotePath, err)
		}
		return EditResult{}, err
	}
	if err := h.remove(latest); err != nil {
		res.BackupWarning = "the file was restored, but its history entry could not be removed: " + err.Error()
	}
	return res, nil
}

// readAllLimited reads r up to limit bytes, refusing anything longer.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("input is larger than %s", humanBytes(limit))
	}
	return b, nil
}

// ReadEditInput reads new content or an edit list (from a file or stdin) with
// the edit size limit applied.
func (a *App) ReadEditInput(r io.Reader) ([]byte, error) {
	// An edit list carries old and new text, so allow room for both.
	return readAllLimited(r, 2*a.FileEditSettings().EffectiveMaxBytes())
}
