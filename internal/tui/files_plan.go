// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/cenvero/fleet/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"
)

// ============================================================================
// Transfer plans
//
// A copy/move is first turned into a plan: what goes where, how many files and
// bytes (folders are measured asynchronously), and which names already exist
// at the destination. The confirm dialog shows exactly that and lets the
// operator choose how to treat collisions — overwrite (folders merge), skip
// the existing ones, or keep both under a "name copy" name — before anything
// is queued.
// ============================================================================

type conflictPolicy int

const (
	policyOverwrite conflictPolicy = iota
	policySkip
	policyKeepBoth
)

func (p conflictPolicy) label() string {
	switch p {
	case policySkip:
		return "Skip existing"
	case policyKeepBoth:
		return "Keep both"
	}
	return "Overwrite"
}

type transferPlan struct {
	kind             dirTransferKind
	fromSide, toSide int
	destDir          string
	items            []fileItem

	files     int
	fileBytes int64
	dirs      int

	scanned   bool
	scanFiles int
	scanBytes int64
	scanErr   error

	checked   bool            // destination listing was available for a collision check
	conflicts []string        // item names that already exist at the destination
	existing  map[string]bool // destination names (lower-cased when case-insensitive)
	fold      bool            // destination is case-insensitive
	samePlace bool            // source folder == destination folder
	policy    conflictPolicy
}

func (p *transferPlan) conflict(name string) bool {
	if !p.checked {
		return false
	}
	key := name
	if p.fold {
		key = strings.ToLower(key)
	}
	return p.existing[key]
}

// startBatch validates and starts a transfer the way a drag-drop does: plain
// files with no name collisions are queued immediately; folders or collisions
// ask first. targetIdx selects a destination folder row in the dest pane, or
// -1 for the dest pane's cwd.
func (m filesModel) startBatch(fromSide, toSide int, items []fileItem, kind dirTransferKind, targetIdx int) (tea.Model, tea.Cmd) {
	return m.planTransfer(fromSide, toSide, items, kind, targetIdx, true)
}

// planTransfer runs the pre-flight checks and either queues the transfer
// (quick path) or opens the confirmation dialog.
func (m filesModel) planTransfer(fromSide, toSide int, items []fileItem, kind dirTransferKind, targetIdx int, quick bool) (tea.Model, tea.Cmd) {
	if fromSide == toSide || len(items) == 0 {
		return m, nil
	}
	srcPane := m.paneRefConst(fromSide)
	dstPane := m.paneRefConst(toSide)
	destDir := m.destDir(toSide, targetIdx)
	if err := core.ValidateTargetPath(dstPane.pathStyle, destDir); err != nil {
		m.status = "invalid destination: " + err.Error()
		return m, nil
	}
	seen := make(map[string]string, len(items))
	caseInsensitive := dstPane.pathStyle.IsWindows()
	if !dstPane.remote {
		var err error
		caseInsensitive, err = core.LocalPathCaseInsensitive(destDir)
		if err != nil {
			m.status = "cannot inspect destination: " + err.Error()
			return m, nil
		}
	}
	for _, item := range items {
		if err := core.ValidateTargetPathComponent(dstPane.pathStyle, item.name); err != nil {
			m.status = "destination cannot represent " + item.name + ": " + err.Error()
			return m, nil
		}
		key := item.name
		if caseInsensitive {
			key = strings.ToLower(key)
		}
		if previous, ok := seen[key]; ok && previous != item.name {
			m.status = fmt.Sprintf("destination name collision: %s and %s", previous, item.name)
			return m, nil
		}
		seen[key] = item.name
	}

	plan := &transferPlan{kind: kind, fromSide: fromSide, toSide: toSide, destDir: destDir,
		items: append([]fileItem(nil), items...), fold: caseInsensitive}
	for _, it := range items {
		if it.isDir {
			plan.dirs++
		} else {
			plan.files++
			plan.fileBytes += it.size
		}
	}
	plan.samePlace = srcPane.source == dstPane.source &&
		srcPane.pathStyle.Clean(srcPane.cwd) == dstPane.pathStyle.Clean(destDir)
	// Collision check against the destination listing we already have.
	if targetIdx < 0 && dstPane.err == nil && dstPane.listedCwd == dstPane.cwd && dstPane.listedCwd != "" {
		plan.checked = true
		plan.existing = make(map[string]bool, len(dstPane.allItems))
		for _, e := range dstPane.allItems {
			key := e.name
			if caseInsensitive {
				key = strings.ToLower(key)
			}
			plan.existing[key] = true
		}
		for _, it := range items {
			if plan.conflict(it.name) {
				plan.conflicts = append(plan.conflicts, it.name)
			}
		}
	}
	if plan.samePlace {
		// Copying a folder onto itself can only mean "make a copy".
		plan.policy = policyKeepBoth
	}

	if quick && plan.dirs == 0 && len(plan.conflicts) == 0 && !plan.samePlace {
		return m.executePlan(plan)
	}
	m.overlay = overlayConfirm
	m.confirm = confirmTransfer
	m.plan = plan
	m.confirmText = ""
	if plan.dirs == 0 {
		plan.scanned = true
		return m, nil
	}
	return m, m.planScanCmd(plan)
}

