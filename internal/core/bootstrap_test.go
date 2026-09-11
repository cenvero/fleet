// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"golang.org/x/crypto/ssh"
)

func TestBootstrapServerDirectUploadsAgentAndUpdatesServer(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	if err := app.AddServer(ServerRecord{
		Name:    "web-01",
		Address: "192.0.2.10",
		Mode:    transport.ModeDirect,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	executor := &fakeBootstrapExecutor{}
	app.BootstrapExecutor = executor

	result, err := app.BootstrapServer("web-01", BootstrapOptions{
		LoginUser:       "ubuntu",
		LoginPort:       22,
		AgentBinaryPath: agentBinary,
		UseSudo:         true,
	})
	if err != nil {
		t.Fatalf("BootstrapServer() error = %v", err)
	}
	if !result.Executed {
		t.Fatalf("expected bootstrap result to be marked executed")
	}
	if len(executor.requests) != 1 {
		t.Fatalf("expected one bootstrap request, got %d", len(executor.requests))
	}
	if got := len(executor.requests[0].Uploads); got != 4 {
		t.Fatalf("expected 4 uploads for direct bootstrap, got %d", got)
	}
	if !strings.Contains(result.ServiceUnit, "/opt/cenvero-fleet/fleet-agent serve") {
		t.Fatalf("expected direct mode service unit, got %q", result.ServiceUnit)
	}
	if !strings.Contains(result.ServiceUnit, "--listen") {
		t.Fatalf("expected --listen flag in direct mode service unit, got %q", result.ServiceUnit)
	}
	if result.Script != "" {
		t.Fatalf("expected executed bootstrap result to omit the raw script payload")
	}
	if !strings.Contains(string(executor.requests[0].Uploads[len(executor.requests[0].Uploads)-1].Content), "systemctl enable --now") {
		t.Fatalf("expected uploaded bootstrap script to enable systemd service")
	}

	server, err := app.GetServer("web-01")
	if err != nil {
		t.Fatalf("GetServer() error = %v", err)
	}
	if server.User != "root" {
		t.Fatalf("expected server user %q, got %q", "root", server.User)
	}
	if server.Port != 2222 {
		t.Fatalf("expected direct-mode agent port 2222, got %d", server.Port)
	}
	if !server.Agent.Managed {
		t.Fatalf("expected bootstrapped agent to be marked managed")
	}
	if server.Agent.LoginUser != "ubuntu" {
		t.Fatalf("expected stored login user %q, got %q", "ubuntu", server.Agent.LoginUser)
	}
	if server.Agent.LoginPort != 22 {
		t.Fatalf("expected stored login port 22, got %d", server.Agent.LoginPort)
	}
	expectedLoginKey := filepath.Join(configDir, "keys", app.Config.Crypto.PrimaryKey)
	if server.Agent.LoginKey != expectedLoginKey {
		t.Fatalf("expected stored login key %q, got %q", expectedLoginKey, server.Agent.LoginKey)
	}
	if !server.Agent.UseSudo {
		t.Fatalf("expected stored sudo setting")
	}
}

func TestBootstrapServerReversePrintScript(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeReverse,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	if err := app.AddServer(ServerRecord{
		Name:    "edge-01",
		Address: "198.51.100.25",
		Mode:    transport.ModeReverse,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	result, err := app.BootstrapServer("edge-01", BootstrapOptions{
		AgentBinaryPath:   agentBinary,
		ControllerAddress: "203.0.113.7:9443",
		PrintScript:       true,
		UseSudo:           true,
	})
	if err != nil {
		t.Fatalf("BootstrapServer(printScript) error = %v", err)
	}
	if result.Executed {
		t.Fatalf("expected print-script result to avoid execution")
	}
	if !strings.Contains(result.ServiceUnit, "/opt/cenvero-fleet/fleet-agent reverse") {
		t.Fatalf("expected reverse mode service unit, got %q", result.ServiceUnit)
	}
	if !strings.Contains(result.ServiceUnit, "--controller") {
		t.Fatalf("expected --controller flag in reverse mode service unit, got %q", result.ServiceUnit)
	}
	if !strings.Contains(result.ServiceUnit, "203.0.113.7:9443") {
		t.Fatalf("expected controller address in reverse service unit")
	}
	if !strings.Contains(result.Script, "Bootstrapped Cenvero Fleet agent on edge-01") {
		t.Fatalf("expected bootstrap script banner in output")
	}
}

func TestBootstrapServerReverseRequiresControllerAddress(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeReverse,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()

	if err := app.AddServer(ServerRecord{
		Name:    "edge-02",
		Address: "198.51.100.26",
		Mode:    transport.ModeReverse,
	}); err != nil {
		t.Fatalf("AddServer() error = %v", err)
	}

	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(agentBinary) error = %v", err)
	}

	if _, err := app.BootstrapServer("edge-02", BootstrapOptions{
		LoginUser:       "ubuntu",
		AgentBinaryPath: agentBinary,
		UseSudo:         true,
	}); err == nil {
		t.Fatalf("expected reverse bootstrap without controller address to fail")
	}
}

type fakeBootstrapExecutor struct {
	requests []BootstrapRequest
}

func (f *fakeBootstrapExecutor) Bootstrap(_ context.Context, req BootstrapRequest) error {
	f.requests = append(f.requests, req)
	return nil
}

func TestBootstrapServerReverseKeepsEnrollmentCredentialOutOfArgv(t *testing.T) {
	t.Parallel()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeReverse, CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	const secret = "bootstrap-one-use-secret"
	if err := app.AddServer(ServerRecord{Name: "edge-secure", Address: "192.0.2.44", Mode: transport.ModeReverse, EnrollSecret: secret}); err != nil {
		t.Fatal(err)
	}
	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	executor := &fakeBootstrapExecutor{}
	app.BootstrapExecutor = executor
	result, err := app.BootstrapServer("edge-secure", BootstrapOptions{LoginUser: "root", AgentBinaryPath: agentBinary, ControllerAddress: "203.0.113.7:9443"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.ServiceUnit, secret) || strings.Contains(result.Script, secret) {
		t.Fatal("enrollment credential leaked into service unit or script")
	}
	if !strings.Contains(result.ServiceUnit, "--enroll-token-file") || !strings.Contains(result.ServiceUnit, "--controller-fingerprint") || !strings.Contains(result.ServiceUnit, "SHA256:") {
		t.Fatalf("secure reverse flags missing from unit: %s", result.ServiceUnit)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("bootstrap requests = %d, want 1", len(executor.requests))
	}
	foundCredential := false
	for _, upload := range executor.requests[0].Uploads {
		if strings.Contains(string(upload.Content), secret) {
			foundCredential = true
			if upload.Mode.Perm() != 0o600 || !strings.HasSuffix(upload.Path, ".enroll") {
				t.Fatalf("credential upload is not owner-only temporary file: %#v", upload)
			}
		}
		if (strings.HasSuffix(upload.Path, ".service") || strings.HasSuffix(upload.Path, ".sh")) && strings.Contains(string(upload.Content), secret) {
			t.Fatalf("credential leaked into uploaded %s", upload.Path)
		}
	}
	if !foundCredential {
		t.Fatal("bootstrap did not upload the one-use enrollment credential")
	}
}

type failingBootstrapEntropy struct{ err error }

func (r failingBootstrapEntropy) Read([]byte) (int, error) { return 0, r.err }

func TestRandomBootstrapTokenPropagatesEntropyFailure(t *testing.T) {
	injected := errors.New("injected bootstrap entropy failure")
	token, err := randomBootstrapToken(failingBootstrapEntropy{err: injected})
	if !errors.Is(err, injected) {
		t.Fatalf("randomBootstrapToken error = %v, want %v", err, injected)
	}
	if token != "" {
		t.Fatalf("randomBootstrapToken token = %q after entropy failure, want empty", token)
	}
}

func TestBootstrapManagedPathsRemainPOSIX(t *testing.T) {
	cfg := resolvedBootstrapConfig{
		agentListenAddr: "0.0.0.0:2222",
		serviceName:     defaultServiceName,
		enrollTokenPath: TargetPathPOSIX.Join(defaultStateDir, "enroll.token"),
	}
	unit, err := buildAgentServiceUnit(ServerRecord{Name: "linux", Mode: transport.ModeDirect}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"/var/lib/cenvero-fleet-agent/ssh_host_ed25519_key",
		"/var/lib/cenvero-fleet-agent/authorized_keys",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("service unit missing POSIX path %q: %s", want, unit)
		}
	}
	if strings.Contains(unit, `\\`) {
		t.Fatalf("Linux service unit contains a backslash path: %s", unit)
	}
}

func TestValidateBootstrapServiceName(t *testing.T) {
	for _, good := range []string{"cenvero-fleet-agent", "fleet_agent@blue", "fleet.agent:1"} {
		if err := validateBootstrapServiceName(good); err != nil {
			t.Errorf("valid service name %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{"../evil", `dir\\evil`, "fleet agent", "x.service", "$(touch-pwned)", "-H", "--no-block", ""} {
		if err := validateBootstrapServiceName(bad); err == nil {
			t.Errorf("unsafe service name %q accepted", bad)
		}
	}
}

func TestBootstrapServerRetryUsesStoredInstallMetadataAndExplicitAgentPort(t *testing.T) {
	t.Parallel()
	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir: configDir, Alias: "fleet", DefaultMode: transport.ModeDirect,
		CryptoAlgorithm: "ed25519", UpdateChannel: "stable", UpdatePolicy: update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatal(err)
	}
	app, err := Open(configDir)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	loginKey := filepath.Join(t.TempDir(), "bootstrap-key")
	if err := app.AddServer(ServerRecord{
		Name: "retry-node", Address: "192.0.2.50", Port: 22, Mode: transport.ModeDirect,
		Agent: AgentInstall{Status: "failed", LoginUser: "ubuntu", LoginPort: 2200, LoginKey: loginKey},
	}); err != nil {
		t.Fatal(err)
	}
	agentBinary := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	executor := &fakeBootstrapExecutor{}
	app.BootstrapExecutor = executor
	result, err := app.BootstrapServer("retry-node", BootstrapOptions{AgentBinaryPath: agentBinary})
	if err != nil {
		t.Fatal(err)
	}
	if result.LoginUser != "ubuntu" || result.LoginPort != 2200 {
		t.Fatalf("stored login metadata not reused: %+v", result)
	}
	if len(executor.requests) != 1 || executor.requests[0].PrivateKeyPath != loginKey {
		t.Fatalf("stored login key not reused: %+v", executor.requests)
	}
	if !strings.Contains(result.ServiceUnit, "0.0.0.0:22") {
		t.Fatalf("explicit agent port 22 was replaced: %s", result.ServiceUnit)
	}
	stored, err := app.GetServer("retry-node")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Port != 22 || stored.Agent.Status != "managed" {
		t.Fatalf("retry result metadata=%+v", stored)
	}
}

func TestBoundedCaptureDrainsAfterLimit(t *testing.T) {
	capture := newBoundedCapture(4)
	if n, err := capture.Write([]byte("abcdef")); err != nil || n != 6 {
		t.Fatalf("Write()=(%d,%v), want full 6-byte drain", n, err)
	}
	if n, err := capture.Write([]byte("gh")); err != nil || n != 2 {
		t.Fatalf("second Write()=(%d,%v), want full drain", n, err)
	}
	got, truncated := capture.snapshot()
	if string(got) != "abcd" || !truncated {
		t.Fatalf("snapshot=(%q,%t), want bounded abcd and truncation", got, truncated)
	}
}

func newBootstrapTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func startBootstrapTestSSHServer(t *testing.T, handler func(*ssh.ServerConn, <-chan ssh.NewChannel) error) (net.Conn, <-chan error) {
	t.Helper()
	return startBootstrapTestSSHServerWithSigner(t, newBootstrapTestSigner(t), handler)
}

func startBootstrapTestSSHServerWithSigner(t *testing.T, signer ssh.Signer, handler func(*ssh.ServerConn, <-chan ssh.NewChannel) error) (net.Conn, <-chan error) {
	t.Helper()
	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer listener.Close()
		serverConn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		conn, channels, requests, err := ssh.NewServerConn(serverConn, config)
		if err != nil {
			done <- err
			return
		}
		go ssh.DiscardRequests(requests)
		err = handler(conn, channels)
		_ = conn.Close()
		done <- err
	}()
	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	return clientConn, done
}

func bootstrapTestRequest(t *testing.T) BootstrapRequest {
	t.Helper()
	return BootstrapRequest{
		Address:          "bootstrap.test",
		Port:             22,
		User:             "tester",
		Password:         "password",
		KnownHostsPath:   filepath.Join(t.TempDir(), "known_hosts"),
		AcceptNewHostKey: false,
	}
}

func serveBootstrapTestExec(channels <-chan ssh.NewChannel) (string, error) {
	newChannel, ok := <-channels
	if !ok {
		return "", fmt.Errorf("SSH connection closed before cleanup command")
	}
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return "", err
	}
	defer channel.Close()
	request, ok := <-requests
	if !ok || request.Type != "exec" {
		return "", fmt.Errorf("expected cleanup exec request")
	}
	var payload struct{ Command string }
	if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
		return "", err
	}
	if err := request.Reply(true, nil); err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, channel)
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	return payload.Command, nil
}

