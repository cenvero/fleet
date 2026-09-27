// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aead.dev/minisign"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"golang.org/x/crypto/blake2b"
)

const agentGitHubRepo = "cenvero/fleet"

// AgentInstallCompletionError reports that the remote agent installation
// completed but a controller-side finalization step failed. ManagedStateSaved
// distinguishes an audit-only failure from a record that needs reconciliation.
type AgentInstallCompletionError struct {
	ManagedStateSaved bool
	Err               error
}

func (e *AgentInstallCompletionError) Error() string {
	if e == nil {
		return "agent install completion error"
	}
	if e.ManagedStateSaved {
		return fmt.Sprintf("agent installed and managed, but audit logging failed: %v", e.Err)
	}
	return fmt.Sprintf("agent installed remotely, but controller state needs reconciliation: %v", e.Err)
}

func (e *AgentInstallCompletionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AutoInstallAgent downloads and installs fleet-agent on a remote Linux server via SSH,
// creates a systemd service, and marks the agent as managed in the server record.
// If version.Version is "dev", it falls back to uploading a local binary.
func (a *App) AutoInstallAgent(serverName, loginUser, loginKeyPath, loginPassword string, loginPort int, useSudo bool) error {
	return a.AutoInstallAgentContext(context.Background(), serverName, loginUser, loginKeyPath, loginPassword, loginPort, useSudo)
}

// AutoInstallAgentContext is AutoInstallAgent with caller cancellation propagated
// through release downloads and the SSH bootstrap operation.
func (a *App) AutoInstallAgentContext(ctx context.Context, serverName, loginUser, loginKeyPath, loginPassword string, loginPort int, useSudo bool) (resultErr error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	loginUser = strings.TrimSpace(loginUser)
	if loginUser == "" {
		return fmt.Errorf("agent install login user is required")
	}
	if loginPort == 0 {
		loginPort = 22
	}
	if loginPort < 1 || loginPort > 65535 {
		return fmt.Errorf("invalid login port %d: must be 1-65535", loginPort)
	}
	if loginKeyPath == "" {
		loginKeyPath = filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	}

	serviceName := defaultServiceName
	agentPort := server.Port
	if agentPort == 0 {
		agentPort = a.Config.Runtime.DefaultAgentPort
		if agentPort == 0 {
			agentPort = 2222
		}
	}
	if agentPort < 1 || agentPort > 65535 {
		return fmt.Errorf("invalid agent port %d: must be 1-65535", agentPort)
	}

	// Store only non-secret retry metadata before network activity. Passwords are
	// deliberately never persisted.
	server.Port = agentPort
	server.Agent = AgentInstall{
		Managed:     false,
		Status:      "installing",
		BinaryPath:  defaultAgentBinaryPath,
		ServiceName: serviceName,
		LoginUser:   loginUser,
		LoginPort:   loginPort,
		LoginKey:    loginKeyPath,
		UseSudo:     useSudo,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := a.SaveServer(server); err != nil {
		return fmt.Errorf("store agent install intent: %w", err)
	}
	installComplete := false
	defer func() {
		if resultErr == nil || installComplete {
			return
		}
		status := "failed"
		var completionErr *AgentInstallCompletionError
		if errors.As(resultErr, &completionErr) && !completionErr.ManagedStateSaved {
			status = "reconcile-required"
		}
		if stateErr := a.recordAgentInstallProblem(serverName, status, resultErr); stateErr != nil {
			resultErr = fmt.Errorf("%w (also failed to store install state: %v)", resultErr, stateErr)
		}
	}()

	sudo := ""
	if useSudo {
		sudo = "sudo "
	}

	pubKeyData, err := os.ReadFile(filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey+".pub"))
	if err != nil {
		return fmt.Errorf("read controller public key: %w", err)
	}

	serviceUnit, err := buildAgentServiceUnit(server, resolvedBootstrapConfig{
		serviceName:     serviceName,
		agentListenAddr: fmt.Sprintf("0.0.0.0:%d", agentPort),
		agentPort:       agentPort,
	})
	if err != nil {
		return err
	}

	token, err := randomBootstrapToken(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate unpredictable agent install paths: %w", err)
	}
	stagingDir := remoteStagingDir(token)
	tempServicePath := stagingDir + "/agent.service"
	tempKeysPath := stagingDir + "/agent.keys"
	tempScriptPath := stagingDir + "/install.sh"

	uploads := []BootstrapUpload{
		{Path: tempServicePath, Mode: 0o600, Content: []byte(serviceUnit)},
		{Path: tempKeysPath, Mode: 0o644, Content: pubKeyData},
	}

	var installScript string
	var agentRelease *BootstrapAgentRelease
	if version.Version == "dev" {
		// Development build: upload local binary
		agentBinaryPath, err := resolveAgentBinaryPath("")
		if err != nil {
			return fmt.Errorf("dev build: %w", err)
		}
		binaryData, err := os.ReadFile(agentBinaryPath) // #nosec G304 -- path is generated by the verified release download staging flow
		if err != nil {
			return fmt.Errorf("read agent binary: %w", err)
		}
		tempBinPath := stagingDir + "/agent.bin"
		uploads = append(uploads, BootstrapUpload{
			Path:    tempBinPath,
			Mode:    0o700,
			Content: binaryData,
		})
		installScript = buildLocalBinaryInstallScript(server, sudo, serviceName, tempBinPath, tempServicePath, tempKeysPath, tempScriptPath)
	} else {
		// Release builds connect to the target first. Over that authenticated,
		// host-key-pinned SSH connection the target detects its architecture and
		// downloads only its matching manifest/archive/signature with wget or curl,
		// keeping the archive in the private staging directory. The target reports
		// the archive's size and digests; the controller checks them against the
		// minisign signature, its binding and the manifest's SHA-256, and the target
		// unpacks the binary only once they pass. A target that cannot hash or
		// unpack the archive sends it back to be verified and unpacked here.
		tempBinPath := stagingDir + "/agent.bin"
		agentRelease = &BootstrapAgentRelease{
			Version:         version.Version,
			DestinationPath: tempBinPath,
		}
		installScript = buildVerifiedBinaryInstallScript(server, sudo, serviceName, tempBinPath, tempServicePath, tempKeysPath, tempScriptPath)
	}

	uploads = append(uploads, BootstrapUpload{
		Path:    tempScriptPath,
		Mode:    0o700,
		Content: []byte(installScript),
	})

	executor := a.BootstrapExecutor
	if executor == nil {
		executor = sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	}
	req := BootstrapRequest{
		Address:        server.Address,
		Port:           loginPort,
		User:           loginUser,
		PrivateKeyPath: loginKeyPath,
		Password:       loginPassword,
		KnownHostsPath: filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		// SECURITY (host-key TOFU): never silently force-replace a pinned host key
		// during a (re-)bootstrap. AcceptNewHostKey maps to forceReplace in the TOFU
		// callback, which on a MISMATCH against an existing pin overwrites it instead
		// of refusing — a MITM during a re-bootstrap could then swap the host key
		// undetected. First-use pinning (no existing pin in bootstrap_known_hosts)
		// always happens regardless of this flag, so leaving it false still pins a
		// brand-new host while REQUIRING a re-presented key to match any existing pin
		// (and refusing with a MITM warning on mismatch). A deliberate re-pin must go
		// through the explicit operator path: `fleet server bootstrap
		// --accept-new-host-key` / `fleet server reconnect --accept-new-host-key`
		// after out-of-band verification, not this silent install flow.
		AcceptNewHostKey: false,
		// On an interactive `fleet server add`, the CLI sets this so a CHANGED host
		// key prompts the operator (y/N) instead of hard-failing — an explicit,
		// non-silent re-pin path. nil keeps the fail-closed default above.
		HostKeyChangedPrompt: a.HostKeyChangedPrompt,
		AgentRelease:         agentRelease,
		Uploads:              uploads,
		RunCommand:           "/bin/sh " + shellQuote(tempScriptPath),
		StagingDir:           stagingDir,
	}
	if err := executor.Bootstrap(ctx, req); err != nil {
		return fmt.Errorf("agent install: %w", err)
	}

	server.User = "root"
	server.Port = agentPort
	server.Agent = AgentInstall{
		Managed:     true,
		Status:      "managed",
		BinaryPath:  defaultAgentBinaryPath,
		ServiceName: serviceName,
		LoginUser:   loginUser,
		LoginPort:   loginPort,
		LoginKey:    loginKeyPath,
		UseSudo:     useSudo,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := a.SaveServer(server); err != nil {
		return &AgentInstallCompletionError{
			ManagedStateSaved: false,
			Err:               fmt.Errorf("save managed state: %w", err),
		}
	}
	installComplete = true
	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "agent.install",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("version=%s service=%s port=%d login=%s:%d", version.Version, serviceName, agentPort, loginUser, loginPort),
	}); err != nil {
		return &AgentInstallCompletionError{ManagedStateSaved: true, Err: err}
	}
	return nil
}