// planScanCmd measures the plan's folders (off the UI thread).
func (m filesModel) planScanCmd(plan *transferPlan) tea.Cmd {
	src := m.paneRefConst(plan.fromSide)
	app := m.app
	source, cwd, style := src.source, src.cwd, src.pathStyle
	var dirs []string
	for _, it := range plan.items {
		if it.isDir {
			dirs = append(dirs, joinPath(cwd, it.name, style))
		}
	}
	return func() tea.Msg {
		total := dirScanMsg{plan: plan}
		for _, d := range dirs {
			var f int
			var b int64
			var err error
			if source == "" {
				f, b, err = core.EstimateLocalTree(d)
			} else if app != nil {
				f, b, err = app.EstimateRemoteTree(source, d)
			}
			total.files += f
			total.bytes += b
			if err != nil && total.err == nil {
				total.err = err
			}
		}
		return total
	}
}

// destDir computes the absolute destination directory in the dest pane.
func (m filesModel) destDir(toSide, targetIdx int) string {
	pane := m.paneRefConst(toSide)
	if targetIdx >= 0 && targetIdx < len(pane.entries) {
		dst := pane.entries[targetIdx]
		if dst.isDir && dst.name != ".." {
			return joinPath(pane.cwd, dst.name, pane.pathStyle)
		}
	}
	return pane.cwd
}

// executePlan queues one transfer per item according to the chosen collision
// policy.
func (m filesModel) executePlan(plan *transferPlan) (tea.Model, tea.Cmd) {
	mm, _ := m.executePlanQueued(plan)
	m = mm.(filesModel)
	return m, m.pumpTransfers()
}

// executePlanQueued adds the plan's rows to the queue without starting them.
func (m filesModel) executePlanQueued(plan *transferPlan) (tea.Model, tea.Cmd) {
	m.overlay = overlayNone
	m.plan = nil
	src := m.paneRefConst(plan.fromSide)
	dst := m.paneRefConst(plan.toSide)
	route := dst.label() + ":" + plan.destDir
	existing := map[string]bool{}
	for k := range plan.existing {
		existing[k] = true
	}
	queued, skipped := 0, 0
	if !m.transfersActive() {
		// A new burst of work: completion summaries count from here.
		m.batchFrom = m.nextID
	}
	for _, it := range plan.items {
		dstName := it.name
		if plan.conflict(it.name) || plan.samePlace {
			switch plan.policy {
			case policySkip:
				skipped++
				continue
			case policyKeepBoth:
				style := dst.pathStyle
				if plan.fold {
					// duplicateName compares case-insensitively for Windows
					// targets; use that for any case-folding destination.
					style = core.TargetPathWindows
				}
				dstName = duplicateName(it.name, existing, style)
				key := dstName
				if plan.fold {
					key = strings.ToLower(key)
				}
				existing[key] = true
			}
		}
		srcPath := joinPath(src.cwd, it.name, src.pathStyle)
		var dstPath string
		if !dst.remote {
			// Vet remote-derived names before composing a LOCAL destination path.
			safe, err := core.SafeLocalJoin(plan.destDir, dstName)
			if err != nil {
				m.setStatus(levelError, "refused unsafe name: "+it.name)
				continue
			}
			dstPath = safe
		} else {
			dstPath = joinPath(plan.destDir, dstName, dst.pathStyle)
		}
		job := &transferJob{
			srcSource: src.source, srcPath: srcPath,
			dstSource: dst.source, dstPath: dstPath,
			isDir: it.isDir, kind: plan.kind,
		}
		name := it.name
		if dstName != it.name {
			name = it.name + " → " + dstName
		}
		label := fmt.Sprintf("%s %s", transferGlyph(src.remote, dst.remote, plan.kind), it.name)
		if it.isDir {
			label += "/"
		}
		total := it.size
		if it.isDir {
			total = 0
			if plan.dirs == 1 && plan.scanned && plan.scanErr == nil {
				total = plan.scanBytes
			}
		}
		m.enqueueTransfer(job, label, name, route, total)
		queued++
	}
	switch {
	case queued == 0 && skipped > 0:
		m.setStatus(levelWarn, fmt.Sprintf("nothing to do — skipped %d existing item(s)", skipped))
		return m, nil
	case queued == 0:
		return m, nil
	}
	msg := fmt.Sprintf("started %d transfer(s) → %s", queued, route)
	if skipped > 0 {
		msg += fmt.Sprintf(" · skipped %d existing", skipped)
	}
	m.setStatus(levelOK, msg)
	m.clearSelection(plan.fromSide)
	return m, nil
}