func TestSSHBootstrapExecutorCancellationInterruptsChannelOpen(t *testing.T) {
	clientConn, serverDone := startBootstrapTestSSHServer(t, func(conn *ssh.ServerConn, _ <-chan ssh.NewChannel) error {
		return conn.Wait()
	})
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}}
	req := bootstrapTestRequest(t)
	req.Uploads = []BootstrapUpload{{Path: "/tmp/cancel-stage", Mode: 0o600, Content: []byte("payload")}}
	req.RunCommand = "true"

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := executor.Bootstrap(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap() error=%v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("channel-open cancellation took %s, want <= 2s", elapsed)
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH test server did not stop after client cancellation")
	}
}

func TestSSHBootstrapExecutorCancellationInterruptsExecRequest(t *testing.T) {
	clientConn, serverDone := startBootstrapTestSSHServer(t, func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		newChannel, ok := <-channels
		if !ok {
			return nil
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return err
		}
		request, ok := <-requests
		if !ok || request.Type != "exec" {
			_ = channel.Close()
			return fmt.Errorf("expected stalled exec request")
		}
		for range requests {
		}
		_ = channel.Close()
		cleanup, err := serveBootstrapTestExec(channels)
		if err != nil {
			return err
		}
		if cleanup != "rm -f '/tmp/cancel-exec' '/tmp/cancel-exec.part'" {
			return fmt.Errorf("unexpected cleanup command %q", cleanup)
		}
		return nil
	})
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}}
	req := bootstrapTestRequest(t)
	req.Uploads = []BootstrapUpload{{Path: "/tmp/cancel-exec", Mode: 0o600, Content: []byte("payload")}}
	req.RunCommand = "true"

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := executor.Bootstrap(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap() error=%v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("exec-request cancellation took %s, want <= 2s", elapsed)
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH test server did not stop after exec cancellation")
	}
}

