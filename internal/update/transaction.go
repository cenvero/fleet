// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	transactionApply    = "apply"
	transactionRollback = "rollback"
)

type updateFileOps struct {
	replace            func(string, string) error
	writeRollbackState func(string, rollbackState) error
	removeDurable      func(string) error
}

func defaultUpdateFileOps() *updateFileOps {
	return &updateFileOps{
		replace:            replaceFile,
		writeRollbackState: writeRollbackState,
		removeDurable:      removeDurable,
	}
}

type updateJournal struct {
	Operation             string          `json:"operation"`
	ExecutablePath        string          `json:"executable_path"`
	RecoveryPath          string          `json:"recovery_path"`
	StagedPath            string          `json:"staged_path"`
	RollbackStatePath     string          `json:"rollback_state_path"`
	HadPreviousState      bool            `json:"had_previous_state"`
	PreviousRollbackState json.RawMessage `json:"previous_rollback_state,omitempty"`
	AppliedVersion        string          `json:"applied_version,omitempty"`
}

func updateJournalPath(configDir string) string {
	return filepath.Join(configDir, "data", "update-transaction.json")
}

func installUpdate(opts ApplyOptions, payload []byte, mode os.FileMode, state rollbackState) (string, string, error) {
	ops := opts.fileOps
	if ops == nil {
		ops = defaultUpdateFileOps()
	}

	updateDir := filepath.Join(opts.ConfigDir, "backups", "updates")
	if err := ensureDirDurable(updateDir, 0o750); err != nil {
		return "", "", err
	}
	backupPath := filepath.Join(updateDir, filepath.Base(opts.ExecutablePath)+"."+opts.Now().UTC().Format("20060102T150405.000000000Z")+".bak")
	if err := copyFile(opts.ExecutablePath, backupPath, 0o755); err != nil {
		return "", "", fmt.Errorf("backup current executable: %w", err)
	}

	stagedPath := opts.ExecutablePath + ".new"
	defer os.Remove(stagedPath) //nolint:errcheck
	if err := writeFileDurable(stagedPath, payload, normalizeMode(mode)); err != nil {
		if os.IsPermission(err) {
			userBin := userBinaryInstallPath()
			return "", "", fmt.Errorf(
				"cannot write to %s: permission denied; the fleet binary is at a system path that requires root access to update; either run 'sudo fleet --config-dir %s update apply' or reinstall to a user path with 'install -m 0755 %s %s' and add %s to PATH",
				opts.ExecutablePath, opts.ConfigDir, opts.ExecutablePath, userBin, filepath.Dir(userBin),
			)
		}
		return "", "", fmt.Errorf("write staged executable: %w", err)
	}

	state.BackupPath = backupPath
	statePath := rollbackStatePath(opts.ConfigDir)
	previous, hadPrevious, err := readOptionalFile(statePath)
	if err != nil {
		return "", "", fmt.Errorf("read previous rollback state: %w", err)
	}
	journal := updateJournal{
		Operation:             transactionApply,
		ExecutablePath:        opts.ExecutablePath,
		RecoveryPath:          backupPath,
		StagedPath:            stagedPath,
		RollbackStatePath:     statePath,
		HadPreviousState:      hadPrevious,
		PreviousRollbackState: previous,
		AppliedVersion:        state.AppliedVersion,
	}
	journalPath := updateJournalPath(opts.ConfigDir)
	if err := writeJSONAtomic(journalPath, journal, 0o600); err != nil {
		return "", "", fmt.Errorf("write update transaction journal: %w", err)
	}

	if err := ops.replace(stagedPath, opts.ExecutablePath); err != nil {
		return "", "", abortUpdateTransaction(journalPath, journal, ops, fmt.Errorf("replace executable: %w", err))
	}
	if err := ops.writeRollbackState(statePath, state); err != nil {
		return "", "", abortUpdateTransaction(journalPath, journal, ops, fmt.Errorf("write rollback state: %w", err))
	}
	if err := ops.removeDurable(journalPath); err != nil {
		// The binary and rollback state are already a durable, matching pair. A
		// retained journal is safe: startup recovery recognizes the committed
		// state and only removes the stale journal.
		return backupPath, statePath, nil
	}
	return backupPath, statePath, nil
}

