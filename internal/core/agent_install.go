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
func (a *App) AutoInstallAgent(serverName, loginUser, loginKeyPath string, loginPort int, useSudo bool) error {
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

	serviceUnit, err := buildAgentServiceUnit(server, resolvedBootstrapConfig{
		serviceName:     serviceName,
		agentListenAddr: fmt.Sprintf("0.0.0.0:%d", agentPort),
		agentPort:       agentPort,
	})
	if err != nil {
		return err
	}

	uploads := []BootstrapUpload{
		{Path: "/tmp/cenvero-fleet-agent.service", Mode: 0o600, Content: []byte(serviceUnit)},
		{Path: "/tmp/cenvero-fleet-authorized_keys", Mode: 0o644, Content: pubKeyData},
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
		uploads = append(uploads, BootstrapUpload{
			Path:    "/tmp/cenvero-fleet-agent.bin",
			Mode:    0o700,
			Content: binaryData,
		})
		installScript = buildLocalBinaryInstallScript(server, sudo, serviceName)
	} else {
		installScript = buildRemoteDownloadInstallScript(server, sudo, serviceName, version.Version)
	}

	uploads = append(uploads, BootstrapUpload{
		Path:    "/tmp/cenvero-fleet-agent-install.sh",
		Mode:    0o700,
		Content: []byte(installScript),
	})

	executor := sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	req := BootstrapRequest{
		Address:          server.Address,
		Port:             loginPort,
		User:             loginUser,
		PrivateKeyPath:   loginKeyPath,
		KnownHostsPath:   filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey: true,
		Uploads:          uploads,
		RunCommand:       "/bin/sh /tmp/cenvero-fleet-agent-install.sh",
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
	if !server.Agent.Managed {
		return nil
	}

	loginUser := server.Agent.LoginUser
	if loginUser == "" {
		return fmt.Errorf("cannot teardown agent on %q: no login user stored", server.Name)
	}
	loginPort := server.Agent.LoginPort
	if loginPort == 0 {
		loginPort = 22
	}
	loginKeyPath := server.Agent.LoginKey
	if loginKeyPath == "" {
		loginKeyPath = filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	}

	sudo := ""
	if server.Agent.UseSudo {
		sudo = "sudo "
	}

	script := buildAgentTeardownScript(server.Agent.ServiceName, sudo)
	executor := sshBootstrapExecutor{networkDialContext: a.NetworkDialContext}
	req := BootstrapRequest{
		Address:          server.Address,
		Port:             loginPort,
		User:             loginUser,
		PrivateKeyPath:   loginKeyPath,
		KnownHostsPath:   filepath.Join(a.ConfigDir, "keys", "bootstrap_known_hosts"),
		AcceptNewHostKey: false,
		Uploads: []BootstrapUpload{
			{Path: "/tmp/cenvero-fleet-teardown.sh", Mode: 0o700, Content: []byte(script)},
		},
		RunCommand: "/bin/sh /tmp/cenvero-fleet-teardown.sh",
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
				"  fleet-agent serve --mode direct --listen 0.0.0.0:2222\n\n"+
				"Add it to your init system and ensure port 2222 is reachable.\n"+
				"Then run: fleet server reconnect %s",
			server.Name, ver, agentGitHubRepo, server.Name,
		)
	}
}

func buildRemoteDownloadInstallScript(server ServerRecord, sudo, serviceName, ver string) string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
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
		"URL=\"https://github.com/${REPO}/releases/download/${VERSION}/fleet-agent_${VERSION}_linux_${ARCH}.tar.gz\"",
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
	lines = append(lines, buildAgentSetupLines(sudo, serviceName, "$EXTRACTED")...)
	lines = append(lines,
		"rm -f /tmp/cenvero-fleet-agent.service /tmp/cenvero-fleet-authorized_keys /tmp/cenvero-fleet-agent-install.sh",
		"echo \""+version.ProductName+" agent installed on "+server.Name+"\"",
	)
	return strings.Join(lines, "\n") + "\n"
}

func buildLocalBinaryInstallScript(server ServerRecord, sudo, serviceName string) string {
	lines := []string{
		"#!/bin/sh",
		"set -eu",
		"SERVICE_NAME=" + shellQuote(serviceName),
		"STATE_DIR=" + shellQuote(defaultStateDir),
		"CONFIG_DIR=" + shellQuote(defaultConfigDir),
		"",
	}
	lines = append(lines, buildAgentSetupLines(sudo, serviceName, "/tmp/cenvero-fleet-agent.bin")...)
	lines = append(lines,
		"rm -f /tmp/cenvero-fleet-agent.service /tmp/cenvero-fleet-authorized_keys /tmp/cenvero-fleet-agent.bin /tmp/cenvero-fleet-agent-install.sh",
		"echo \""+version.ProductName+" agent installed on "+server.Name+" (dev build)\"",
	)
	return strings.Join(lines, "\n") + "\n"
}

func buildAgentSetupLines(sudo, serviceName, binarySource string) []string {
	return []string{
		sudo + "mkdir -p \"$STATE_DIR\" \"$CONFIG_DIR\"",
		sudo + "install -m 0755 " + binarySource + " \"$BIN_PATH\"",
		sudo + "install -m 0644 /tmp/cenvero-fleet-agent.service /etc/systemd/system/" + shellQuote(serviceName) + ".service",
		sudo + "install -m 0600 /tmp/cenvero-fleet-authorized_keys \"$STATE_DIR/authorized_keys\"",
		sudo + "systemctl daemon-reload",
		sudo + "systemctl enable --now " + shellQuote(serviceName) + ".service",
	}
}

func buildAgentTeardownScript(serviceName, sudo string) string {
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
		sudo + "rm -f " + shellQuote(defaultAgentBinaryPath),
		sudo + "rm -rf " + shellQuote(defaultStateDir) + " " + shellQuote(defaultConfigDir),
		"rm -f /tmp/cenvero-fleet-teardown.sh",
		"echo \"" + version.ProductName + " agent removed\"",
	}
	return strings.Join(lines, "\n") + "\n"
}
