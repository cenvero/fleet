// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/cenvero/fleet/pkg/proto"
)

func runShellExec(ctx context.Context, payload proto.ExecPayload) (proto.ExecResult, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", payload.Command) //nolint:gosec
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return proto.ExecResult{}, err
		}
	}
	return proto.ExecResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
	}, nil
}