func (a *App) recordAgentInstallProblem(serverName, status string, cause error) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	server.Agent.Managed = false
	server.Agent.Status = status
	server.Agent.LastError = boundedAgentInstallError(cause)
	server.Agent.UpdatedAt = time.Now().UTC()
	return a.SaveServer(server)
}

func boundedAgentInstallError(err error) string {
	if err == nil {
		return ""
	}
	const maxLen = 2048
	message := strings.TrimSpace(err.Error())
	if len(message) <= maxLen {
		return message
	}
	return message[:maxLen-3] + "..."
}

// TeardownAgent SSHes to the server using stored login credentials and removes the managed agent.
func (a *App) TeardownAgent(server ServerRecord) error {
	return a.TeardownAgentWithPassword(server, "")
}

// TeardownAgentWithPassword is like TeardownAgent but accepts an explicit password,
// overriding key-based auth. Used by the --via-ssh remove flow.
func (a *App) TeardownAgentWithPassword(server ServerRecord, password string) error {
	if !server.Agent.Managed {
		return nil
	}

	loginUser := server.Agent.LoginUser
	if loginUser == "" {
		return fmt.Errorf("cannot teardown agent on %q: no login user stored (pass --login-user)", server.Name)
	}
	loginPort := server.Agent.LoginPort
	if loginPort == 0 {
		loginPort = 22
	}
	loginKeyPath := server.Agent.LoginKey
	if password != "" {
		loginKeyPath = "" // use password auth
	} else if loginKeyPath == "" {
		loginKeyPath = filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	}

	sudo := ""
	if server.Agent.UseSudo {
		sudo = "sudo "
	}

	token, err := randomBootstrapToken(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate unpredictable agent teardown path: %w", err)
	}
	teardownStagingDir := remoteStagingDir(token)
	tempTeardownPath := teardownStagingDir + "/teardown.sh"
	script := buildAgentTeardownScript(server.Agent.ServiceName, sudo, tempTeardownPath)
	executor := sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	req := BootstrapRequest{
		Address:        server.Address,
		Port:           loginPort,
		User:           loginUser,
		PrivateKeyPath: loginKeyPath,
		Password:       password,
		KnownHostsPath: filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		// SECURITY (host-key TOFU): same rule as AutoInstallAgent — never silently
		// force-replace a pinned host key. Setting AcceptNewHostKey=false still pins a
		// never-seen host on first use but REQUIRES a matching key when a pin already
		// exists, refusing with a MITM warning on mismatch. A deliberate re-pin goes
		// through the explicit --accept-new-host-key operator path.
		AcceptNewHostKey:     false,
		HostKeyChangedPrompt: a.HostKeyChangedPrompt,
		Uploads: []BootstrapUpload{
			{Path: tempTeardownPath, Mode: 0o700, Content: []byte(script)},
		},
		RunCommand: "/bin/sh " + shellQuote(tempTeardownPath),
		StagingDir: teardownStagingDir,
	}
	if err := executor.Bootstrap(context.Background(), req); err != nil {
		return fmt.Errorf("agent teardown: %w", err)
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "agent.teardown",
		Target:   server.Name,
		Operator: a.operator(),
	})
}

