// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"golang.org/x/crypto/ssh"
)

// requireStagedPrivately checks that a request stages every file directly
// inside its private staging directory. The names of the files a bootstrap
// uploads are visible to every local user on the target (they appear in the
// process list), so they must never live directly in /tmp, where such a user
// could plant a file for root to write through, keep, and later execute.
func requireStagedPrivately(t *testing.T, req BootstrapRequest) {
	t.Helper()
	if !strings.HasPrefix(req.StagingDir, "/tmp/cenvero-") || strings.ContainsAny(strings.TrimPrefix(req.StagingDir, "/tmp/"), "/.") {
		t.Fatalf("staging dir = %q, want a fresh /tmp/cenvero-<random> directory", req.StagingDir)
	}
	paths := []string{}
	for _, u := range req.Uploads {
		paths = append(paths, u.Path)
	}
	if req.AgentRelease != nil {
		paths = append(paths, req.AgentRelease.DestinationPath)
	}
	for _, p := range paths {
		if TargetPathPOSIX.Dir(p) != req.StagingDir {
			t.Fatalf("staged file %q is not inside the private staging dir %q", p, req.StagingDir)
		}
	}
	if !strings.Contains(req.RunCommand, "'"+req.StagingDir+"/") {
		t.Fatalf("run command %q does not run a script from the staging dir", req.RunCommand)
	}
}

func TestBootstrapAndInstallStageInsidePrivateDirectory(t *testing.T) {
	for _, mode := range []transport.Mode{transport.ModeDirect, transport.ModeReverse} {
		configDir := filepath.Join(t.TempDir(), "fleet")
		if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: mode, CryptoAlgorithm: "ed25519",
			UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly, ListenAddress: "203.0.113.5:9443"}); err != nil {
			t.Fatal(err)
		}
		app, err := Open(configDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.AddServer(ServerRecord{Name: "web-01", Address: "192.0.2.10", Mode: mode}); err != nil {
			t.Fatal(err)
		}
		agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
		if err := os.WriteFile(agentBinary, []byte("binary"), 0o700); err != nil {
			t.Fatal(err)
		}
		executor := &fakeBootstrapExecutor{}
		app.BootstrapExecutor = executor
		if _, err := app.BootstrapServer("web-01", BootstrapOptions{LoginUser: "ubuntu", LoginPort: 22, AgentBinaryPath: agentBinary, UseSudo: true}); err != nil {
			t.Fatalf("%s bootstrap: %v", mode, err)
		}
		requireStagedPrivately(t, executor.requests[0])
		_ = app.Close()
	}

	// A dev build uploads the fleet-agent found on PATH.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "fleet-agent"), []byte("agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	for _, v := range []string{"dev", "v1.2.3"} {
		oldVersion := version.Version
		version.Version = v
		configDir := filepath.Join(t.TempDir(), "fleet")
		if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
			CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
			t.Fatal(err)
		}
		app, err := Open(configDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := app.AddServer(ServerRecord{Name: "db-01", Address: "192.0.2.11", Port: 5911, Mode: transport.ModeDirect}); err != nil {
			t.Fatal(err)
		}
		executor := &failingAgentInstallExecutor{err: errors.New("stop after request capture")}
		app.BootstrapExecutor = executor
		_ = app.AutoInstallAgentContext(context.Background(), "db-01", "root", "", "password", 22, false)
		version.Version = oldVersion
		if len(executor.requests) != 1 {
			t.Fatalf("%s install did not reach the executor (%d requests)", v, len(executor.requests))
		}
		requireStagedPrivately(t, executor.requests[0])
		_ = app.Close()
	}
}

// TestStagingDirCommandRefusesAPlantedDirectory: the staging directory is
// created atomically with owner-only permissions and never reused — if anyone
// created that name first (a directory, a file or a symlink), bootstrap stops.
func TestStagingDirCommandRefusesAPlantedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell staging")
	}
	base := t.TempDir()
	fresh := filepath.Join(base, "cenvero-fresh")
	if out, err := exec.Command("/bin/sh", "-c", buildStagingDirCommand(fresh)).CombinedOutput(); err != nil {
		t.Fatalf("create fresh staging dir: %v (%s)", err, out)
	}
	if info, err := os.Lstat(fresh); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("staging dir = %v (err %v), want a 0700 directory", info, err)
	}

	plantedDir := filepath.Join(base, "cenvero-dir")
	if err := os.Mkdir(plantedDir, 0o777); err != nil { // #nosec G301 -- simulates another user's planted world-writable directory
		t.Fatal(err)
	}
	plantedLink := filepath.Join(base, "cenvero-link")
	if err := os.Symlink(base, plantedLink); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{plantedDir, plantedLink} {
		if out, err := exec.Command("/bin/sh", "-c", buildStagingDirCommand(p)).CombinedOutput(); err == nil {
			t.Fatalf("staging into an existing %s succeeded (%s)", p, out)
		}
	}
}

// TestSSHBootstrapExecutorCreatesAndRemovesPrivateStagingDir: the executor
// creates the staging directory before any upload, refuses uploads outside
// it, and removes it (with anything left inside) afterwards.
func TestSSHBootstrapExecutorCreatesAndRemovesPrivateStagingDir(t *testing.T) {
	const dir = "/tmp/cenvero-0123456789abcdef"
	commands := make(chan string, 8)
	clientConn, serverDone := startBootstrapTestSSHServer(t, func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		for newChannel := range channels {
			channel, requests, err := newChannel.Accept()
			if err != nil {
				return err
			}
			request, ok := <-requests
			if !ok || request.Type != "exec" {
				_ = channel.Close()
				return fmt.Errorf("expected exec request")
			}
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				_ = channel.Close()
				return err
			}
			commands <- payload.Command
			_ = request.Reply(true, nil)
			_, _ = io.Copy(io.Discard, channel)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			_ = channel.Close()
			if strings.HasPrefix(payload.Command, "rm -rf") {
				return nil
			}
		}
		return nil
	})
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) { return clientConn, nil }}
	req := bootstrapTestRequest(t)
	req.StagingDir = dir
	req.Uploads = []BootstrapUpload{{Path: dir + "/install.sh", Mode: 0o700, Content: []byte("true\n")}}
	req.RunCommand = "/bin/sh " + shellQuote(dir+"/install.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := executor.Bootstrap(ctx, req); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("SSH test server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH test server did not see the staging dir removed")
	}
	close(commands)
	var got []string
	for c := range commands {
		got = append(got, c)
	}
	if len(got) != 4 || got[0] != buildStagingDirCommand(dir) || !strings.Contains(got[1], shellQuote(dir+"/install.sh")) ||
		got[2] != req.RunCommand || got[3] != "rm -rf -- "+shellQuote(dir) {
		t.Fatalf("commands = %#v, want mkdir, upload, run, rm -rf", got)
	}

	outside := bootstrapTestRequest(t)
	outside.StagingDir = dir
	outside.Uploads = []BootstrapUpload{{Path: "/tmp/elsewhere.sh", Mode: 0o700, Content: []byte("true\n")}}
	if err := (sshBootstrapExecutor{}).Bootstrap(context.Background(), outside); err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("an upload outside the staging dir was not refused: %v", err)
	}
}