func TestSSHBootstrapExecutorCleansAttemptedPathsAfterUploadFailure(t *testing.T) {
	commands := make(chan string, 4)
	clientConn, serverDone := startBootstrapTestSSHServer(t, func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		commandNumber := 0
		for newChannel := range channels {
			if newChannel.ChannelType() != "session" {
				_ = newChannel.Reject(ssh.UnknownChannelType, "session required")
				continue
			}
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
			commandNumber++
			if err := request.Reply(true, nil); err != nil {
				_ = channel.Close()
				return err
			}
			_, _ = io.Copy(io.Discard, channel)
			status := uint32(0)
			if commandNumber == 2 {
				status = 1
			}
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			_ = channel.Close()
			if commandNumber == 3 {
				return nil
			}
		}
		return nil
	})
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}}
	req := bootstrapTestRequest(t)
	req.Uploads = []BootstrapUpload{
		{Path: "/tmp/fleet-first", Mode: 0o600, Content: []byte("first")},
		{Path: "/tmp/fleet-second", Mode: 0o600, Content: []byte("second")},
	}
	req.RunCommand = "true"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := executor.Bootstrap(ctx, req); err == nil {
		t.Fatal("Bootstrap() unexpectedly succeeded after second upload failure")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("SSH test server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSH test server did not observe cleanup command")
	}
	close(commands)
	var got []string
	for command := range commands {
		got = append(got, command)
	}
	if len(got) != 3 {
		t.Fatalf("commands=%#v, want two uploads followed by cleanup", got)
	}
	if !strings.Contains(got[0], "'/tmp/fleet-first'") || !strings.Contains(got[1], "'/tmp/fleet-second'") {
		t.Fatalf("upload commands do not target expected paths: %#v", got[:2])
	}
	if got[2] != "rm -f '/tmp/fleet-first' '/tmp/fleet-first.part' '/tmp/fleet-second' '/tmp/fleet-second.part'" {
		t.Fatalf("cleanup command=%q", got[2])
	}
}

