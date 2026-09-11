// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/version"
	"golang.org/x/crypto/ssh"
)

const (
	defaultAgentUser       = "cenvero-agent"
	defaultServiceName     = "cenvero-fleet-agent"
	defaultAgentBinDir     = "/opt/cenvero-fleet"
	defaultAgentBinaryPath = "/opt/cenvero-fleet/fleet-agent"
	defaultStateDir        = "/var/lib/cenvero-fleet-agent"
	defaultConfigDir       = "/etc/cenvero-fleet-agent"
	defaultDirectListen    = "0.0.0.0:2222"
)

func defaultDirectAuthorizedKeysPath() string {
	return TargetPathPOSIX.Join(defaultStateDir, "authorized_keys")
}

type BootstrapExecutor interface {
	Bootstrap(context.Context, BootstrapRequest) error
}

// BootstrapAgentRelease asks the SSH executor to have the target download the
// exact release matching Version, verify it on the controller, and stage the
// extracted binary at DestinationPath before RunCommand executes.
type BootstrapAgentRelease struct {
	Version         string
	DestinationPath string
}

type BootstrapRequest struct {
	Address          string
	Port             int
	User             string
	PrivateKeyPath   string
	Password         string
	KnownHostsPath   string
	AcceptNewHostKey bool
	// HostKeyChangedPrompt, when non-nil, makes the bootstrap host-key check
	// INTERACTIVE: a never-seen host is still pinned (TOFU), but a host whose key
	// no longer matches its pin invokes this prompt — returning true re-pins and
	// continues, false refuses. It takes precedence over AcceptNewHostKey. When
	// nil, the static AcceptNewHostKey behavior applies (refuse a changed key
	// unless the flag forces a re-pin).
	HostKeyChangedPrompt func(host, oldFP, newFP string) bool
	AgentRelease         *BootstrapAgentRelease
	Uploads              []BootstrapUpload
	RunCommand           string
}

type BootstrapUpload struct {
	Path    string
	Mode    os.FileMode
	Content []byte
}