func rollbackUpdate(configDir, executablePath string, ops *updateFileOps) (RollbackResult, error) {
	if configDir == "" {
		return RollbackResult{}, fmt.Errorf("config dir is required")
	}
	if executablePath == "" {
		path, err := os.Executable()
		if err != nil {
			return RollbackResult{}, err
		}
		executablePath = path
	}
	if ops == nil {
		ops = defaultUpdateFileOps()
	}
	if err := recoverInterruptedUpdate(configDir, ops); err != nil {
		return RollbackResult{}, err
	}

	statePath := rollbackStatePath(configDir)
	stateBytes, err := os.ReadFile(statePath) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if err != nil {
		return RollbackResult{}, fmt.Errorf("read rollback state: %w", err)
	}
	var state rollbackState
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		return RollbackResult{}, fmt.Errorf("decode rollback state: %w", err)
	}
	if state.ExecutablePath != "" {
		executablePath = state.ExecutablePath
	}
	if state.BackupPath == "" {
		return RollbackResult{}, fmt.Errorf("rollback backup path is missing")
	}

	transactionDir := filepath.Join(configDir, "backups", "updates")
	if err := ensureDirDurable(transactionDir, 0o750); err != nil {
		return RollbackResult{}, err
	}
	recoveryPath := filepath.Join(transactionDir, filepath.Base(executablePath)+".rollback-recovery")
	if err := copyFile(executablePath, recoveryPath, 0o755); err != nil {
		return RollbackResult{}, fmt.Errorf("backup executable before rollback: %w", err)
	}
	stagedPath := executablePath + ".rollback"
	defer os.Remove(stagedPath) //nolint:errcheck
	if err := copyFile(state.BackupPath, stagedPath, 0o755); err != nil {
		return RollbackResult{}, fmt.Errorf("stage rollback executable: %w", err)
	}

	journal := updateJournal{
		Operation:             transactionRollback,
		ExecutablePath:        executablePath,
		RecoveryPath:          recoveryPath,
		StagedPath:            stagedPath,
		RollbackStatePath:     statePath,
		HadPreviousState:      true,
		PreviousRollbackState: stateBytes,
	}
	journalPath := updateJournalPath(configDir)
	if err := writeJSONAtomic(journalPath, journal, 0o600); err != nil {
		return RollbackResult{}, fmt.Errorf("write rollback transaction journal: %w", err)
	}
	if err := ops.replace(stagedPath, executablePath); err != nil {
		return RollbackResult{}, abortUpdateTransaction(journalPath, journal, ops, fmt.Errorf("replace executable during rollback: %w", err))
	}
	if err := ops.removeDurable(statePath); err != nil {
		return RollbackResult{}, abortUpdateTransaction(journalPath, journal, ops, fmt.Errorf("remove rollback state: %w", err))
	}
	if err := ops.removeDurable(journalPath); err == nil {
		_ = removeDurable(recoveryPath)
	}

	return RollbackResult{
		ExecutablePath: executablePath,
		RestoredFrom:   state.BackupPath,
		Version:        state.PreviousVersion,
		Restored:       true,
	}, nil
}

func recoverInterruptedUpdate(configDir string, ops *updateFileOps) error {
	journalPath := updateJournalPath(configDir)
	data, err := os.ReadFile(journalPath) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read update transaction journal: %w", err)
	}
	var journal updateJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return fmt.Errorf("decode update transaction journal: %w", err)
	}
	if journal.ExecutablePath == "" || journal.RecoveryPath == "" || journal.RollbackStatePath == "" {
		return fmt.Errorf("update transaction journal is incomplete; refusing unsafe recovery")
	}

	committed := false
	switch journal.Operation {
	case transactionApply:
		state, stateErr := readRollbackState(journal.RollbackStatePath)
		if stateErr == nil && state.AppliedVersion == journal.AppliedVersion && state.ExecutablePath == journal.ExecutablePath && state.BackupPath == journal.RecoveryPath {
			if _, statErr := os.Stat(journal.ExecutablePath); statErr == nil {
				committed = true
			}
		}
	case transactionRollback:
		if _, stateErr := os.Stat(journal.RollbackStatePath); os.IsNotExist(stateErr) {
			if _, statErr := os.Stat(journal.ExecutablePath); statErr == nil {
				committed = true
			}
		}
	default:
		return fmt.Errorf("unknown update transaction operation %q", journal.Operation)
	}
	if committed {
		if err := ops.removeDurable(journalPath); err != nil {
			return fmt.Errorf("remove committed update journal: %w", err)
		}
		if journal.Operation == transactionRollback {
			_ = removeDurable(journal.RecoveryPath)
		}
		return nil
	}
	return abortUpdateTransaction(journalPath, journal, ops, fmt.Errorf("recovered interrupted %s transaction", journal.Operation))
}

func abortUpdateTransaction(journalPath string, journal updateJournal, ops *updateFileOps, cause error) error {
	restoreStage := journal.ExecutablePath + ".restore"
	_ = os.Remove(restoreStage)
	if err := copyFile(journal.RecoveryPath, restoreStage, 0o755); err != nil {
		return errors.Join(cause, fmt.Errorf("stage original executable for recovery: %w", err))
	}
	defer os.Remove(restoreStage) //nolint:errcheck
	if err := ops.replace(restoreStage, journal.ExecutablePath); err != nil {
		return errors.Join(cause, fmt.Errorf("restore original executable: %w", err))
	}
	if err := restorePreviousRollbackState(journal, ops); err != nil {
		return errors.Join(cause, fmt.Errorf("restore previous rollback state: %w", err))
	}
	if err := ops.removeDurable(journalPath); err != nil {
		return errors.Join(cause, fmt.Errorf("remove aborted update journal: %w", err))
	}
	return cause
}

func restorePreviousRollbackState(journal updateJournal, ops *updateFileOps) error {
	if journal.HadPreviousState {
		if len(journal.PreviousRollbackState) == 0 {
			return fmt.Errorf("journal says previous rollback state existed but carries no data")
		}
		return writeFileAtomic(journal.RollbackStatePath, journal.PreviousRollbackState, 0o600)
	}
	return ops.removeDurable(journal.RollbackStatePath)
}

func writeRollbackState(path string, state rollbackState) error {
	return writeJSONAtomic(path, state, 0o600)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), mode)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := ensureDirDurable(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameReplacePlatform(tmpPath, path); err != nil {
		return err
	}
	return syncParentDir(path)
}

func writeFileDurable(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncParentDir(path)
}

func copyFile(source, target string, mode os.FileMode) error {
	src, err := os.Open(source) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if err != nil {
		return err
	}
	defer src.Close()
	if mode == 0 {
		if info, statErr := src.Stat(); statErr == nil {
			mode = info.Mode().Perm()
		}
	}
	if mode == 0 {
		mode = 0o755
	}
	if err := ensureDirDurable(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return syncParentDir(target)
}

func ensureDirDurable(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return syncParentDir(filepath.Join(path, ".dir-entry"))
}

func removeDurable(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncParentDir(path)
}

func readOptionalFile(path string) (json.RawMessage, bool, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is derived from the configured executable/controller update transaction directory
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(data), true, nil
}