func TestRemoteUploadCommandRejectsTruncatedInputAtomically(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "fleet-stage")
	upload := BootstrapUpload{Path: destination, Mode: 0o700, Content: []byte("complete-payload")}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteUploadCommand(upload))
	cmd.Stdin = strings.NewReader("truncated")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("truncated upload unexpectedly succeeded: %q", output)
	}
	for _, path := range []string{destination, remoteUploadPartialPath(destination)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("truncated upload left %q behind, stat error=%v", path, err)
		}
	}
}

func TestSSHBootstrapExecutorCancellationInterruptsSessionWait(t *testing.T) {
	reachedWait := make(chan struct{})
	clientConn, serverDone := startBootstrapTestSSHServer(t, func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		newChannel, ok := <-channels
		if !ok {
			return nil
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return err
		}
		request, ok := <-requests
		if !ok || request.Type != "exec" {
			_ = channel.Close()
			return fmt.Errorf("expected exec request")
		}
		if err := request.Reply(true, nil); err != nil {
			_ = channel.Close()
			return err
		}
		_, _ = io.Copy(io.Discard, channel)
		close(reachedWait)
		for range requests {
		}
		_ = channel.Close()
		cleanup, err := serveBootstrapTestExec(channels)
		if err != nil {
			return err
		}
		if cleanup != "rm -f '/tmp/cancel-wait' '/tmp/cancel-wait.part'" {
			return fmt.Errorf("unexpected cleanup command %q", cleanup)
		}
		return nil
	})
	executor := sshBootstrapExecutor{networkDialContext: func(context.Context, string, string) (net.Conn, error) {
		return clientConn, nil
	}}
	req := bootstrapTestRequest(t)
	req.Uploads = []BootstrapUpload{{Path: "/tmp/cancel-wait", Mode: 0o600, Content: []byte("payload")}}
	req.RunCommand = "true"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := executor.Bootstrap(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap() error=%v, want context deadline", err)
	}
	select {
	case <-reachedWait:
	default:
		t.Fatal("server did not reach the stalled session.Wait phase")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("session.Wait cancellation took %s, want <= 2s", elapsed)
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH test server did not stop after session.Wait cancellation")
	}
}

