// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/version"
)

const agentGitHubRepo = "cenvero/fleet"

// AutoInstallAgent downloads and installs fleet-agent on a remote Linux server via SSH,
// creates a systemd service, and marks the agent as managed in the server record.
// If version.Version is "dev", it falls back to uploading a local binary.
func (a *App) AutoInstallAgent(serverName, loginUser, loginKeyPath, loginPassword string, loginPort int, useSudo bool) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	if loginPort == 0 {
		loginPort = 22
	}
	if loginKeyPath == "" {
		loginKeyPath = filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	}

	sudo := ""
	if useSudo {
		sudo = "sudo "
	}

	pubKeyData, err := os.ReadFile(filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey+".pub"))
	if err != nil {
		return fmt.Errorf("read controller public key: %w", err)
	}

	serviceName := defaultServiceName
	agentPort := 2222
	if a.Config.Runtime.DefaultAgentPort > 0 {
		agentPort = a.Config.Runtime.DefaultAgentPort
	}

	serviceUnit, err := buildAgentServiceUnit(server, resolvedBootstrapConfig{
		serviceName:     serviceName,
		agentListenAddr: fmt.Sprintf("0.0.0.0:%d", agentPort),
		agentPort:       agentPort,
	})
	if err != nil {
		return err
	}

	token := randomBootstrapToken()
	tempServicePath := "/tmp/cenvero-" + token + ".service"
	tempKeysPath := "/tmp/cenvero-" + token + ".keys"
	tempScriptPath := "/tmp/cenvero-" + token + ".sh"

	uploads := []BootstrapUpload{
		{Path: tempServicePath, Mode: 0o600, Content: []byte(serviceUnit)},
		{Path: tempKeysPath, Mode: 0o644, Content: pubKeyData},
	}

	var installScript string
	if version.Version == "dev" {
		// Development build: upload local binary
		agentBinaryPath, err := resolveAgentBinaryPath("")
		if err != nil {
			return fmt.Errorf("dev build: %w", err)
		}
		binaryData, err := os.ReadFile(agentBinaryPath)
		if err != nil {
			return fmt.Errorf("read agent binary: %w", err)
		}
		tempBinPath := "/tmp/cenvero-" + token + ".bin"
		uploads = append(uploads, BootstrapUpload{
			Path:    tempBinPath,
			Mode:    0o700,
			Content: binaryData,
		})
		installScript = buildLocalBinaryInstallScript(server, sudo, serviceName, tempBinPath, tempServicePath, tempKeysPath, tempScriptPath)
	} else {
		installScript = buildRemoteDownloadInstallScript(server, sudo, serviceName, version.Version, tempServicePath, tempKeysPath, tempScriptPath)
	}

	uploads = append(uploads, BootstrapUpload{
		Path:    tempScriptPath,
		Mode:    0o700,
		Content: []byte(installScript),
	})

	executor := sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	req := BootstrapRequest{
		Address:          server.Address,
		Port:             loginPort,
		User:             loginUser,
		PrivateKeyPath:   loginKeyPath,
		Password:         loginPassword,
		KnownHostsPath:   filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey: true,
		Uploads:          uploads,
		RunCommand:       "/bin/sh " + shellQuote(tempScriptPath),
	}
	if err := executor.Bootstrap(context.Background(), req); err != nil {
		return fmt.Errorf("agent install: %w", err)
	}

	server.User = "root"
	server.Port = agentPort
	server.Agent = AgentInstall{
		Managed:     true,
		BinaryPath:  defaultAgentBinaryPath,
		ServiceName: serviceName,
		LoginUser:   loginUser,
		LoginPort:   loginPort,
		LoginKey:    loginKeyPath,
		UseSudo:     useSudo,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "agent.install",
		Target:   serverName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("version=%s service=%s port=%d login=%s:%d", version.Version, serviceName, agentPort, loginUser, loginPort),
	})
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

	token := randomBootstrapToken()
	tempTeardownPath := "/tmp/cenvero-" + token + ".sh"
	script := buildAgentTeardownScript(server.Agent.ServiceName, sudo, tempTeardownPath)
	executor := sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	req := BootstrapRequest{
		Address:          server.Address,
		Port:             loginPort,
		User:             loginUser,
		PrivateKeyPath:   loginKeyPath,
		Password:         password,
		KnownHostsPath:   filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey: true,
		Uploads: []BootstrapUpload{
			{Path: tempTeardownPath, Mode: 0o700, Content: []byte(script)},
		},
		RunCommand: "/bin/sh " + shellQuote(tempTeardownPath),
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
		return fmt.Sprintf(
			"To install the agent on %s, download fleet-agent_%s for your OS/arch from\n"+
				"https://github.com/%s/releases and run:\n\n"+
				"  fleet-agent reverse --controller <this-controller-address> --server-name %s\n\n"+
				"Add it to your init system to start on boot.",
			server.Name, ver, agentGitHubRepo, server.Name,
		)
	default:
		return fmt.Sprintf(
			"To install the agent on %s, download fleet-agent_%s for your OS/arch from\n"+
				"https://github.com/%s/releases and run:\n\n"+
				"  fleet-agent serve --listen 0.0.0.0:2222\n\n"+
				"Add it to your init system and ensure port 2222 is reachable.\n"+
				"Then run: fleet server reconnect %s",
			server.Name, ver, agentGitHubRepo, server.Name,
		)
	}
}

func buildRemoteDownloadInstallScript(server ServerRecord, sudo, serviceName, ver, tempServicePath, tempKeysPath, tempScriptPath string) string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
		"BIN_DIR=" + shellQuote(defaultAgentBinDir),
		"BIN_PATH=" + shellQuote(defaultAgentBinaryPath),
		"VERSION=" + shellQuote(ver),
		"REPO=" + shellQuote(agentGitHubRepo),
		"",
		"ARCH=\"$(uname -m)\"",
		"case \"$ARCH\" in",
		"  x86_64)          ARCH=amd64 ;;",
		"  aarch64|arm64)   ARCH=arm64 ;;",
		"  armv7l)          ARCH=armv7 ;;",
		"  *) echo \"unsupported architecture: $ARCH\" >&2; exit 1 ;;",
		"esac",
		"",
		"DLDIR=\"$(mktemp -d)\"",
		"trap 'rm -rf \"$DLDIR\"' EXIT",
		"TARBALL=\"$DLDIR/fleet-agent.tar.gz\"",
		"URL=\"https://github.com/${REPO}/releases/download/v${VERSION}/fleet-agent_${VERSION}_linux_${ARCH}.tar.gz\"",
		"",
		"if command -v curl >/dev/null 2>&1; then",
		"  curl -fsSL \"$URL\" -o \"$TARBALL\"",
		"elif command -v wget >/dev/null 2>&1; then",
		"  wget -q \"$URL\" -O \"$TARBALL\"",
		"else",
		"  echo \"curl or wget is required to install the agent\" >&2; exit 1",
		"fi",
		"",
		"tar -xzf \"$TARBALL\" -C \"$DLDIR\"",
		"EXTRACTED=\"$DLDIR/fleet-agent\"",
		"",
	}
	lines = append(lines, buildAgentSetupLines(sudo, serviceName, "$EXTRACTED", tempServicePath, tempKeysPath)...)
	lines = append(lines,
		"rm -f "+shellQuote(tempServicePath)+" "+shellQuote(tempKeysPath)+" "+shellQuote(tempScriptPath),
		"echo \""+version.ProductName+" agent installed on "+server.Name+"\"",
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