func (a *App) BootstrapServer(name string, opts BootstrapOptions) (BootstrapResult, error) {
	server, err := a.GetServer(name)
	if err != nil {
		return BootstrapResult{}, err
	}
	if server.Mode == transport.ModeReverse && server.EnrollSecret == "" {
		server.EnrollSecret, err = GenerateEnrollSecret()
		if err != nil {
			return BootstrapResult{}, err
		}
		if err := a.SaveServer(server); err != nil {
			return BootstrapResult{}, fmt.Errorf("store reverse enrollment credential: %w", err)
		}
	}

	resolved, err := a.resolveBootstrapConfig(server, opts)
	if err != nil {
		return BootstrapResult{}, err
	}

	binaryData, err := os.ReadFile(resolved.agentBinaryPath)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("read agent binary %s: %w", resolved.agentBinaryPath, err)
	}

	serviceUnit, err := buildAgentServiceUnit(server, resolved)
	if err != nil {
		return BootstrapResult{}, err
	}

	authorizedKeys := []byte(nil)
	if server.Mode == transport.ModeDirect {
		authorizedKeys, err = os.ReadFile(filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey+".pub"))
		if err != nil {
			return BootstrapResult{}, fmt.Errorf("read controller public key: %w", err)
		}
	}

	script := buildBootstrapScript(server, resolved, len(authorizedKeys) > 0)
	result := BootstrapResult{
		Server:            server.Name,
		LoginAddress:      net.JoinHostPort(server.Address, strconv.Itoa(resolved.loginPort)),
		LoginUser:         resolved.loginUser,
		LoginPort:         resolved.loginPort,
		Mode:              server.Mode,
		AgentBinaryPath:   resolved.agentBinaryPath,
		AgentListenAddr:   resolved.agentListenAddr,
		ControllerAddress: resolved.controllerAddress,
		ServiceName:       resolved.serviceName,
		ServiceUnit:       serviceUnit,
	}
	if opts.PrintScript {
		result.Script = script
		return result, nil
	}

	executor := a.BootstrapExecutor
	if executor == nil {
		executor = sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	}

	request := BootstrapRequest{
		Address:              server.Address,
		Port:                 resolved.loginPort,
		User:                 resolved.loginUser,
		PrivateKeyPath:       resolved.loginKeyPath,
		KnownHostsPath:       filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey:     resolved.acceptNewHostKey,
		HostKeyChangedPrompt: a.HostKeyChangedPrompt,
		Uploads: []BootstrapUpload{
			{Path: resolved.tempBinaryPath, Mode: 0o700, Content: binaryData},
			{Path: resolved.tempUnitPath, Mode: 0o600, Content: []byte(serviceUnit)},
		},
		RunCommand: "/bin/sh " + shellQuote(resolved.tempScriptPath),
	}
	if len(authorizedKeys) > 0 {
		request.Uploads = append(request.Uploads, BootstrapUpload{
			Path:    resolved.tempAuthorizedKeysPath,
			Mode:    0o600, // keep temp file owner-only until the install script moves it
			Content: authorizedKeys,
		})
	}
	if server.Mode == transport.ModeReverse {
		request.Uploads = append(request.Uploads, BootstrapUpload{
			Path:    resolved.tempEnrollTokenPath,
			Mode:    0o600,
			Content: []byte(server.EnrollSecret + "\n"),
		})
	}
	request.Uploads = append(request.Uploads, BootstrapUpload{
		Path:    resolved.tempScriptPath,
		Mode:    0o700,
		Content: []byte(script),
	})

	if err := executor.Bootstrap(context.Background(), request); err != nil {
		return BootstrapResult{}, err
	}

	server.User = "root"
	if server.Mode == transport.ModeDirect {
		server.Port = resolved.agentPort
	}
	server.Agent = AgentInstall{
		Managed:     true,
		Status:      "managed",
		BinaryPath:  defaultAgentBinaryPath,
		ServiceName: resolved.serviceName,
		LoginUser:   resolved.loginUser,
		LoginPort:   resolved.loginPort,
		LoginKey:    resolved.loginKeyPath,
		UseSudo:     resolved.useSudo,
		UpdatedAt:   time.Now().UTC(),
	}
	// Seed per-server file-transfer defaults from the global runtime defaults on
	// first connection. They start as a copy so the operator can tune any server
	// later without affecting the rest of the fleet.
	if server.FileTransfer == (FileTransferDefaults{}) {
		server.FileTransfer = a.Config.Runtime.FileTransfer
	}
	if err := a.SaveServer(server); err != nil {
		return BootstrapResult{}, err
	}
	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "server.bootstrap",
		Target:   server.Name,
		Operator: a.operator(),
		Details:  fmt.Sprintf("mode=%s login=%s:%d service=%s", server.Mode, resolved.loginUser, resolved.loginPort, resolved.serviceName),
	}); err != nil {
		return BootstrapResult{}, err
	}

	result.Executed = true
	return result, nil
}

type resolvedBootstrapConfig struct {
	loginUser              string
	loginPort              int
	loginKeyPath           string
	agentBinaryPath        string
	agentListenAddr        string
	agentPort              int
	controllerAddress      string
	controllerFingerprint  string
	serviceName            string
	useSudo                bool
	acceptNewHostKey       bool
	tempBinaryPath         string
	tempUnitPath           string
	tempScriptPath         string
	tempAuthorizedKeysPath string
	tempEnrollTokenPath    string
	enrollTokenPath        string
}