func TestSSHBootstrapExecutorReconnectsForCleanupAfterChannelOpenCancellation(t *testing.T) {
	signer := newBootstrapTestSigner(t)
	firstConn, firstDone := startBootstrapTestSSHServerWithSigner(t, signer, func(conn *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		if _, err := serveBootstrapTestExec(channels); err != nil {
			return err
		}
		_ = conn.Wait()
		return nil
	})
	cleanupCommands := make(chan string, 1)
	secondConn, secondDone := startBootstrapTestSSHServerWithSigner(t, signer, func(_ *ssh.ServerConn, channels <-chan ssh.NewChannel) error {
		command, err := serveBootstrapTestExec(channels)
		if err == nil {
			cleanupCommands <- command
		}
		return err
	})
	connections := make(chan net.Conn, 2)
	connections <- firstConn
	connections <- secondConn
	executor := sshBootstrapExecutor{networkDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case conn := <-connections:
			return conn, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	req := bootstrapTestRequest(t)
	req.Uploads = []BootstrapUpload{
		{Path: "/tmp/reconnect-first", Mode: 0o600, Content: []byte("first")},
		{Path: "/tmp/reconnect-second", Mode: 0o600, Content: []byte("second")},
	}
	req.RunCommand = "true"

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := executor.Bootstrap(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Bootstrap() error=%v, want context deadline", err)
	}
	select {
	case command := <-cleanupCommands:
		want := "rm -f '/tmp/reconnect-first' '/tmp/reconnect-first.part' '/tmp/reconnect-second' '/tmp/reconnect-second.part'"
		if command != want {
			t.Fatalf("cleanup command=%q, want %q", command, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fresh SSH connection did not receive cleanup command")
	}
	for name, done := range map[string]<-chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s SSH test server: %v", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s SSH test server did not stop", name)
		}
	}
}