// AgentInstallInstructions returns manual installation instructions for non-Linux platforms.
func AgentInstallInstructions(server ServerRecord, mode transport.Mode) string {
	ver := version.Version
	if ver == "dev" {
		ver = "<version>"
	}
	switch mode {
	case transport.ModeReverse:
		enrollSetup := ""
		note := "Add it to your init system to start on boot."
		if server.EnrollSecret != "" {
			enrollSetup = fmt.Sprintf("  install -m 600 /dev/null /path/to/fleet-enroll.token\n  # securely write this one-time token to the file: %s\n\n", server.EnrollSecret)
			note = "The enrollment file is removed after successful authentication. Re-mint with 'fleet server enroll-token " + server.Name + "'."
		}
		return fmt.Sprintf(
			"To install the agent on %s, download fleet-agent_%s for your OS/arch from\n"+
				"https://github.com/%s/releases. Verify the controller fingerprint shown by\n"+
				"'fleet key fingerprint', then run:\n\n%s"+
				"  fleet-agent reverse --controller <this-controller-address> --server-name %s --controller-fingerprint <verified-SHA256-fingerprint> --enroll-token-file /path/to/fleet-enroll.token\n\n"+
				"%s",
			server.Name, ver, agentGitHubRepo, enrollSetup, server.Name, note,
		)
	default:
		agentPort := server.Port
		if agentPort == 0 {
			agentPort = 2222
		}
		return fmt.Sprintf(
			"To install the agent on %s, download fleet-agent_%s for your OS/arch from\n"+
				"https://github.com/%s/releases and run:\n\n"+
				"  fleet-agent serve --listen 0.0.0.0:%d\n\n"+
				"Add it to your init system and ensure port %d is reachable.\n"+
				"Then run: fleet server reconnect %s",
			server.Name, ver, agentGitHubRepo, agentPort, agentPort, server.Name,
		)
	}
}

type agentLinuxTarget struct {
	name string
	arch string
}

var agentLinuxTargets = []agentLinuxTarget{
	{name: "linux-amd64", arch: "amd64"},
	{name: "linux-arm64", arch: "arm64"},
	{name: "linux-armv7", arch: "armv7"},
}

const (
	maxAgentArtifactBytes             = 512 << 20
	maxRemoteAgentManifestBytes       = 8 << 20
	maxAgentSignatureBytes            = 1 << 20
	remoteAgentDownloadTimeoutSeconds = 300
)

type remoteOutputRunner interface {
	Output(context.Context, string, int64) ([]byte, error)
}

type selectedAgentRelease struct {
	versionTag string
	target     agentLinuxTarget
	info       update.BinaryInfo
}

func agentTargetForUname(system, machine string) (agentLinuxTarget, error) {
	if !strings.EqualFold(strings.TrimSpace(system), "linux") {
		return agentLinuxTarget{}, fmt.Errorf("agent auto-install supports Linux targets only, got %q", strings.TrimSpace(system))
	}
	switch strings.ToLower(strings.TrimSpace(machine)) {
	case "x86_64", "amd64":
		return agentLinuxTargets[0], nil
	case "aarch64", "arm64":
		return agentLinuxTargets[1], nil
	case "armv7l", "armv7":
		return agentLinuxTargets[2], nil
	default:
		return agentLinuxTarget{}, fmt.Errorf("unsupported Linux architecture %q", strings.TrimSpace(machine))
	}
}

