// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// SigningPublicKey overrides the embedded minisign key; empty uses the
	// embedded default. Set only in tests.
	SigningPublicKey string
	FetchManifest    func(context.Context, string) (update.Manifest, error)
	DownloadURL      func(context.Context, string) ([]byte, error)
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
		ManifestURL:      payload.ManifestURL,
		Channel:          payload.Channel,
		ConfigDir:        configDir,
		ExecutablePath:   executablePath,
		CurrentVersion:   version.Version,
		AgentBinary:      true,
		SigningPublicKey: u.SigningPublicKey,
		FetchManifest:    u.FetchManifest,
		DownloadURL:      u.DownloadURL,
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
	// Validate the controller-supplied service name before it reaches
	// systemd-run/systemctl. These run via exec (no shell), so the risk is not
	// shell metacharacters but OPTION INJECTION: a name like "--no-block" or
	// "-H" would be parsed as a flag by systemctl, and a leading '-' anywhere is
	// dangerous. validServiceName already constrains to the systemd unit charset
	// ([A-Za-z0-9_@.:-], <=256); we additionally reject a leading '-' so the name
	// can never be taken for an option. With this charset the value cannot be an
	// option or carry a metacharacter, so it is safe to pass as a bare argument.
	if !validServiceName.MatchString(serviceName) || strings.HasPrefix(serviceName, "-") {
		return false, &RPCError{
			Code:    "invalid_service_name",
			Message: "service name must be a valid systemd unit name and may not start with '-'",
		}
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
