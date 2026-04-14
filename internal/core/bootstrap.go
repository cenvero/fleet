// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	defaultAgentBinaryPath = "/usr/local/bin/fleet-agent"
	defaultStateDir        = "/var/lib/cenvero-fleet-agent"
	defaultConfigDir       = "/etc/cenvero-fleet-agent"
	defaultDirectListen    = "0.0.0.0:2222"
)

func defaultDirectAuthorizedKeysPath() string {
	return filepath.Join(defaultStateDir, "authorized_keys")
}

type BootstrapExecutor interface {
	Bootstrap(context.Context, BootstrapRequest) error
}

type BootstrapRequest struct {
	Address          string
	Port             int
	User             string
	PrivateKeyPath   string
	KnownHostsPath   string
	AcceptNewHostKey bool
	Uploads          []BootstrapUpload
	RunCommand       string
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
		Address:          server.Address,
		Port:             resolved.loginPort,
		User:             resolved.loginUser,
		PrivateKeyPath:   resolved.loginKeyPath,
		KnownHostsPath:   filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey: resolved.acceptNewHostKey,
		Uploads: []BootstrapUpload{
			{Path: resolved.tempBinaryPath, Mode: 0o700, Content: binaryData},
			{Path: resolved.tempUnitPath, Mode: 0o600, Content: []byte(serviceUnit)},
		},
		RunCommand: "/bin/sh " + shellQuote(resolved.tempScriptPath),
	}
	if len(authorizedKeys) > 0 {
		request.Uploads = append(request.Uploads, BootstrapUpload{
			Path:    resolved.tempAuthorizedKeysPath,
			Mode:    0o644,
			Content: authorizedKeys,
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
		BinaryPath:  defaultAgentBinaryPath,
		ServiceName: resolved.serviceName,
		UpdatedAt:   time.Now().UTC(),
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
	serviceName            string
	useSudo                bool
	acceptNewHostKey       bool
	tempBinaryPath         string
	tempUnitPath           string
	tempScriptPath         string
	tempAuthorizedKeysPath string
}

func (a *App) resolveBootstrapConfig(server ServerRecord, opts BootstrapOptions) (resolvedBootstrapConfig, error) {
	loginUser := strings.TrimSpace(opts.LoginUser)
	if loginUser == "" {
		if opts.PrintScript {
			loginUser = "root"
		} else {
			return resolvedBootstrapConfig{}, fmt.Errorf("bootstrap login user is required")
		}
	}

	loginPort := opts.LoginPort
	if loginPort == 0 {
		loginPort = 22
	}

	loginKeyPath := strings.TrimSpace(opts.LoginKeyPath)
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

	agentListenAddr := strings.TrimSpace(opts.AgentListenAddr)
	agentPort := server.Port
	if agentPort == 0 || agentPort == 22 {
		agentPort = 2222
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
	if server.Mode == transport.ModeReverse {
		if controllerAddress == "" {
			controllerAddress = defaultControllerBootstrapAddress(a.Config.Runtime.ListenAddress)
		}
		if controllerAddress == "" {
			return resolvedBootstrapConfig{}, fmt.Errorf("controller address is required for reverse bootstrap; pass --controller with a reachable address")
		}
	}

	return resolvedBootstrapConfig{
		loginUser:              loginUser,
		loginPort:              loginPort,
		loginKeyPath:           loginKeyPath,
		agentBinaryPath:        agentBinaryPath,
		agentListenAddr:        agentListenAddr,
		agentPort:              agentPort,
		controllerAddress:      controllerAddress,
		serviceName:            serviceName,
		useSudo:                opts.UseSudo,
		acceptNewHostKey:       opts.AcceptNewHostKey,
		tempBinaryPath:         "/tmp/cenvero-fleet-agent.bin",
		tempUnitPath:           "/tmp/cenvero-fleet-agent.service",
		tempScriptPath:         "/tmp/cenvero-fleet-agent-bootstrap.sh",
		tempAuthorizedKeysPath: "/tmp/cenvero-fleet-authorized_keys",
	}, nil
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
		execStart = fmt.Sprintf("%s serve --mode direct --listen %s --host-key %s --authorized-keys %s",
			defaultAgentBinaryPath,
			shellQuote(cfg.agentListenAddr),
			shellQuote(filepath.Join(defaultStateDir, "ssh_host_ed25519_key")),
			shellQuote(defaultDirectAuthorizedKeysPath()),
		)
	case transport.ModeReverse:
		execStart = fmt.Sprintf("%s reverse --mode reverse --controller %s --server-name %s --host-key %s --known-hosts %s",
			defaultAgentBinaryPath,
			shellQuote(cfg.controllerAddress),
			shellQuote(server.Name),
			shellQuote(filepath.Join(defaultStateDir, "ssh_host_ed25519_key")),
			shellQuote(filepath.Join(defaultStateDir, "controller_known_hosts")),
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
		"BIN_PATH=" + shellQuote(defaultAgentBinaryPath),
		"TEMP_BIN=" + shellQuote(cfg.tempBinaryPath),
		"TEMP_UNIT=" + shellQuote(cfg.tempUnitPath),
		"TEMP_SCRIPT=" + shellQuote(cfg.tempScriptPath),
		"",
		sudo + "mkdir -p \"$STATE_DIR\" \"$CONFIG_DIR\"",
		sudo + "install -m 0755 \"$TEMP_BIN\" \"$BIN_PATH\"",
		sudo + "install -m 0644 \"$TEMP_UNIT\" /etc/systemd/system/\"$SERVICE_NAME\".service",
	}
	if includeAuthorizedKeys {
		lines = append(lines, sudo+"install -m 0600 "+shellQuote(cfg.tempAuthorizedKeysPath)+" "+shellQuote(defaultDirectAuthorizedKeysPath()))
	}
	lines = append(lines,
		sudo+"systemctl daemon-reload",
		sudo+"systemctl enable --now \"$SERVICE_NAME\".service",
		"rm -f \"$TEMP_BIN\" \"$TEMP_UNIT\" \"$TEMP_SCRIPT\"",
	)
	if includeAuthorizedKeys {
		lines = append(lines, "rm -f "+shellQuote(cfg.tempAuthorizedKeysPath))
	}
	lines = append(lines, "echo \"Bootstrapped "+version.ProductName+" agent on "+server.Name+"\"")
	return strings.Join(lines, "\n") + "\n"
}

type sshBootstrapExecutor struct {
	networkDialContext func(context.Context, string, string) (net.Conn, error)
}

func (e sshBootstrapExecutor) Bootstrap(ctx context.Context, req BootstrapRequest) error {
	signer, err := fleetcrypto.LoadPrivateKeySigner(req.PrivateKeyPath, nil)
	if err != nil {
		return err
	}

	hostKeyCallback, err := transport.NewTOFUHostKeyCallback(req.KnownHostsPath, req.AcceptNewHostKey, &transport.HostKeyState{})
	if err != nil {
		return err
	}

	address := net.JoinHostPort(req.Address, strconv.Itoa(req.Port))
	config := &ssh.ClientConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		User:            req.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         10 * time.Second,
	}

	var rawConn net.Conn
	if e.networkDialContext != nil {
		rawConn, err = e.networkDialContext(ctx, "tcp", address)
	} else {
		rawConn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("dial bootstrap target %s: %w", address, err)
	}
	defer rawConn.Close()

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, address, config)
	if err != nil {
		return fmt.Errorf("establish bootstrap ssh connection to %s: %w", address, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	for _, upload := range req.Uploads {
		if err := uploadRemoteFile(ctx, client, upload); err != nil {
			return err
		}
	}
	return runRemoteCommand(ctx, client, req.RunCommand)
}

func uploadRemoteFile(ctx context.Context, client *ssh.Client, upload BootstrapUpload) error {
	command := fmt.Sprintf("umask 077 && cat > %s && chmod %04o %s", shellQuote(upload.Path), upload.Mode.Perm(), shellQuote(upload.Path))
	return runRemoteCommandWithInput(ctx, client, command, bytes.NewReader(upload.Content))
}

func runRemoteCommand(ctx context.Context, client *ssh.Client, command string) error {
	return runRemoteCommandWithInput(ctx, client, command, nil)
}

func runRemoteCommandWithInput(ctx context.Context, client *ssh.Client, command string, input io.Reader) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	var output bytes.Buffer
	session.Stdout = &output
	session.Stderr = &output

	if input != nil {
		stdin, err := session.StdinPipe()
		if err != nil {
			return err
		}
		go func() {
			_, _ = io.Copy(stdin, input)
			_ = stdin.Close()
		}()
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	select {
	case <-ctx.Done():
		_ = session.Close()
		return ctx.Err()
	case err := <-done:
		if err != nil {
			text := strings.TrimSpace(output.String())
			if text != "" {
				return fmt.Errorf("remote command failed: %s: %w", text, err)
			}
			return err
		}
		return nil
	}
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
