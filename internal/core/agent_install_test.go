// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aead.dev/minisign"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

type fakeRemoteOutputRunner struct {
	uname      string
	manifest   []byte
	archive    []byte
	signature  []byte
	archiveURL string
	calls      []string
}

func (f *fakeRemoteOutputRunner) Output(_ context.Context, command string, limit int64) ([]byte, error) {
	f.calls = append(f.calls, command)
	var data []byte
	switch {
	case command == "uname -s && uname -m":
		data = []byte(f.uname)
	case strings.Contains(command, update.DefaultManifestURL):
		data = f.manifest
	case strings.Contains(command, f.archiveURL+".minisig"):
		data = f.signature
	case strings.Contains(command, f.archiveURL):
		data = f.archive
	default:
		return nil, fmt.Errorf("unexpected remote command: %s", command)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("remote output exceeds %d bytes", limit)
	}
	return append([]byte(nil), data...), nil
}

func agentReleaseFixture(t *testing.T, target agentLinuxTarget, trustedComment string) (update.Manifest, []byte, []byte, string, []byte) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("binary-" + target.name)
	archive := agentTestArchive(t, binary)
	sum := sha256.Sum256(archive)
	url := "https://github.com/cenvero/fleet/releases/download/v1.2.3/fleet-agent_1.2.3_linux_" + target.arch + ".tar.gz"
	manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{
		"v1.2.3": {
			target.name: {URL: url, Signature: url + ".minisig", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))},
		},
	}}
	if trustedComment == "" {
		trustedComment = "cenvero-fleet fleet-agent v1.2.3 " + target.name
	}
	signature := minisign.SignWithComments(priv, archive, trustedComment, "test")
	return manifest, archive, signature, string(pubText), binary
}

func TestAgentTargetForUname(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"x86_64":  "linux-amd64",
		"amd64":   "linux-amd64",
		"aarch64": "linux-arm64",
		"arm64":   "linux-arm64",
		"armv7l":  "linux-armv7",
		"armv7":   "linux-armv7",
	}
	for machine, want := range tests {
		target, err := agentTargetForUname("Linux", machine)
		if err != nil || target.name != want {
			t.Fatalf("agentTargetForUname(Linux,%s)=(%+v,%v), want %s", machine, target, err, want)
		}
	}
	if _, err := agentTargetForUname("Darwin", "arm64"); err == nil {
		t.Fatal("non-Linux target unexpectedly accepted")
	}
	if _, err := agentTargetForUname("Linux", "s390x"); err == nil {
		t.Fatal("unsupported Linux architecture unexpectedly accepted")
	}
}

func TestFetchVerifiedAgentReleaseDownloadsOnlyDetectedTarget(t *testing.T) {
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, wantBinary := agentReleaseFixture(t, target, "")
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRemoteOutputRunner{
		uname: "Linux\nx86_64\n", manifest: manifestData, archive: archive,
		signature: signature, archiveURL: manifest.AgentBinaries["v1.2.3"][target.name].URL,
	}
	got, err := fetchVerifiedAgentReleaseWithKey(context.Background(), runner, BootstrapAgentRelease{Version: "1.2.3"}, pubText)
	if err != nil {
		t.Fatalf("fetchVerifiedAgentReleaseWithKey() error = %v", err)
	}
	if !bytes.Equal(got, wantBinary) {
		t.Fatalf("verified binary=%q, want %q", got, wantBinary)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("remote calls=%d, want uname + manifest + archive + signature: %#v", len(runner.calls), runner.calls)
	}
	joined := strings.Join(runner.calls, "\n")
	if strings.Contains(joined, "linux_arm64") || strings.Contains(joined, "linux_armv7") {
		t.Fatalf("unrelated architecture was requested: %s", joined)
	}
	if strings.Index(runner.calls[1], "command -v wget") > strings.Index(runner.calls[1], "command -v curl") {
		t.Fatalf("target downloader does not prefer wget before curl: %s", runner.calls[1])
	}
	for _, want := range []string{"WGET_ATTEMPT=1", `while [ "$WGET_ATTEMPT" -le 3 ]`, "wget -q -T 30", "curl -fL", "--retry 3", "--proto '=https'"} {
		if !strings.Contains(runner.calls[1], want) {
			t.Fatalf("target downloader missing %q: %s", want, runner.calls[1])
		}
	}
	if strings.Contains(runner.calls[1], "wget -q -t") {
		t.Fatalf("target downloader uses GNU-only wget retry flag: %s", runner.calls[1])
	}
}