func selectAgentRelease(rawVersion string, target agentLinuxTarget, manifest update.Manifest) (selectedAgentRelease, error) {
	versionTag, ok := version.NormalizeSemVer(rawVersion)
	if !ok {
		return selectedAgentRelease{}, fmt.Errorf("invalid agent release version %q", rawVersion)
	}
	entries, ok := manifest.AgentBinaries[versionTag]
	if !ok {
		return selectedAgentRelease{}, fmt.Errorf("agent release %s is absent from manifest", versionTag)
	}
	info, ok := entries[target.name]
	if !ok {
		return selectedAgentRelease{}, fmt.Errorf("agent release %s is missing target %s", versionTag, target.name)
	}
	versionNoV := strings.TrimPrefix(versionTag, "v")
	expectedURL := fmt.Sprintf("https://github.com/%s/releases/download/%s/fleet-agent_%s_linux_%s.tar.gz", agentGitHubRepo, versionTag, versionNoV, target.arch)
	if info.URL != expectedURL || info.Signature != expectedURL+".minisig" {
		return selectedAgentRelease{}, fmt.Errorf("agent %s URLs are not bound to product, version, and target", target.name)
	}
	expectedDigest, err := hex.DecodeString(info.SHA256)
	if err != nil || len(expectedDigest) != sha256.Size || info.Size <= 0 || info.Size > maxAgentArtifactBytes {
		return selectedAgentRelease{}, fmt.Errorf("agent %s has invalid checksum or size metadata", target.name)
	}
	return selectedAgentRelease{versionTag: versionTag, target: target, info: info}, nil
}

func verifySelectedAgentRelease(selected selectedAgentRelease, archive, signature []byte, publicKeyText string) ([]byte, error) {
	if int64(len(archive)) != selected.info.Size {
		return nil, fmt.Errorf("agent %s size mismatch", selected.target.name)
	}
	var publicKey minisign.PublicKey
	if err := publicKey.UnmarshalText([]byte(strings.TrimSpace(publicKeyText))); err != nil {
		return nil, fmt.Errorf("parse embedded minisign key: %w", err)
	}
	if !minisign.Verify(publicKey, archive, signature) {
		return nil, fmt.Errorf("agent %s signature verification failed", selected.target.name)
	}
	var parsed minisign.Signature
	if err := parsed.UnmarshalText(signature); err != nil {
		return nil, fmt.Errorf("parse %s signature: %w", selected.target.name, err)
	}
	expectedComment := fmt.Sprintf("cenvero-fleet fleet-agent %s %s", selected.versionTag, selected.target.name)
	if parsed.TrustedComment != expectedComment {
		return nil, fmt.Errorf("agent %s signature binding mismatch", selected.target.name)
	}
	digest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), selected.info.SHA256) {
		return nil, fmt.Errorf("agent %s checksum mismatch", selected.target.name)
	}
	binary, err := extractVerifiedAgentBinary(archive)
	if err != nil {
		return nil, fmt.Errorf("extract %s: %w", selected.target.name, err)
	}
	return binary, nil
}

// verifyMinisignDigest reports whether sig, a prehashed minisign signature,
// signs the BLAKE2b-512 digest and its trusted comment with publicKey. It is
// the check minisign.Verify makes once it has hashed the message, so a file
// can be verified from its digest alone.
func verifyMinisignDigest(publicKey minisign.PublicKey, digest []byte, sig minisign.Signature) bool {
	if sig.Algorithm != minisign.HashEdDSA || sig.KeyID != publicKey.ID() || len(digest) != blake2b.Size {
		return false
	}
	// A minisign public key is the algorithm (2 bytes), the key ID (8 bytes)
	// and the Ed25519 key.
	raw, err := base64.StdEncoding.DecodeString(publicKey.String())
	if err != nil || len(raw) != 10+ed25519.PublicKeySize {
		return false
	}
	key := ed25519.PublicKey(raw[10:])
	if !ed25519.Verify(key, digest, sig.Signature[:]) {
		return false
	}
	signed := make([]byte, 0, len(sig.Signature)+len(sig.TrustedComment))
	signed = append(signed, sig.Signature[:]...)
	signed = append(signed, sig.TrustedComment...)
	return ed25519.Verify(key, signed, sig.CommentSignature[:])
}

// agentArchiveDigests is what the target reports about the archive it
// downloaded.
type agentArchiveDigests struct {
	size       int64
	blake2b512 []byte
	sha256     []byte
}

// verifySelectedAgentReleaseDigests is verifySelectedAgentRelease for an
// archive that stays on the target: it checks the target's report of the
// archive against the release signature and manifest entry.
func verifySelectedAgentReleaseDigests(selected selectedAgentRelease, digests agentArchiveDigests, signature []byte, publicKeyText string) error {
	if digests.size != selected.info.Size {
		return fmt.Errorf("agent %s size mismatch", selected.target.name)
	}
	var publicKey minisign.PublicKey
	if err := publicKey.UnmarshalText([]byte(strings.TrimSpace(publicKeyText))); err != nil {
		return fmt.Errorf("parse embedded minisign key: %w", err)
	}
	var parsed minisign.Signature
	if err := parsed.UnmarshalText(signature); err != nil {
		return fmt.Errorf("agent %s signature verification failed", selected.target.name)
	}
	if parsed.Algorithm != minisign.HashEdDSA {
		// A legacy signature signs the whole archive; only the archive verifies it.
		return fmt.Errorf("agent %s signature does not sign a digest", selected.target.name)
	}
	if !verifyMinisignDigest(publicKey, digests.blake2b512, parsed) {
		return fmt.Errorf("agent %s signature verification failed", selected.target.name)
	}
	expectedComment := fmt.Sprintf("cenvero-fleet fleet-agent %s %s", selected.versionTag, selected.target.name)
	if parsed.TrustedComment != expectedComment {
		return fmt.Errorf("agent %s signature binding mismatch", selected.target.name)
	}
	if !strings.EqualFold(hex.EncodeToString(digests.sha256), selected.info.SHA256) {
		return fmt.Errorf("agent %s checksum mismatch", selected.target.name)
	}
	return nil
}

