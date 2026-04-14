// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

func TestManagedUpdaterAppliesAgentBinaryAndSchedulesRestart(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "agent")
	executablePath := filepath.Join(t.TempDir(), runtimeExecutableName("fleet-agent"))
	if err := os.WriteFile(executablePath, []byte("old-agent"), 0o755); err != nil {
		t.Fatalf("WriteFile(executable) error = %v", err)
	}

	archive := tarGzBinary(t, filepath.Base(executablePath), []byte("new-agent"))
	sum := sha256.Sum256(archive)
	manifest := update.Manifest{
		Channels: map[string]update.ChannelInfo{
			"stable": {Version: "v1.2.3"},
		},
		AgentBinaries: map[string]map[string]update.BinaryInfo{
			"v1.2.3": {
				runtime.GOOS + "-" + runtime.GOARCH: {
					URL:    "https://example.invalid/fleet-agent.tar.gz",
					SHA256: hex.EncodeToString(sum[:]),
				},
			},
		},
	}

	runner := &recordingRunner{}
	updater := managedUpdater{
		Runner:         runner,
		ExecutablePath: executablePath,
		ConfigDir:      configDir,
		FetchManifest: func(context.Context, string) (update.Manifest, error) {
			return manifest, nil
		},
		DownloadURL: func(context.Context, string) ([]byte, error) {
			return archive, nil
		},
	}

	op, err := updater.Apply(context.Background(), proto.UpdateApplyPayload{
		ManifestURL: "https://example.invalid/manifest.json",
		Channel:     "stable",
		ServiceName: "cenvero-fleet-agent",
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !op.Result.Applied {
		t.Fatalf("expected agent update to be applied")
	}
	if runtime.GOOS == "linux" {
		if !op.Result.RestartScheduled {
			t.Fatalf("expected restart to be scheduled on linux")
		}
		if len(runner.calls) != 1 {
			t.Fatalf("expected one restart scheduling call, got %d", len(runner.calls))
		}
		if got := runner.calls[0]; len(got) < 7 || got[0] != "systemd-run" || got[len(got)-2] != "restart" || got[len(got)-1] != "cenvero-fleet-agent" {
			t.Fatalf("unexpected restart command: %#v", got)
		}
	} else {
		if op.Result.RestartScheduled {
			t.Fatalf("expected restart scheduling to be skipped on %s", runtime.GOOS)
		}
		if len(runner.calls) != 0 {
			t.Fatalf("expected no restart scheduler call on %s, got %#v", runtime.GOOS, runner.calls)
		}
	}

	current, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(executable) error = %v", err)
	}
	if string(current) != "new-agent" {
		t.Fatalf("expected new agent binary contents, got %q", string(current))
	}
}

type recordingRunner struct {
	calls [][]string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	return []byte("ok"), nil
}

func tarGzBinary(t *testing.T, name string, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(payload)),
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close(tar) error = %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("Close(gzip) error = %v", err)
	}
	return buf.Bytes()
}

func runtimeExecutableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}