func TestSelectedAgentReleaseFailsClosed(t *testing.T) {
	target := agentLinuxTargets[0]
	manifest, archive, signature, pubText, _ := agentReleaseFixture(t, target, "")
	selected, err := selectAgentRelease("v1.2.3", target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	badHash := selected
	badHash.info.SHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := verifySelectedAgentRelease(badHash, archive, signature, pubText); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum tampering error=%v", err)
	}
	if _, err := verifySelectedAgentRelease(selected, archive[:len(archive)-1], signature, pubText); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("short archive error=%v", err)
	}
	if _, err := verifySelectedAgentRelease(selected, archive, []byte("not a minisign signature"), pubText); err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("invalid signature error=%v", err)
	}

	wrongManifest, wrongArchive, wrongBinding, wrongPub, _ := agentReleaseFixture(t, target, "cenvero-fleet fleet-agent v9.9.9 linux-amd64")
	wrongSelected, err := selectAgentRelease("v1.2.3", target, wrongManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifySelectedAgentRelease(wrongSelected, wrongArchive, wrongBinding, wrongPub); err == nil || !strings.Contains(err.Error(), "signature binding mismatch") {
		t.Fatalf("wrong trusted comment error=%v", err)
	}
	badChecksumManifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{
		"v1.2.3": {target.name: selected.info},
	}}
	badChecksumEntry := badChecksumManifest.AgentBinaries["v1.2.3"][target.name]
	badChecksumEntry.SHA256 = strings.Repeat("z", sha256.Size*2)
	badChecksumManifest.AgentBinaries["v1.2.3"][target.name] = badChecksumEntry
	if _, err := selectAgentRelease("v1.2.3", target, badChecksumManifest); err == nil || !strings.Contains(err.Error(), "invalid checksum") {
		t.Fatalf("malformed checksum metadata error=%v", err)
	}
	if _, err := selectAgentRelease("v1.2.3", target, update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{"v1.2.3": {}}}); err == nil || !strings.Contains(err.Error(), "missing target") {
		t.Fatalf("missing target error=%v", err)
	}

	badManifest := manifest
	entry := badManifest.AgentBinaries["v1.2.3"][target.name]
	entry.URL = "https://example.com/agent.tar.gz"
	badManifest.AgentBinaries["v1.2.3"][target.name] = entry
	if _, err := selectAgentRelease("v1.2.3", target, badManifest); err == nil {
		t.Fatal("unbound manifest URL unexpectedly accepted")
	}
}

func TestFetchVerifiedAgentReleaseStopsBeforeArtifactOnInvalidManifest(t *testing.T) {
	manifest := update.Manifest{AgentBinaries: map[string]map[string]update.BinaryInfo{"v1.2.3": {}}}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRemoteOutputRunner{uname: "Linux\nx86_64\n", manifest: manifestData}
	_, err = fetchVerifiedAgentReleaseWithKey(context.Background(), runner, BootstrapAgentRelease{Version: "v1.2.3"}, "invalid key never reached")
	if err == nil || !strings.Contains(err.Error(), "missing target") {
		t.Fatalf("error=%v, want missing target", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls=%d, want uname and manifest only: %#v", len(runner.calls), runner.calls)
	}
}

func TestRemoteAgentDownloadCommandFallsBackFromWgetToCurl(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "wget"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	curlScript := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then
    shift
    printf 'remote-payload' > "$1"
    exit 0
  fi
  shift
done
exit 2
`
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(curlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommand("https://example.com/artifact", 1<<20))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote downloader shell failed: %v", err)
	}
	if string(output) != "remote-payload" {
		t.Fatalf("output=%q, want curl fallback payload", output)
	}
}

func TestRemoteAgentDownloadCommandPrefersSuccessfulWget(t *testing.T) {
	binDir := t.TempDir()
	curlMarker := filepath.Join(t.TempDir(), "curl-called")
	wgetScript := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-O" ]; then
    shift
    printf 'wget-payload' > "$1"
    exit 0
  fi
  shift
done
exit 2
`
	if err := os.WriteFile(filepath.Join(binDir, "wget"), []byte(wgetScript), 0o755); err != nil {
		t.Fatal(err)
	}
	curlScript := "#!/bin/sh\nprintf called > \"$CURL_MARKER\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(curlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommand("https://example.com/artifact", 1<<20))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin", "CURL_MARKER="+curlMarker)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("remote downloader shell failed: %v", err)
	}
	if string(output) != "wget-payload" {
		t.Fatalf("output=%q, want wget payload", output)
	}
	if _, err := os.Stat(curlMarker); !os.IsNotExist(err) {
		t.Fatalf("curl ran after successful wget, stat error=%v", err)
	}
}