func fetchVerifiedAgentReleaseWithKey(ctx context.Context, runner remoteOutputRunner, spec BootstrapAgentRelease, publicKeyText string) ([]byte, error) {
	selected, err := selectTargetAgentRelease(ctx, runner, spec)
	if err != nil {
		return nil, err
	}
	archive, err := runner.Output(ctx, buildRemoteAgentDownloadCommand(selected.info.URL, selected.info.Size), selected.info.Size)
	if err != nil {
		return nil, fmt.Errorf("target download %s: %w", selected.target.name, err)
	}
	signature, err := runner.Output(ctx, buildRemoteAgentDownloadCommand(selected.info.Signature, maxAgentSignatureBytes), maxAgentSignatureBytes)
	if err != nil {
		return nil, fmt.Errorf("target download %s signature: %w", selected.target.name, err)
	}
	return verifySelectedAgentRelease(selected, archive, signature, publicKeyText)
}

// selectTargetAgentRelease detects the target's architecture and picks the
// release for it from the manifest the target downloads.
func selectTargetAgentRelease(ctx context.Context, runner remoteOutputRunner, spec BootstrapAgentRelease) (selectedAgentRelease, error) {
	uname, err := runner.Output(ctx, "uname -s && uname -m", 4<<10)
	if err != nil {
		return selectedAgentRelease{}, fmt.Errorf("detect target architecture: %w", err)
	}
	fields := strings.Fields(string(uname))
	if len(fields) != 2 {
		return selectedAgentRelease{}, fmt.Errorf("unexpected uname output %q", strings.TrimSpace(string(uname)))
	}
	target, err := agentTargetForUname(fields[0], fields[1])
	if err != nil {
		return selectedAgentRelease{}, err
	}

	manifestData, err := runner.Output(ctx, buildRemoteAgentDownloadCommand(update.DefaultManifestURL, maxRemoteAgentManifestBytes), maxRemoteAgentManifestBytes)
	if err != nil {
		return selectedAgentRelease{}, fmt.Errorf("target download release manifest: %w", err)
	}
	var manifest update.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return selectedAgentRelease{}, fmt.Errorf("decode target-downloaded release manifest: %w", err)
	}
	return selectAgentRelease(spec.Version, target, manifest)
}

// acquireVerifiedAgentRelease has the target download the release matching
// spec and verifies it. With a private stagingDir the archive is downloaded
// into it and stays there: the target reports the archive's size and digests,
// the controller checks them against the release signature and manifest, and
// the target then checks the digests again and unpacks the binary to
// spec.DestinationPath itself, so staged is true and nothing larger than the
// manifest and signature crosses the connection. Otherwise (the target cannot
// hash or unpack the archive, or its digests do not verify) the archive is
// read back, verified whole and its binary returned for the caller to upload
// to spec.DestinationPath, as it always is without a stagingDir. Either way a
// release is accepted only if it would be when verified whole.
func acquireVerifiedAgentRelease(ctx context.Context, runner remoteOutputRunner, spec BootstrapAgentRelease, stagingDir, publicKeyText string) (binary []byte, staged bool, err error) {
	if stagingDir == "" {
		binary, err := fetchVerifiedAgentReleaseWithKey(ctx, runner, spec, publicKeyText)
		return binary, false, err
	}
	destination := strings.TrimSpace(spec.DestinationPath)
	archivePath := agentReleaseArchivePath(stagingDir)
	selected, err := selectTargetAgentRelease(ctx, runner, spec)
	if err != nil {
		return nil, false, err
	}

	report, err := runner.Output(ctx, buildRemoteAgentFetchCommand(selected.info.URL, selected.info.Size, archivePath), 4<<10)
	if err != nil {
		return nil, false, fmt.Errorf("target download %s: %w", selected.target.name, err)
	}
	signature, err := runner.Output(ctx, buildRemoteAgentDownloadCommand(selected.info.Signature, maxAgentSignatureBytes), maxAgentSignatureBytes)
	if err != nil {
		return nil, false, fmt.Errorf("target download %s signature: %w", selected.target.name, err)
	}
	digests, hashed, err := parseAgentArchiveReport(report)
	if err != nil {
		return nil, false, fmt.Errorf("target report on %s: %w", selected.target.name, err)
	}
	// Digests that do not verify are not trusted to mean a bad release (the
	// target's tool may be at fault, or the signature may not sign a digest):
	// the archive itself is read back and decides.
	if hashed && verifySelectedAgentReleaseDigests(selected, digests, signature, publicKeyText) == nil {
		out, err := runner.Output(ctx, buildRemoteAgentStageCommand(archivePath, destination, digests), 4<<10)
		if err != nil {
			return nil, false, fmt.Errorf("unpack verified %s on the target: %w", selected.target.name, err)
		}
		switch strings.TrimSpace(string(out)) {
		case agentStagedMarker:
			return nil, true, nil
		case agentUnpackUnavailableMarker:
		default:
			return nil, false, fmt.Errorf("unpack verified %s on the target: unexpected output %q", selected.target.name, bytes.TrimSpace(out))
		}
	}

	archive, err := runner.Output(ctx, "cat "+shellQuote(archivePath), selected.info.Size)
	if err != nil {
		return nil, false, fmt.Errorf("read back %s from the target: %w", selected.target.name, err)
	}
	binary, err = verifySelectedAgentRelease(selected, archive, signature, publicKeyText)
	return binary, false, err
}