func (a *App) resolveBootstrapConfig(server ServerRecord, opts BootstrapOptions) (resolvedBootstrapConfig, error) {
	loginUser := strings.TrimSpace(opts.LoginUser)
	if loginUser == "" {
		loginUser = strings.TrimSpace(server.Agent.LoginUser)
	}
	if loginUser == "" {
		if opts.PrintScript {
			loginUser = "root"
		} else {
			return resolvedBootstrapConfig{}, fmt.Errorf("bootstrap login user is required")
		}
	}

	loginPort := opts.LoginPort
	if loginPort == 0 {
		loginPort = server.Agent.LoginPort
		if loginPort == 0 {
			loginPort = 22
		}
	}
	if loginPort < 1 || loginPort > 65535 {
		return resolvedBootstrapConfig{}, fmt.Errorf("invalid bootstrap login port %d: must be 1-65535", loginPort)
	}

	loginKeyPath := strings.TrimSpace(opts.LoginKeyPath)
	if loginKeyPath == "" {
		loginKeyPath = strings.TrimSpace(server.Agent.LoginKey)
	}
	if loginKeyPath == "" {
		loginKeyPath = a.serverPrivateKeyPath(server)
	}

	agentBinaryPath, err := resolveAgentBinaryPath(opts.AgentBinaryPath)
	if err != nil {
		return resolvedBootstrapConfig{}, err
	}

	serviceName := strings.TrimSpace(opts.ServiceName)
	if serviceName == "" {
		serviceName = defaultServiceName
	}
	if err := validateBootstrapServiceName(serviceName); err != nil {
		return resolvedBootstrapConfig{}, err
	}

	agentListenAddr := strings.TrimSpace(opts.AgentListenAddr)
	agentPort := server.Port
	if agentPort == 0 {
		if a.Config.Runtime.DefaultAgentPort > 0 {
			agentPort = a.Config.Runtime.DefaultAgentPort
		} else {
			agentPort = 2222
		}
	}
	if agentPort < 1 || agentPort > 65535 {
		return resolvedBootstrapConfig{}, fmt.Errorf("invalid agent port %d: must be 1-65535", agentPort)
	}
	if agentListenAddr == "" && server.Mode == transport.ModeDirect {
		agentListenAddr = fmt.Sprintf("0.0.0.0:%d", agentPort)
	}
	if server.Mode == transport.ModeDirect {
		port, err := portFromAddress(agentListenAddr)
		if err != nil {
			return resolvedBootstrapConfig{}, err
		}
		agentPort = port
	}

	controllerAddress := strings.TrimSpace(opts.ControllerAddress)
	controllerFingerprint := ""
	if server.Mode == transport.ModeReverse {
		if controllerAddress == "" {
			controllerAddress = defaultControllerBootstrapAddress(a.Config.Runtime.ListenAddress)
		}
		if controllerAddress == "" {
			return resolvedBootstrapConfig{}, fmt.Errorf("controller address is required for reverse bootstrap; pass --controller with a reachable address")
		}
		signer, err := fleetcrypto.LoadPrivateKeySigner(a.controllerPrivateKeyPath(), nil)
		if err != nil {
			return resolvedBootstrapConfig{}, fmt.Errorf("load controller host key for bootstrap verification: %w", err)
		}
		controllerFingerprint = ssh.FingerprintSHA256(signer.PublicKey())
		if server.EnrollSecret == "" {
			return resolvedBootstrapConfig{}, fmt.Errorf("reverse bootstrap requires a pending enrollment credential")
		}
	}

	entropy := opts.entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	token, err := randomBootstrapToken(entropy)
	if err != nil {
		return resolvedBootstrapConfig{}, fmt.Errorf("generate unpredictable bootstrap paths: %w", err)
	}
	return resolvedBootstrapConfig{
		loginUser:              loginUser,
		loginPort:              loginPort,
		loginKeyPath:           loginKeyPath,
		agentBinaryPath:        agentBinaryPath,
		agentListenAddr:        agentListenAddr,
		agentPort:              agentPort,
		controllerAddress:      controllerAddress,
		controllerFingerprint:  controllerFingerprint,
		serviceName:            serviceName,
		useSudo:                opts.UseSudo,
		acceptNewHostKey:       opts.AcceptNewHostKey,
		tempBinaryPath:         "/tmp/cenvero-" + token + ".bin",
		tempUnitPath:           "/tmp/cenvero-" + token + ".service",
		tempScriptPath:         "/tmp/cenvero-" + token + ".sh",
		tempAuthorizedKeysPath: "/tmp/cenvero-" + token + ".keys",
		tempEnrollTokenPath:    "/tmp/cenvero-" + token + ".enroll",
		enrollTokenPath:        TargetPathPOSIX.Join(defaultStateDir, "enroll.token"),
	}, nil
}

