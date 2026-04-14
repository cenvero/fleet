// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
)

type UpdateOperation struct {
	Result   proto.UpdateApplyResult
	Finalize func() error
}

type Updater interface {
	Apply(context.Context, proto.UpdateApplyPayload) (UpdateOperation, error)
}

type managedUpdater struct {
	Runner         commandRunner
	ExecutablePath string
	ConfigDir      string
	FetchManifest  func(context.Context, string) (update.Manifest, error)
	DownloadURL    func(context.Context, string) ([]byte, error)
}

func defaultUpdater() Updater {
	return managedUpdater{Runner: execRunner{}}
}

func (u managedUpdater) Apply(ctx context.Context, payload proto.UpdateApplyPayload) (UpdateOperation, error) {
	executablePath := u.ExecutablePath
	if executablePath == "" {
		path, err := os.Executable()
		if err != nil {
			return UpdateOperation{}, err
		}
		executablePath = path
	}

	configDir := u.ConfigDir
	if configDir == "" {
		configDir = agentConfigDir()
	}

	result, err := update.Apply(ctx, update.ApplyOptions{
		ManifestURL:    payload.ManifestURL,
		Channel:        payload.Channel,
		ConfigDir:      configDir,
		ExecutablePath: executablePath,
		CurrentVersion: version.Version,
		AgentBinary:    true,
		FetchManifest:  u.FetchManifest,
		DownloadURL:    u.DownloadURL,
	})
	if err != nil {
		return UpdateOperation{}, err
	}

	op := UpdateOperation{
		Result: proto.UpdateApplyResult{
			Channel:           result.Channel,
			CurrentVersion:    result.CurrentVersion,
			Version:           result.Version,
			BackupPath:        result.BackupPath,
			RollbackState:     result.RollbackState,
			ReleaseNotesURL:   result.ReleaseNotesURL,
			Applied:           result.Applied,
			SHA256Verified:    result.SHA256Verified,
			SignatureVerified: result.SignatureVerified,
			ServiceName:       payload.ServiceName,
		},
	}

	if result.Applied && payload.ServiceName != "" {
		scheduled, err := u.scheduleRestart(payload.ServiceName)
		if err != nil {
			return UpdateOperation{}, err
		}
		op.Result.RestartScheduled = scheduled
	}
	return op, nil
}

func (u managedUpdater) scheduleRestart(serviceName string) (bool, error) {
	if runtime.GOOS != "linux" {
		return false, nil
	}
	if u.Runner == nil {
		u.Runner = execRunner{}
	}
	unitName := fmt.Sprintf("cenvero-fleet-agent-update-%d", time.Now().UTC().UnixNano())
	output, err := u.Runner.Run(
		context.Background(),
		"systemd-run",
		"--quiet",
		"--unit", unitName,
		"--on-active=1s",
		"systemctl", "restart", serviceName,
	)
	if err != nil {
		return false, &RPCError{
			Code:    "agent_restart_failed",
			Message: nonEmptyCommandMessage(output, err),
		}
	}
	return true, nil
}

func agentConfigDir() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ".cenvero-fleet-agent"
	}
	return filepath.Join(home, ".cenvero-fleet-agent")
}