// agentReleaseArchivePath is where the target keeps the release archive it
// downloads into the private staging directory.
func agentReleaseArchivePath(stagingDir string) string {
	return TargetPathPOSIX.Join(stagingDir, "agent-release.tar.gz")
}

const (
	agentDigestsMarker           = "fleet-agent-archive"
	agentDigestsUnavailable      = "fleet-agent-archive-unhashed"
	agentStagedMarker            = "fleet-agent-unpacked"
	agentUnpackUnavailableMarker = "fleet-agent-not-unpacked"
)

// parseAgentArchiveReport reads buildRemoteAgentFetchCommand's output, a single
// line: the archive's size and digests, or that the target has no working tool
// to compute them (hashed is false).
func parseAgentArchiveReport(report []byte) (digests agentArchiveDigests, hashed bool, err error) {
	fields := strings.Fields(string(report))
	if len(fields) == 1 && fields[0] == agentDigestsUnavailable {
		return agentArchiveDigests{}, false, nil
	}
	if len(fields) != 4 || fields[0] != agentDigestsMarker {
		return agentArchiveDigests{}, false, fmt.Errorf("unexpected archive report %q", bytes.TrimSpace(report))
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || size <= 0 {
		return agentArchiveDigests{}, false, fmt.Errorf("invalid archive size %q", fields[1])
	}
	blake, err := hex.DecodeString(fields[2])
	if err != nil || len(blake) != blake2b.Size {
		return agentArchiveDigests{}, false, fmt.Errorf("invalid BLAKE2b-512 digest %q", fields[2])
	}
	sum, err := hex.DecodeString(fields[3])
	if err != nil || len(sum) != sha256.Size {
		return agentArchiveDigests{}, false, fmt.Errorf("invalid SHA-256 digest %q", fields[3])
	}
	return agentArchiveDigests{size: size, blake2b512: blake, sha256: sum}, true, nil
}

// remoteDigestFunctions defines fleet_blake2b and fleet_sha256 for the
// target's shell. Each prints a file's digest in lowercase hex, computed with
// the first of the usual tools that is installed and gives the right digest
// of empty input, and fails when none does.
func remoteDigestFunctions() []string {
	emptyBlake := blake2b.Sum512(nil)
	emptySHA := sha256.Sum256(nil)
	const python = `'import hashlib,sys; print(getattr(hashlib, sys.argv[1])(open(sys.argv[2], "rb").read()).hexdigest())'`
	return []string{
		`fleet_digest_run() {`,
		`  fd_out=$("$@" 2>/dev/null) || return 1`,
		`  set -- $fd_out`,
		`  [ "$#" -ge 1 ] || return 1`,
		`  fd_hex=$(printf '%s' "$1" | tr 'ABCDEF' 'abcdef')`,
		`  case $fd_hex in ''|*[!0-9a-f]*) return 1 ;; esac`,
		`  printf '%s\n' "$fd_hex"`,
		`}`,
		`fleet_digest_try() {`,
		`  fd_empty=$1; fd_file=$2; shift 2`,
		`  fd_got=$(fleet_digest_run "$@" /dev/null) || return 1`,
		`  [ "$fd_got" = "$fd_empty" ] || return 1`,
		`  fd_got=$(fleet_digest_run "$@" "$fd_file") || return 1`,
		`  [ "${#fd_got}" -eq "${#fd_empty}" ] || return 1`,
		`  printf '%s\n' "$fd_got"`,
		`}`,
		`fleet_blake2b() {`,
		`  E=` + hex.EncodeToString(emptyBlake[:]),
		`  fleet_digest_try "$E" "$1" b2sum && return 0`,
		`  fleet_digest_try "$E" "$1" openssl dgst -blake2b512 -r && return 0`,
		`  fleet_digest_try "$E" "$1" python3 -c ` + python + ` blake2b && return 0`,
		`  return 1`,
		`}`,
		`fleet_sha256() {`,
		`  E=` + hex.EncodeToString(emptySHA[:]),
		`  fleet_digest_try "$E" "$1" sha256sum && return 0`,
		`  fleet_digest_try "$E" "$1" shasum -a 256 && return 0`,
		`  fleet_digest_try "$E" "$1" openssl dgst -sha256 -r && return 0`,
		`  fleet_digest_try "$E" "$1" python3 -c ` + python + ` sha256 && return 0`,
		`  return 1`,
		`}`,
	}
}

// buildRemoteAgentFetchCommand downloads the archive at rawURL to archivePath
// on the target, like buildRemoteAgentDownloadCommand, and prints its size and
// digests instead of its contents.
func buildRemoteAgentFetchCommand(rawURL string, maxBytes int64, archivePath string) string {
	lines := remoteAgentDownloadLines(rawURL, maxBytes, remoteAgentDownloadTimeoutSeconds,
		"ARCHIVE="+shellQuote(archivePath), `TMP=$(mktemp "$ARCHIVE.XXXXXX")`)
	lines = append(lines, `mv -f "$TMP" "$ARCHIVE"`, "set -f")
	lines = append(lines, remoteDigestFunctions()...)
	lines = append(lines,
		`SIZE=$(wc -c < "$ARCHIVE")`,
		`if BLAKE2B=$(fleet_blake2b "$ARCHIVE") && SHA256=$(fleet_sha256 "$ARCHIVE"); then`,
		`  printf '`+agentDigestsMarker+` %s %s %s\n' $SIZE "$BLAKE2B" "$SHA256"`,
		`else`,
		`  echo `+agentDigestsUnavailable,
		`fi`,
	)
	return strings.Join(lines, "\n")
}

// buildRemoteAgentStageCommand checks that the archive on the target still has
// the digests the controller verified, then unpacks its fleet-agent to
// destination. It prints agentUnpackUnavailableMarker, leaving the archive in
// place, when the target cannot unpack it.
func buildRemoteAgentStageCommand(archivePath, destination string, digests agentArchiveDigests) string {
	lines := []string{
		"set -eu",
		"set -f",
		"umask 077",
		"ARCHIVE=" + shellQuote(archivePath),
		"DEST=" + shellQuote(destination),
		"PART=" + shellQuote(remoteUploadPartialPath(destination)),
		"WANT_BLAKE2B=" + hex.EncodeToString(digests.blake2b512),
		"WANT_SHA256=" + hex.EncodeToString(digests.sha256),
		`trap 'rm -f "$PART"' 0`,
		`trap 'exit 1' 1 2 13 15`,
	}
	lines = append(lines, remoteDigestFunctions()...)
	lines = append(lines,
		`if ! GOT_BLAKE2B=$(fleet_blake2b "$ARCHIVE") || ! GOT_SHA256=$(fleet_sha256 "$ARCHIVE"); then`,
		`  echo "cannot hash the downloaded agent archive again" >&2; exit 1`,
		`fi`,
		`if [ "$GOT_BLAKE2B" != "$WANT_BLAKE2B" ] || [ "$GOT_SHA256" != "$WANT_SHA256" ]; then`,
		`  echo "the downloaded agent archive changed after it was verified" >&2; exit 1`,
		`fi`,
		`rm -f "$PART"`,
		`if tar -xzOf "$ARCHIVE" fleet-agent > "$PART" 2>/dev/null && [ -s "$PART" ]; then`,
		`  chmod 0700 "$PART"`,
		`  mv -f "$PART" "$DEST"`,
		`  rm -f "$ARCHIVE"`,
		`  echo `+agentStagedMarker,
		`else`,
		`  echo `+agentUnpackUnavailableMarker,
		`fi`,
	)
	return strings.Join(lines, "\n")
}

func buildRemoteAgentDownloadCommand(rawURL string, maxBytes int64) string {
	return buildRemoteAgentDownloadCommandWithTimeout(rawURL, maxBytes, remoteAgentDownloadTimeoutSeconds)
}

func buildRemoteAgentDownloadCommandWithTimeout(rawURL string, maxBytes int64, timeoutSeconds int) string {
	return strings.Join(append(remoteAgentDownloadLines(rawURL, maxBytes, timeoutSeconds, "TMP=$(mktemp)"), `cat "$TMP"`), "\n")
}

// remoteAgentDownloadLines downloads rawURL on the target into the temporary
// file $TMP, which the lines in setup create, with wget or else curl, bounded
// to maxBytes and timeoutSeconds. $TMP is removed when the shell exits.
func remoteAgentDownloadLines(rawURL string, maxBytes int64, timeoutSeconds int, setup ...string) []string {
	if maxBytes <= 0 {
		maxBytes = 1
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 1
	}
	fileBlocks := maxBytes / 512
	if maxBytes%512 != 0 {
		fileBlocks++
	}
	lines := []string{
		"set -eu",
		"umask 077",
		"URL=" + shellQuote(rawURL),
		"MAX_BLOCKS=" + strconv.FormatInt(fileBlocks, 10),
		"DEADLINE_SECONDS=" + strconv.Itoa(timeoutSeconds),
	}
	lines = append(lines, setup...)
	return append(lines,
		"DOWNLOAD_PID=",
		"WATCHDOG_PID=",
		`cleanup() {`,
		`  status=$?`,
		`  trap - 0 1 2 15`,
		`  if [ -n "$DOWNLOAD_PID" ]; then kill "$DOWNLOAD_PID" 2>/dev/null || true; wait "$DOWNLOAD_PID" 2>/dev/null || true; fi`,
		// SIGKILL: a TERM that reaches the watchdog before its shell has reset
		// the inherited trap is lost, and waiting for it would take the
		// whole deadline.
		`  if [ -n "$WATCHDOG_PID" ]; then kill -9 "$WATCHDOG_PID" 2>/dev/null || true; wait "$WATCHDOG_PID" 2>/dev/null || true; fi`,
		`  rm -f "$TMP"`,
		`  exit "$status"`,
		`}`,
		`trap cleanup 0`,
		`trap 'exit 124' 1 2 15`,
		`ulimit -f "$MAX_BLOCKS"`,
		// The watchdog must not hold the command's output open: its sleep
		// outlives the kill below and would delay the end of the output.
		`( remaining=$DEADLINE_SECONDS; while [ "$remaining" -gt 0 ]; do sleep 1; remaining=$((remaining - 1)); done; kill -TERM "$$" 2>/dev/null || true ) >/dev/null 2>&1 &`,
		`WATCHDOG_PID=$!`,
		`run_download() {`,
		`  "$@" &`,
		`  DOWNLOAD_PID=$!`,
		`  status=0`,
		`  wait "$DOWNLOAD_PID" || status=$?`,
		`  DOWNLOAD_PID=`,
		`  return "$status"`,
		`}`,
		"DOWNLOADED=0",
		`if command -v wget >/dev/null 2>&1; then`,
		`  WGET_ATTEMPT=1`,
		`  while [ "$WGET_ATTEMPT" -le 3 ]; do`,
		`    if run_download wget -q -T 30 -O "$TMP" "$URL"; then DOWNLOADED=1; break; fi`,
		`    WGET_ATTEMPT=$((WGET_ATTEMPT + 1))`,
		`  done`,
		"fi",
		`if [ "$DOWNLOADED" -ne 1 ] && command -v curl >/dev/null 2>&1; then`,
		`  if run_download curl -fL --silent --show-error --retry 3 --retry-delay 1 --connect-timeout 15 --max-time "$DEADLINE_SECONDS" --proto '=https' -o "$TMP" "$URL"; then DOWNLOADED=1; fi`,
		"fi",
		`if [ "$DOWNLOADED" -ne 1 ]; then echo "wget and curl could not download the Fleet release payload" >&2; exit 1; fi`,
	)
}

func extractVerifiedAgentBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "fleet-agent" {
			continue
		}
		binary, err := io.ReadAll(io.LimitReader(tr, maxAgentArtifactBytes+1))
		if err != nil {
			return nil, err
		}
		if len(binary) == 0 || len(binary) > maxAgentArtifactBytes {
			return nil, fmt.Errorf("invalid extracted fleet-agent size")
		}
		return binary, nil
	}
	return nil, fmt.Errorf("fleet-agent binary not found in archive")
}