func TestRemoteAgentDownloadCommandRetriesBusyBoxCompatibleWget(t *testing.T) {
	binDir := t.TempDir()
	countPath := filepath.Join(t.TempDir(), "wget-count")
	curlMarker := filepath.Join(t.TempDir(), "curl-called")
	wgetScript := `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -q) shift ;;
    -T) shift 2 ;;
    -O) out=$2; shift 2 ;;
    https://*) shift ;;
    *) exit 64 ;;
  esac
done
count=0
if [ -f "$COUNT_PATH" ]; then count=$(cat "$COUNT_PATH"); fi
count=$((count + 1))
printf '%s' "$count" > "$COUNT_PATH"
if [ "$count" -lt 3 ]; then exit 1; fi
printf 'busybox-wget-payload' > "$out"
`
	if err := os.WriteFile(filepath.Join(binDir, "wget"), []byte(wgetScript), 0o755); err != nil {
		t.Fatal(err)
	}
	curlScript := "#!/bin/sh\nprintf called > \"$CURL_MARKER\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(curlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommand("https://example.com/artifact", 1<<20))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin", "COUNT_PATH="+countPath, "CURL_MARKER="+curlMarker)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("BusyBox-compatible wget retry shell failed: %v", err)
	}
	if string(output) != "busybox-wget-payload" {
		t.Fatalf("output=%q, want BusyBox-compatible wget payload", output)
	}
	attempts, err := os.ReadFile(countPath)
	if err != nil || string(attempts) != "3" {
		t.Fatalf("wget attempts=%q, err=%v, want 3", attempts, err)
	}
	if _, err := os.Stat(curlMarker); !os.IsNotExist(err) {
		t.Fatalf("curl ran after third wget attempt succeeded, stat error=%v", err)
	}
}

func TestRemoteAgentDownloadCommandRejectsOversizedPayloadAndCleansTemp(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "target-temp-path")
	wgetScript := `#!/bin/sh
out=
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-O" ]; then
    shift
    out=$1
    break
  fi
  shift
done
printf '%s' "$out" > "$RECORD_PATH"
dd if=/dev/zero of="$out" bs=1024 count=8 2>/dev/null
`
	if err := os.WriteFile(filepath.Join(binDir, "wget"), []byte(wgetScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommandWithTimeout("https://example.com/oversized", 512, 5))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin", "RECORD_PATH="+marker)
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("oversized target download unexpectedly succeeded: %q", output)
	}
	tmpPath, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read recorded target temp path: %v", err)
	}
	if _, err := os.Stat(string(tmpPath)); !os.IsNotExist(err) {
		t.Fatalf("target temp file was not removed, stat error=%v", err)
	}
}