// ---- dialog ----

func (m filesModel) handlePlanKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	plan := m.plan
	if plan == nil {
		m.overlay = overlayNone
		return m, nil
	}
	switch msg.String() {
	case "esc", "q", "n":
		m.overlay = overlayNone
		m.plan = nil
		m.status = "cancelled"
		return m, nil
	case "enter", "y":
		return m.executePlan(plan)
	case "o":
		if !plan.samePlace {
			plan.policy = policyOverwrite
		}
	case "s":
		plan.policy = policySkip
	case "k":
		plan.policy = policyKeepBoth
	case "tab", "right", "l":
		plan.policy = m.nextPolicy(plan, 1)
	case "shift+tab", "left", "h":
		plan.policy = m.nextPolicy(plan, -1)
	}
	return m, nil
}

func (m filesModel) nextPolicy(plan *transferPlan, delta int) conflictPolicy {
	if len(plan.conflicts) == 0 && !plan.samePlace {
		return plan.policy
	}
	p := (int(plan.policy) + delta + 3) % 3
	if plan.samePlace && conflictPolicy(p) == policyOverwrite {
		p = (p + delta + 3) % 3
	}
	return conflictPolicy(p)
}

func (m filesModel) handlePlanMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	plan := m.plan
	if plan == nil {
		m.overlay = overlayNone
		return m, nil
	}
	for _, p := range []conflictPolicy{policyOverwrite, policySkip, policyKeepBoth} {
		if zone.Get(fmt.Sprintf("%spolicy-%d", fmPlanPrefix, p)).InBounds(msg) {
			if !(plan.samePlace && p == policyOverwrite) {
				plan.policy = p
			}
			return m, nil
		}
	}
	if zone.Get(fmPlanPrefix + "ok").InBounds(msg) {
		return m.executePlan(plan)
	}
	if zone.Get(fmPlanPrefix + "cancel").InBounds(msg) {
		m.overlay = overlayNone
		m.plan = nil
		m.status = "cancelled"
	}
	return m, nil
}

const fmPlanPrefix = "fm-plan-"