func buildVerifiedBinaryInstallScript(server ServerRecord, sudo, serviceName, tempBinPath, tempServicePath, tempKeysPath, tempScriptPath string) string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
		"BIN_DIR=" + shellQuote(defaultAgentBinDir),
		"BIN_PATH=" + shellQuote(defaultAgentBinaryPath),
		"TEMP_BIN=" + shellQuote(tempBinPath),
		"TEMP_SERVICE=" + shellQuote(tempServicePath),
		"TEMP_KEYS=" + shellQuote(tempKeysPath),
		"TEMP_SCRIPT=" + shellQuote(tempScriptPath),
		`trap 'rm -f "$TEMP_BIN" "$TEMP_SERVICE" "$TEMP_KEYS" "$TEMP_SCRIPT"' 0 1 2 15`,
	}
	lines = append(lines, buildAgentSetupLines(sudo, serviceName, `"$TEMP_BIN"`, tempServicePath, tempKeysPath)...)
	lines = append(lines,
		"echo \""+version.ProductName+" agent installed on "+server.Name+" (target-downloaded, controller-verified artifact)\"",
	)
	return strings.Join(lines, "\n") + "\n"
}

func buildLocalBinaryInstallScript(server ServerRecord, sudo, serviceName, tempBinPath, tempServicePath, tempKeysPath, tempScriptPath string) string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
		"BIN_DIR=" + shellQuote(defaultAgentBinDir),
		"BIN_PATH=" + shellQuote(defaultAgentBinaryPath),
		"",
	}
	lines = append(lines, buildAgentSetupLines(sudo, serviceName, shellQuote(tempBinPath), tempServicePath, tempKeysPath)...)
	lines = append(lines,
		"rm -f "+shellQuote(tempServicePath)+" "+shellQuote(tempKeysPath)+" "+shellQuote(tempBinPath)+" "+shellQuote(tempScriptPath),
		"echo \""+version.ProductName+" agent installed on "+server.Name+" (dev build)\"",
	)
	return strings.Join(lines, "\n") + "\n"
}