// validateBootstrapServiceName accepts a systemd unit basename, not a path or
// shell fragment. Bootstrap appends ".service" itself and interpolates the name
// into a managed Linux path, so separators and other punctuation are refused.
func validateBootstrapServiceName(name string) error {
	if len(name) == 0 || len(name) > 256 {
		return fmt.Errorf("invalid bootstrap service name %q: must be 1-256 characters", name)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid bootstrap service name %q: may not start with '-'", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("_@.:-", r) {
			continue
		}
		return fmt.Errorf("invalid bootstrap service name %q: contains unsupported character %q", name, r)
	}
	if strings.HasSuffix(name, ".service") {
		return fmt.Errorf("invalid bootstrap service name %q: omit the .service suffix", name)
	}
	return nil
}

// randomBootstrapToken returns a 16-character hex token for use in temp file
// paths during bootstrap. This prevents local users on the target server from
// pre-staging symlinks or scripts at predictable /tmp paths. Entropy failures
// are returned so bootstrap fails closed without terminating the controller.
func randomBootstrapToken(entropy io.Reader) (string, error) {
	var b [8]byte
	if _, err := io.ReadFull(entropy, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func resolveAgentBinaryPath(override string) (string, error) {
	candidates := make([]string, 0, 4)
	if strings.TrimSpace(override) != "" {
		candidates = append(candidates, strings.TrimSpace(override))
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "fleet-agent"))
	}
	candidates = append(candidates, filepath.Join("dist", "fleet-agent"))
	if path, err := exec.LookPath("fleet-agent"); err == nil {
		candidates = append(candidates, path)
	}

	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find a local fleet-agent binary; pass --agent-binary")
}

func portFromAddress(addr string) (int, error) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("parse agent listen address %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, fmt.Errorf("parse agent port %q: %w", portText, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid agent port %d: must be 1-65535", port)
	}
	return port, nil
}

func defaultControllerBootstrapAddress(listenAddress string) string {
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" || strings.EqualFold(host, "localhost") || strings.HasPrefix(host, "127.") {
		return ""
	}
	return net.JoinHostPort(host, port)
}