func (m filesModel) renderTransferConfirm() string {
	plan := m.plan
	src := m.paneRefConst(plan.fromSide)
	dst := m.paneRefConst(plan.toSide)
	dw := m.dialogWidth(66)
	verb := "Copy"
	titleSty := fmTitleSty
	border := fmAccent
	if plan.kind == dtMove {
		verb = "Move"
		titleSty = fmWarnSty
		border = fmWarnC
	}
	var b strings.Builder
	what := plural(len(plan.items), "item", "items")
	if len(plan.items) == 1 {
		what = "“" + fmFitName(fmSanitize(plan.items[0].name), dw-24) + "”"
	}
	b.WriteString(titleSty.Render(fmt.Sprintf("⇄ %s %s to %s", verb, what, fmSanitize(dst.label()))))
	b.WriteString("\n\n")
	row := func(label string, remote bool, name, path string) {
		head := fmt.Sprintf("%-5s %s %s  ", label, sourceGlyph(remote), fmSanitize(name))
		b.WriteString(fmDimSty.Render(head))
		b.WriteString(fmTextSty.Render(fmFitPathLeft(fmSanitize(path), dw-fmWidth(head))))
		b.WriteString("\n")
	}
	row("From", src.remote, src.label(), src.cwd)
	row("To", dst.remote, dst.label(), plan.destDir)
	b.WriteString("\n")

	// Show up to 6 items with the ones that collide marked.
	max := 6
	if m.height < 30 {
		max = 3
	}
	for i, it := range plan.items {
		if i >= max {
			b.WriteString(fmDimSty.Render(fmt.Sprintf("  … and %d more", len(plan.items)-max)))
			b.WriteString("\n")
			break
		}
		name := fmSanitize(it.name)
		meta := humanSize(it.size)
		if it.isDir {
			name += "/"
			meta = "folder"
		}
		mark := "•"
		sty := fmTextSty
		if plan.conflict(it.name) || plan.samePlace {
			mark = "!"
			sty = fmWarnSty
		}
		nameW := dw - 16
		b.WriteString(sty.Render("  " + mark + " " + fmPadRight(fmFitName(name, nameW), nameW)))
		b.WriteString(fmDimSty.Render(fmPadLeft(meta, 10)))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	// Totals.
	var parts []string
	if plan.files > 0 {
		parts = append(parts, fmt.Sprintf("%s (%s)", plural(plan.files, "file", "files"), humanSize(plan.fileBytes)))
	}
	if plan.dirs > 0 {
		d := plural(plan.dirs, "folder", "folders")
		switch {
		case plan.scanErr != nil:
			d += " (size unknown: " + fmFit(fmSanitize(plan.scanErr.Error()), 30) + ")"
		case plan.scanned:
			d += fmt.Sprintf(" with %s (%s)", plural(plan.scanFiles, "file", "files"), humanSize(plan.scanBytes))
		default:
			d += " (measuring…)"
		}
		parts = append(parts, d)
	}
	total := "Total: " + strings.Join(parts, " + ")
	for _, ln := range fmWrap(total, dw) {
		b.WriteString(fmTextSty.Render(ln))
		b.WriteString("\n")
	}
	if plan.kind == dtMove {
		b.WriteString(fmDimSty.Render("Originals are removed after each item is copied."))
		b.WriteString("\n")
	}
	if !dst.remote && src.remote && m.app != nil {
		if free, ok := localDiskFree(plan.destDir); ok {
			need := plan.fileBytes + plan.scanBytes
			if need > free {
				b.WriteString(fmErrSty.Render(fmt.Sprintf("Not enough space: needs %s, %s free", humanSize(need), humanSize(free))))
				b.WriteString("\n")
			}
		}
	}

	// Collisions + policy.
	if len(plan.conflicts) > 0 || plan.samePlace {
		b.WriteString("\n")
		var head string
		if plan.samePlace {
			head = "⚠ Source and destination are the same folder — copies get new names."
		} else {
			names := make([]string, 0, 3)
			for i, n := range plan.conflicts {
				if i == 3 {
					names = append(names, fmt.Sprintf("+%d", len(plan.conflicts)-3))
					break
				}
				names = append(names, fmSanitize(n))
			}
			verb := "already exist"
			if len(plan.conflicts) == 1 {
				verb = "already exists"
			}
			head = fmt.Sprintf("⚠ %d %s at the destination: %s",
				len(plan.conflicts), verb, strings.Join(names, ", "))
		}
		for _, ln := range fmWrap(head, dw) {
			b.WriteString(fmWarnSty.Render(ln))
			b.WriteString("\n")
		}
		chip := func(p conflictPolicy, key string) string {
			txt := " " + key + " " + p.label() + " "
			if plan.samePlace && p == policyOverwrite {
				return fmDimSty.Render(txt)
			}
			if plan.policy == p {
				return zone.Mark(fmt.Sprintf("%spolicy-%d", fmPlanPrefix, p), fmSelRow.Render("●"+txt))
			}
			return zone.Mark(fmt.Sprintf("%spolicy-%d", fmPlanPrefix, p), fmTextSty.Render("○"+txt))
		}
		b.WriteString(chip(policyOverwrite, "o") + " " + chip(policySkip, "s") + " " + chip(policyKeepBoth, "k"))
		b.WriteString("\n")
		switch plan.policy {
		case policyOverwrite:
			b.WriteString(fmDimSty.Render("Existing files are replaced; folders are merged."))
		case policySkip:
			b.WriteString(fmDimSty.Render("Items that already exist are left untouched."))
		case policyKeepBoth:
			b.WriteString(fmDimSty.Render("Colliding items get a “name copy” name."))
		}
		b.WriteString("\n")
	} else if !plan.checked {
		b.WriteString(fmDimSty.Render("Existing items at the destination will be replaced."))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	ok := zone.Mark(fmPlanPrefix+"ok", lipgloss.NewStyle().Background(border).Foreground(fmInk).Bold(true).
		Padding(0, 1).Render("↵ "+verb))
	cancel := zone.Mark(fmPlanPrefix+"cancel", lipgloss.NewStyle().Foreground(fmDimC).Padding(0, 1).Render("esc Cancel"))
	b.WriteString(ok + "  " + cancel)
	return fmOverlayBox.BorderForeground(border).Render(b.String())
}

// ============================================================================
// File operations by route
// ============================================================================

// runFileOp dispatches a single-file operation by source/dest type and kind.
//
//	local  -> remote : Upload  (+ delete source for move)
//	remote -> local  : Download(+ delete source for move)
//	remote -> remote : Copy / Move(rename or relay)
//	local  -> local  : os copy (via relay-less rename/copy) — handled below
func runFileOp(app *core.App, srcServer, srcPath, dstServer, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	opts := core.FileTransferOptions{}
	switch {
	case srcServer == "" && dstServer != "":
		// local -> remote
		if _, err := app.UploadFile(dstServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return os.Remove(srcPath)
		}
		return nil
	case srcServer != "" && dstServer == "":
		// remote -> local
		if _, err := app.DownloadFile(srcServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return app.RemoteDelete(srcServer, srcPath, false)
		}
		return nil
	case srcServer != "" && dstServer != "":
		// remote -> remote
		if kind == dtMove {
			return app.MoveFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
		}
		_, err := app.CopyFile(srcServer, srcPath, dstServer, dstPath, opts, progress)
		return err
	default:
		// local -> local
		return localFileCopyMove(srcPath, dstPath, kind, progress)
	}
}

// localFileCopyMove copies (or moves) a single local file. Move tries rename
// first (cheap, cross-pane on the same FS) then falls back to copy+remove.
func localFileCopyMove(srcPath, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	if kind == dtMove {
		if err := os.Rename(srcPath, dstPath); err == nil {
			if progress != nil {
				if fi, e := os.Stat(dstPath); e == nil {
					progress(core.ProgressUpdate{BytesDone: fi.Size(), TotalBytes: fi.Size(), Done: true})
				}
			}
			return nil
		}
	}
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if err := core.CopyLocalFileAtomic(srcPath, dstPath, info.Mode()); err != nil {
		return err
	}
	if progress != nil {
		progress(core.ProgressUpdate{BytesDone: info.Size(), TotalBytes: info.Size(), Done: true})
	}
	if kind == dtMove {
		return os.Remove(srcPath)
	}
	return nil
}

// runDirOp dispatches a recursive directory operation by source/dest type.
func runDirOp(app *core.App, srcServer, srcPath, dstServer, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	opts := core.FileTransferOptions{}
	switch {
	case srcServer == "" && dstServer != "":
		if _, err := app.UploadDir(dstServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return os.RemoveAll(srcPath)
		}
		return nil
	case srcServer != "" && dstServer == "":
		if _, err := app.DownloadDir(srcServer, srcPath, dstPath, opts, progress); err != nil {
			return err
		}
		if kind == dtMove {
			return app.RemoteDelete(srcServer, srcPath, true)
		}
		return nil
	case srcServer != "" && dstServer != "":
		if kind == dtMove {
			_, err := app.MoveDir(srcServer, srcPath, dstServer, dstPath, opts, progress)
			return err
		}
		_, err := app.CopyDir(srcServer, srcPath, dstServer, dstPath, opts, progress)
		return err
	default:
		return localDirCopyMove(srcPath, dstPath, kind, progress)
	}
}

// localDirCopyMove recursively copies (or moves) a local directory tree.
func localDirCopyMove(srcPath, dstPath string, kind dirTransferKind, progress core.ProgressFunc) error {
	if kind == dtMove {
		if err := os.Rename(srcPath, dstPath); err == nil {
			if progress != nil {
				progress(core.ProgressUpdate{Done: true})
			}
			return nil
		}
	}
	err := core.CopyLocalTreeAtomic(srcPath, dstPath)
	if err != nil {
		return err
	}
	if progress != nil {
		progress(core.ProgressUpdate{Done: true})
	}
	if kind == dtMove {
		return os.RemoveAll(srcPath)
	}
	return nil
}