func buildAgentSetupLines(sudo, serviceName, binarySource, tempServicePath, tempKeysPath string) []string {
	return []string{
		sudo + "mkdir -p \"$BIN_DIR\" \"$STATE_DIR\" \"$CONFIG_DIR\"",
		sudo + "install -m 0755 " + binarySource + " \"$BIN_PATH\"",
		sudo + "install -m 0644 " + shellQuote(tempServicePath) + " /etc/systemd/system/" + shellQuote(serviceName) + ".service",
		sudo + "install -m 0600 " + shellQuote(tempKeysPath) + " \"$STATE_DIR/authorized_keys\"",
		sudo + "systemctl daemon-reload",
		sudo + "systemctl enable --now " + shellQuote(serviceName) + ".service",
	}
}

func buildAgentTeardownScript(serviceName, sudo, selfPath string) string {
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"",
		sudo + "systemctl disable --now \"$SERVICE_NAME\".service 2>/dev/null || true",
		sudo + "rm -f /etc/systemd/system/\"$SERVICE_NAME\".service",
		sudo + "systemctl daemon-reload",
		sudo + "rm -rf " + shellQuote(defaultAgentBinDir),
		sudo + "rm -rf " + shellQuote(defaultStateDir) + " " + shellQuote(defaultConfigDir),
		"rm -f " + shellQuote(selfPath),
		"echo \"" + version.ProductName + " agent removed\"",
	}
	return strings.Join(lines, "\n") + "\n"
}