func buildAgentServiceUnit(server ServerRecord, cfg resolvedBootstrapConfig) (string, error) {
	var execStart string
	switch server.Mode {
	case transport.ModeDirect:
		execStart = fmt.Sprintf("%s serve --listen %s --host-key %s --authorized-keys %s",
			defaultAgentBinaryPath,
			shellQuote(cfg.agentListenAddr),
			shellQuote(TargetPathPOSIX.Join(defaultStateDir, "ssh_host_ed25519_key")),
			shellQuote(defaultDirectAuthorizedKeysPath()),
		)
	case transport.ModeReverse:
		execStart = fmt.Sprintf("%s reverse --controller %s --server-name %s --host-key %s --known-hosts %s --controller-fingerprint %s --enroll-token-file %s",
			defaultAgentBinaryPath,
			shellQuote(cfg.controllerAddress),
			shellQuote(server.Name),
			shellQuote(TargetPathPOSIX.Join(defaultStateDir, "ssh_host_ed25519_key")),
			shellQuote(TargetPathPOSIX.Join(defaultStateDir, "controller_known_hosts")),
			shellQuote(cfg.controllerFingerprint),
			shellQuote(cfg.enrollTokenPath),
		)
	default:
		return "", fmt.Errorf("bootstrap is not implemented for mode %q", server.Mode)
	}

	unit := strings.Join([]string{
		"[Unit]",
		"Description=" + version.ProductName + " agent",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + execStart,
		"Restart=always",
		"RestartSec=5",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
	return unit, nil
}

func buildBootstrapScript(server ServerRecord, cfg resolvedBootstrapConfig, includeAuthorizedKeys bool) string {
	sudo := ""
	if cfg.useSudo {
		sudo = "sudo "
	}

	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(cfg.serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
		"BIN_DIR=" + shellQuote(defaultAgentBinDir),
		"BIN_PATH=" + shellQuote(defaultAgentBinaryPath),
		"TEMP_BIN=" + shellQuote(cfg.tempBinaryPath),
		"TEMP_UNIT=" + shellQuote(cfg.tempUnitPath),
		"TEMP_SCRIPT=" + shellQuote(cfg.tempScriptPath),
		"TEMP_ENROLL=" + shellQuote(cfg.tempEnrollTokenPath),
		"",
		sudo + "mkdir -p \"$BIN_DIR\" \"$STATE_DIR\" \"$CONFIG_DIR\"",
		sudo + "install -m 0755 \"$TEMP_BIN\" \"$BIN_PATH\"",
		sudo + "install -m 0644 \"$TEMP_UNIT\" /etc/systemd/system/\"$SERVICE_NAME\".service",
	}
	if includeAuthorizedKeys {
		lines = append(lines, sudo+"install -m 0600 "+shellQuote(cfg.tempAuthorizedKeysPath)+" "+shellQuote(defaultDirectAuthorizedKeysPath()))
	}
	if server.Mode == transport.ModeReverse {
		lines = append(lines, sudo+"install -m 0600 \"$TEMP_ENROLL\" "+shellQuote(cfg.enrollTokenPath))
	}
	lines = append(lines,
		sudo+"systemctl daemon-reload",
		sudo+"systemctl enable --now \"$SERVICE_NAME\".service",
		"rm -f \"$TEMP_BIN\" \"$TEMP_UNIT\" \"$TEMP_SCRIPT\" \"$TEMP_ENROLL\"",
	)
	if includeAuthorizedKeys {
		lines = append(lines, "rm -f "+shellQuote(cfg.tempAuthorizedKeysPath))
	}
	lines = append(lines, "echo "+shellQuote(fmt.Sprintf("Bootstrapped %s agent on %s", version.ProductName, server.Name)))
	return strings.Join(lines, "\n") + "\n"
}

type sshBootstrapExecutor struct {
	networkDialContext func(context.Context, string, string) (net.Conn, error)
}

func (e sshBootstrapExecutor) connect(ctx context.Context, req BootstrapRequest, authMethods []ssh.AuthMethod) (*ssh.Client, error) {
	var hostKeyCallback ssh.HostKeyCallback
	var err error
	if req.HostKeyChangedPrompt != nil {
		hostKeyCallback, err = transport.NewInteractiveHostKeyCallback(req.KnownHostsPath, req.HostKeyChangedPrompt, &transport.HostKeyState{})
	} else {
		hostKeyCallback, err = transport.NewTOFUHostKeyCallback(req.KnownHostsPath, req.AcceptNewHostKey, &transport.HostKeyState{})
	}
	if err != nil {
		return nil, err
	}

	address := net.JoinHostPort(req.Address, strconv.Itoa(req.Port))
	config := &ssh.ClientConfig{
		Config: ssh.Config{Ciphers: transport.SupportedCiphers()},
		User:   req.User, Auth: authMethods, HostKeyCallback: hostKeyCallback,
		Timeout: 10 * time.Second,
	}
	var rawConn net.Conn
	if e.networkDialContext != nil {
		rawConn, err = e.networkDialContext(ctx, "tcp", address)
	} else {
		rawConn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return nil, fmt.Errorf("dial bootstrap target %s: %w", address, err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = rawConn.Close()
		}
	}()

	handshakeDeadline := time.Now().Add(config.Timeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if err := rawConn.SetDeadline(handshakeDeadline); err != nil {
		return nil, fmt.Errorf("set bootstrap ssh handshake deadline: %w", err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, address, config)
	if err != nil {
		return nil, fmt.Errorf("establish bootstrap ssh connection to %s: %w", address, err)
	}
	if err := rawConn.SetDeadline(time.Time{}); err != nil {
		_ = sshConn.Close()
		return nil, fmt.Errorf("clear bootstrap ssh handshake deadline: %w", err)
	}
	closeOnError = false
	return ssh.NewClient(sshConn, chans, reqs), nil
}
func (e sshBootstrapExecutor) Bootstrap(ctx context.Context, req BootstrapRequest) error {
	var authMethods []ssh.AuthMethod
	if req.Password != "" {
		authMethods = append(authMethods, ssh.Password(req.Password))
	}
	if req.PrivateKeyPath != "" {
		signer, err := fleetcrypto.LoadPrivateKeySigner(req.PrivateKeyPath, nil)
		if err != nil {
			return err
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if len(authMethods) == 0 {
		return fmt.Errorf("bootstrap: no authentication method provided (need --login-key or --login-password)")
	}

	client, err := e.connect(ctx, req, authMethods)
	if err != nil {
		return err
	}
	defer client.Close()

	bootstrapSucceeded := false
	stagedPaths := make([]string, 0, len(req.Uploads)+1)
	defer func() {
		if bootstrapSucceeded || len(stagedPaths) == 0 {
			return
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 2*time.Second)
		cleanupErr := cleanupRemoteStagingFiles(cleanupCtx, client, stagedPaths)
		cancelCleanup()
		if cleanupErr == nil {
			return
		}
		reconnectCtx, cancelReconnect := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelReconnect()
		cleanupClient, err := e.connect(reconnectCtx, req, authMethods)
		if err != nil {
			return
		}
		defer cleanupClient.Close()
		_ = cleanupRemoteStagingFiles(reconnectCtx, cleanupClient, stagedPaths)
	}()

	var releaseUpload *BootstrapUpload
	if req.AgentRelease != nil {
		destination := strings.TrimSpace(req.AgentRelease.DestinationPath)
		if destination == "" {
			return fmt.Errorf("agent release destination path is required")
		}
		for _, upload := range req.Uploads {
			if upload.Path == destination {
				return fmt.Errorf("agent release destination %q collides with a bootstrap upload", destination)
			}
		}
		binary, err := fetchVerifiedAgentRelease(ctx, sshRemoteOutputRunner{client: client}, *req.AgentRelease)
		if err != nil {
			return fmt.Errorf("target agent release acquisition: %w", err)
		}
		releaseUpload = &BootstrapUpload{Path: destination, Mode: 0o700, Content: binary}
	}

	for _, upload := range req.Uploads {
		stagedPaths = append(stagedPaths, upload.Path, remoteUploadPartialPath(upload.Path))
		if err := uploadRemoteFile(ctx, client, upload); err != nil {
			return err
		}
	}
	if releaseUpload != nil {
		stagedPaths = append(stagedPaths, releaseUpload.Path, remoteUploadPartialPath(releaseUpload.Path))
		if err := uploadRemoteFile(ctx, client, *releaseUpload); err != nil {
			return err
		}
	}
	if err := runRemoteCommand(ctx, client, req.RunCommand); err != nil {
		return err
	}
	bootstrapSucceeded = true
	return nil
}

func cleanupRemoteStagingFiles(ctx context.Context, client *ssh.Client, paths []string) error {
	quoted := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			quoted = append(quoted, shellQuote(path))
		}
	}
	if len(quoted) == 0 {
		return nil
	}
	return runRemoteCommand(ctx, client, "rm -f "+strings.Join(quoted, " "))
}

func remoteUploadPartialPath(path string) string {
	return path + ".part"
}

func buildRemoteUploadCommand(upload BootstrapUpload) string {
	return strings.Join([]string{
		"set -eu",
		"umask 077",
		"DEST=" + shellQuote(upload.Path),
		"PART=" + shellQuote(remoteUploadPartialPath(upload.Path)),
		"EXPECTED=" + strconv.Itoa(len(upload.Content)),
		`cleanup_upload() { rm -f "$PART"; }`,
		`trap cleanup_upload 0`,
		`trap 'exit 1' 1 2 13 15`,
		`rm -f "$PART"`,
		`cat > "$PART"`,
		`ACTUAL=$(wc -c < "$PART")`,
		`if [ "$ACTUAL" -ne "$EXPECTED" ]; then echo "incomplete bootstrap upload" >&2; exit 1; fi`,
		fmt.Sprintf(`chmod %04o "$PART"`, upload.Mode.Perm()),
		`mv -f "$PART" "$DEST"`,
		`trap - 0 1 2 13 15`,
	}, "\n")
}

func uploadRemoteFile(ctx context.Context, client *ssh.Client, upload BootstrapUpload) error {
	return runRemoteCommandWithInput(ctx, client, buildRemoteUploadCommand(upload), bytes.NewReader(upload.Content))
}

func runRemoteCommand(ctx context.Context, client *ssh.Client, command string) error {
	_, _, err := runRemoteCommandCapture(ctx, client, command, nil, 64<<10, 64<<10)
	return err
}

func runRemoteCommandWithInput(ctx context.Context, client *ssh.Client, command string, input io.Reader) error {
	_, _, err := runRemoteCommandCapture(ctx, client, command, input, 64<<10, 64<<10)
	return err
}

type sshRemoteOutputRunner struct {
	client *ssh.Client
}

func (r sshRemoteOutputRunner) Output(ctx context.Context, command string, limit int64) ([]byte, error) {
	stdout, _, err := runRemoteCommandCapture(ctx, r.client, command, nil, limit, 64<<10)
	return stdout, err
}

type boundedCapture struct {
	mu        sync.Mutex
	limit     int64
	data      []byte
	truncated bool
}

func newBoundedCapture(limit int64) *boundedCapture {
	if limit < 0 {
		limit = 0
	}
	return &boundedCapture{limit: limit}
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	originalLen := len(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - int64(len(b.data))
	if remaining > 0 {
		keep := int64(len(p))
		if keep > remaining {
			keep = remaining
		}
		b.data = append(b.data, p[:int(keep)]...)
	}
	if int64(originalLen) > remaining {
		b.truncated = true
	}
	// Always report the full write so SSH continues draining the channel even
	// after the retained diagnostic/payload limit has been reached.
	return originalLen, nil
}

func (b *boundedCapture) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.data...), b.truncated
}

type sshSessionResult struct {
	session *ssh.Session
	err     error
}

func newSSHSessionContext(ctx context.Context, client *ssh.Client) (*ssh.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(chan sshSessionResult, 1)
	go func() {
		session, err := client.NewSession()
		result <- sshSessionResult{session: session, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = client.Close()
		return nil, ctx.Err()
	case opened := <-result:
		return opened.session, opened.err
	}
}

const sshSessionCancelGrace = 250 * time.Millisecond

func closeSSHSessionOnCancel(client *ssh.Client, session *ssh.Session) {
	closed := make(chan struct{})
	go func() {
		_ = session.Close()
		close(closed)
	}()
	timer := time.NewTimer(sshSessionCancelGrace)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
		_ = client.Close()
	}
}

func startSSHSessionContext(ctx context.Context, client *ssh.Client, session *ssh.Session, command string) error {
	if err := ctx.Err(); err != nil {
		closeSSHSessionOnCancel(client, session)
		return err
	}
	result := make(chan error, 1)
	go func() { result <- session.Start(command) }()
	select {
	case <-ctx.Done():
		closeSSHSessionOnCancel(client, session)
		return ctx.Err()
	case err := <-result:
		return err
	}
}

func runRemoteCommandCapture(ctx context.Context, client *ssh.Client, command string, input io.Reader, stdoutLimit, stderrLimit int64) ([]byte, []byte, error) {
	session, err := newSSHSessionContext(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	defer session.Close()

	stdoutCapture := newBoundedCapture(stdoutLimit)
	stderrCapture := newBoundedCapture(stderrLimit)
	session.Stdout = stdoutCapture
	session.Stderr = stderrCapture
	if input != nil {
		session.Stdin = input
	}
	if err := startSSHSessionContext(ctx, client, session, command); err != nil {
		return nil, nil, err
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case <-ctx.Done():
		closeSSHSessionOnCancel(client, session)
		return nil, nil, ctx.Err()
	case runErr := <-done:
		stdout, stdoutTruncated := stdoutCapture.snapshot()
		stderr, stderrTruncated := stderrCapture.snapshot()
		if stdoutTruncated {
			return nil, stderr, fmt.Errorf("remote command stdout exceeds %d bytes", stdoutLimit)
		}
		if stderrTruncated {
			return nil, stderr, fmt.Errorf("remote command stderr exceeds %d bytes", stderrLimit)
		}
		if runErr != nil {
			detail := strings.TrimSpace(string(stderr))
			if detail == "" {
				detail = strings.TrimSpace(string(stdout))
			}
			if detail != "" {
				return stdout, stderr, fmt.Errorf("remote command failed: %s: %w", detail, runErr)
			}
			return stdout, stderr, runErr
		}
		return stdout, stderr, nil
	}
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