func TestRemoteAgentDownloadCommandAppliesSharedDeadline(t *testing.T) {
	binDir := t.TempDir()
	curlMarker := filepath.Join(t.TempDir(), "curl-called")
	tmpMarker := filepath.Join(t.TempDir(), "target-temp-path")
	wgetScript := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-O" ]; then
    shift
    printf '%s' "$1" > "$RECORD_PATH"
    break
  fi
  shift
done
exec sleep 10
`
	if err := os.WriteFile(filepath.Join(binDir, "wget"), []byte(wgetScript), 0o755); err != nil {
		t.Fatal(err)
	}
	curlScript := "#!/bin/sh\nprintf called > \"$CURL_MARKER\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(curlScript), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", buildRemoteAgentDownloadCommandWithTimeout("https://example.com/trickle", 1<<20, 1))
	cmd.Env = append(os.Environ(), "PATH="+binDir+":/usr/bin:/bin", "CURL_MARKER="+curlMarker, "RECORD_PATH="+tmpMarker)
	started := time.Now()
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("trickling target download unexpectedly succeeded: %q", output)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("shared target download deadline took %s, want <= 4s", elapsed)
	}
	if _, err := os.Stat(curlMarker); !os.IsNotExist(err) {
		t.Fatalf("curl fallback ran after the shared deadline, stat error=%v", err)
	}
	tmpPath, err := os.ReadFile(tmpMarker)
	if err != nil {
		t.Fatalf("read recorded target temp path: %v", err)
	}
	if _, err := os.Stat(string(tmpPath)); !os.IsNotExist(err) {
		t.Fatalf("interrupted target temp file was not removed, stat error=%v", err)
	}
}

func TestAutoInstallReleaseDefersArtifactDownloadToSSHExecutor(t *testing.T) {
	oldVersion := version.Version
	version.Version = "v1.2.3"
	t.Cleanup(func() { version.Version = oldVersion })

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
	if err := app.AddServer(ServerRecord{Name: "remote-release", Address: "192.0.2.70", Port: 5911, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}
	executor := &failingAgentInstallExecutor{err: errors.New("stop after request capture")}
	app.BootstrapExecutor = executor
	err = app.AutoInstallAgentContext(context.Background(), "remote-release", "root", "", "password", 22, false)
	if err == nil || len(executor.requests) != 1 {
		t.Fatalf("error=%v requests=%d", err, len(executor.requests))
	}
	req := executor.requests[0]
	if req.AgentRelease == nil || req.AgentRelease.Version != "v1.2.3" || !strings.HasSuffix(req.AgentRelease.DestinationPath, ".bin") {
		t.Fatalf("release request=%+v", req.AgentRelease)
	}
	for _, upload := range req.Uploads {
		if upload.Path == req.AgentRelease.DestinationPath || strings.Contains(upload.Path, "-amd64.bin") || strings.Contains(upload.Path, "-arm64.bin") || strings.Contains(upload.Path, "-armv7.bin") {
			t.Fatalf("controller pre-uploaded a release binary: %+v", upload)
		}
	}
	if !strings.Contains(req.RunCommand, ".sh") {
		t.Fatalf("missing install command: %s", req.RunCommand)
	}
	script := string(req.Uploads[len(req.Uploads)-1].Content)
	if !strings.Contains(script, req.AgentRelease.DestinationPath) || strings.Contains(script, "AMD64_BIN") {
		t.Fatalf("install script is not single-binary: %s", script)
	}
}

func agentTestArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "fleet-agent", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type failingAgentInstallExecutor struct {
	requests []BootstrapRequest
	err      error
}

func (f *failingAgentInstallExecutor) Bootstrap(_ context.Context, req BootstrapRequest) error {
	f.requests = append(f.requests, req)
	return f.err
}

func TestAutoInstallAgentPreservesPortAndRecordsFailure(t *testing.T) {
	oldVersion := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = oldVersion })

	binDir := t.TempDir()
	agentBinary := filepath.Join(binDir, "fleet-agent")
	if err := os.WriteFile(agentBinary, []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

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
	if err := app.AddServer(ServerRecord{Name: "herm", Address: "91.99.197.39", Port: 5911, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}

	executor := &failingAgentInstallExecutor{err: errors.New("remote unavailable")}
	app.BootstrapExecutor = executor
	const password = "do-not-persist-this-password"
	err = app.AutoInstallAgentContext(context.Background(), "herm", "ubuntu", "", password, 2200, true)
	if err == nil || !strings.Contains(err.Error(), "remote unavailable") {
		t.Fatalf("AutoInstallAgentContext() error=%v", err)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("bootstrap requests=%d, want 1", len(executor.requests))
	}
	req := executor.requests[0]
	if req.Port != 2200 {
		t.Fatalf("bootstrap login port=%d, want 2200", req.Port)
	}
	serviceUnit := string(req.Uploads[0].Content)
	if !strings.Contains(serviceUnit, "0.0.0.0:5911") {
		t.Fatalf("service unit did not preserve configured agent port: %s", serviceUnit)
	}

	stored, err := app.GetServer("herm")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Port != 5911 || stored.Agent.LoginPort != 2200 || stored.Agent.LoginUser != "ubuntu" || !stored.Agent.UseSudo {
		t.Fatalf("failed install lost retry metadata: %+v", stored)
	}
	if stored.Agent.Managed || stored.Agent.Status != "failed" || !strings.Contains(stored.Agent.LastError, "remote unavailable") {
		t.Fatalf("failed install state=%+v", stored.Agent)
	}
	recordData, err := os.ReadFile(filepath.Join(configDir, "servers", "herm.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(recordData), password) {
		t.Fatal("login password was persisted in the server record")
	}
}

func TestAutoInstallAgentAuditFailureDoesNotMisclassifyInstall(t *testing.T) {
	oldVersion := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = oldVersion })

	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "fleet-agent"), []byte("agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

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
	if err := app.AddServer(ServerRecord{Name: "audit-node", Address: "192.0.2.60", Port: 3022, Mode: transport.ModeDirect}); err != nil {
		t.Fatal(err)
	}
	app.BootstrapExecutor = &fakeBootstrapExecutor{}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	app.AuditLog = logs.NewAuditLog(filepath.Join(blocker, "audit.log"))

	err = app.AutoInstallAgentContext(context.Background(), "audit-node", "root", "", "", 22, false)
	var completionErr *AgentInstallCompletionError
	if !errors.As(err, &completionErr) || !completionErr.ManagedStateSaved {
		t.Fatalf("error=%v, want managed-state-saved completion error", err)
	}
	stored, getErr := app.GetServer("audit-node")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if !stored.Agent.Managed || stored.Agent.Status != "managed" {
		t.Fatalf("audit failure misclassified successful install: %+v", stored.Agent)
	}
}
