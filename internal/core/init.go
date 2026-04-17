// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

func Initialize(opts InitOptions) (InitResult, error) {
	if opts.ConfigDir == "" {
		opts.ConfigDir = DefaultConfigDir("")
	}
	if opts.Alias == "" {
		opts.Alias = "fleet"
	}
	if opts.DefaultMode == "" {
		opts.DefaultMode = transport.ModeReverse
	}
	if opts.CryptoAlgorithm == "" {
		opts.CryptoAlgorithm = string(crypto.AlgorithmEd25519)
	}
	if opts.UpdateChannel == "" {
		opts.UpdateChannel = "stable"
	}
	if opts.UpdatePolicy == "" {
		opts.UpdatePolicy = update.PolicyNotifyOnly
	}
	if opts.DatabaseBackend == "" {
		opts.DatabaseBackend = store.BackendSQLite
	}
	if opts.Operator == "" {
		if currentUser, err := user.Current(); err == nil {
			opts.Operator = currentUser.Username
		}
	}

	if err := EnsureLayout(opts.ConfigDir); err != nil {
		return InitResult{}, err
	}

	configPath := ConfigPath(opts.ConfigDir)
	if _, err := os.Stat(configPath); err == nil {
		return InitResult{}, fmt.Errorf("configuration already exists at %s", configPath)
	}

	instanceID, err := generateInstanceID()
	if err != nil {
		return InitResult{}, err
	}

	cfg := DefaultConfig(opts.ConfigDir)
	cfg.Alias = opts.Alias
	cfg.InitVersion = CurrentInitVersion
	cfg.LastSeenVersion = version.Version
	cfg.InstanceID = instanceID
	cfg.DefaultMode = opts.DefaultMode
	cfg.Operator = opts.Operator
	cfg.Crypto.Algorithm = opts.CryptoAlgorithm
	cfg.Crypto.PassphraseProtected = opts.Passphrase != ""
	cfg.Updates.Channel = opts.UpdateChannel
	cfg.Updates.Policy = opts.UpdatePolicy
	cfg.Database.Backend = opts.DatabaseBackend
	switch opts.DatabaseBackend {
	case store.BackendPostgres:
		cfg.Database.Postgres.DSN = opts.DatabaseDSN
	case store.BackendMySQL:
		cfg.Database.MySQL.DSN = opts.DatabaseDSN
	case store.BackendMariaDB:
		cfg.Database.MariaDB.DSN = opts.DatabaseDSN
	}
	cfg.Database = store.WithDefaults(cfg.Database, opts.ConfigDir)
	if opts.CryptoAlgorithm == string(crypto.AlgorithmRSA4096) {
		cfg.Crypto.PrimaryKey = "id_rsa4096"
	}
	if opts.DefaultAgentPort > 0 {
		cfg.Runtime.DefaultAgentPort = opts.DefaultAgentPort
	}
	if opts.ListenAddress != "" {
		cfg.Runtime.ListenAddress = opts.ListenAddress
	}

	algo, err := crypto.ParseAlgorithm(opts.CryptoAlgorithm)
	if err != nil {
		return InitResult{}, err
	}
	if err := crypto.GenerateKeySet(filepath.Join(opts.ConfigDir, "keys"), algo, []byte(opts.Passphrase)); err != nil {
		return InitResult{}, err
	}

	if err := SaveConfig(configPath, cfg); err != nil {
		return InitResult{}, err
	}

	if err := os.WriteFile(filepath.Join(opts.ConfigDir, "instance.id"), []byte(instanceID+"\n"), 0o600); err != nil {
		return InitResult{}, fmt.Errorf("write instance id: %w", err)
	}

	stateStore, err := store.Open(cfg.Database, store.WorkloadState)
	if err != nil {
		return InitResult{}, err
	}
	defer stateStore.Close()
	metricsStore, err := store.Open(cfg.Database, store.WorkloadMetrics)
	if err != nil {
		return InitResult{}, err
	}
	defer metricsStore.Close()
	eventsStore, err := store.Open(cfg.Database, store.WorkloadEvents)
	if err != nil {
		return InitResult{}, err
	}
	defer eventsStore.Close()

	_ = stateStore.PutState("instance_id", instanceID)
	_ = metricsStore.PutState("initialized", "true")
	_ = eventsStore.AppendEvent(time.Now().UTC(), "controller.init", fmt.Sprintf(`{"instance_id":%q}`, instanceID))

	audit := logs.NewAuditLog(filepath.Join(opts.ConfigDir, "logs", "_audit.log"))
	if err := audit.Append(logs.AuditEntry{
		Action:   "controller.init",
		Target:   opts.ConfigDir,
		Operator: opts.Operator,
		Details:  fmt.Sprintf("mode=%s channel=%s policy=%s", opts.DefaultMode, opts.UpdateChannel, opts.UpdatePolicy),
	}); err != nil {
		return InitResult{}, err
	}

	if opts.Alias != "fleet" {
		_ = maybeCreateAlias(opts.ExecutablePath, opts.Alias)
	}

	keys := []string{"id_ed25519"}
	if algo == crypto.AlgorithmRSA4096 {
		keys = []string{"id_rsa4096"}
	}
	if algo == crypto.AlgorithmBoth {
		keys = []string{"id_ed25519", "id_rsa4096"}
	}

	return InitResult{
		Config:     cfg,
		ConfigPath: configPath,
		Keys:       keys,
	}, nil
}

func RunInitInteractive(in io.Reader, out io.Writer, executablePath string) (InitResult, error) {
	reader := bufio.NewReader(in)
	home, _ := os.UserHomeDir()
	defaultDir := DefaultConfigDir(home)

	fmt.Fprintln(out, "┌─────────────────────────────────────────────────────────────┐")
	fmt.Fprintln(out, "│  Welcome to Cenvero Fleet v1.0                              │")
	fmt.Fprintln(out, "│  Command your fleet.                                        │")
	fmt.Fprintln(out, "│  https://fleet.cenvero.org                                  │")
	fmt.Fprintln(out, "└─────────────────────────────────────────────────────────────┘")
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Step 1 of 6 — Configuration directory")
	fmt.Fprintln(out, "─────────────────────────────────────")
	fmt.Fprintf(out, "  [1] %s              (recommended, per-user)\n", defaultDir)
	fmt.Fprintln(out, "  [2] /etc/cenvero-fleet            (system-wide, requires sudo)")
	fmt.Fprintln(out, "  [3] /opt/cenvero-fleet")
	fmt.Fprintln(out, "  [4] Custom path")
	choice, err := prompt(reader, out, "  Choice [1]: ", "1")
	if err != nil {
		return InitResult{}, err
	}
	configDir := defaultDir
	switch choice {
	case "2":
		configDir = "/etc/cenvero-fleet"
	case "3":
		configDir = "/opt/cenvero-fleet"
	case "4":
		configDir, err = prompt(reader, out, "  Custom path: ", defaultDir)
		if err != nil {
			return InitResult{}, err
		}
	}

	alias := "fleet"

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Step 2 of 6 — Default transport mode")
	fmt.Fprintln(out, "─────────────────────────────────────")
	fmt.Fprintln(out, "  [1] Reverse mode (agent connects to you — works behind NAT)")
	fmt.Fprintln(out, "  [2] Direct mode  (you SSH into the agent — agent must be reachable)")
	fmt.Fprintln(out, "  [3] Decide per server (no default)")
	modeChoice, err := prompt(reader, out, "  Choice [1]: ", "1")
	if err != nil {
		return InitResult{}, err
	}
	mode := transport.ModeReverse
	if modeChoice == "2" {
		mode = transport.ModeDirect
	}
	if modeChoice == "3" {
		mode = transport.ModePerNode
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Step 3 of 6 — Networking")
	fmt.Fprintln(out, "─────────────────────────────────────")
	defaultAgentPort := 0
	listenAddress := ""
	if mode == transport.ModeDirect || mode == transport.ModePerNode {
		fmt.Fprintln(out, "  Direct mode: the agent exposes an SSH port so this controller can connect.")
		agentPortStr, err := prompt(reader, out, "  Default agent SSH port [2222]: ", "2222")
		if err != nil {
			return InitResult{}, err
		}
		p, err := strconv.Atoi(strings.TrimSpace(agentPortStr))
		if err != nil || p < 1 || p > 65535 {
			return InitResult{}, fmt.Errorf("invalid agent port %q", agentPortStr)
		}
		defaultAgentPort = p
	}
	if mode == transport.ModeReverse || mode == transport.ModePerNode {
		fmt.Fprintln(out, "  Reverse mode: agents dial this controller — it must have a reachable static IP.")
		fmt.Fprintln(out, "  Use 0.0.0.0 to listen on all interfaces, or set a specific IP.")
		listenAddress, err = prompt(reader, out, "  Controller listen address [0.0.0.0:9443]: ", "0.0.0.0:9443")
		if err != nil {
			return InitResult{}, err
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Step 4 of 6 — Cryptography")
	fmt.Fprintln(out, "─────────────────────────────────────")
	fmt.Fprintln(out, "  Algorithm:")
	fmt.Fprintln(out, "    [1] Ed25519  (recommended)")
	fmt.Fprintln(out, "    [2] RSA-4096 (legacy compatibility)")
	fmt.Fprintln(out, "    [3] Both     (Ed25519 primary, RSA-4096 fallback)")
	algoChoice, err := prompt(reader, out, "  Choice [1]: ", "1")
	if err != nil {
		return InitResult{}, err
	}
	algo := string(crypto.AlgorithmEd25519)
	switch algoChoice {
	case "2":
		algo = string(crypto.AlgorithmRSA4096)
	case "3":
		algo = string(crypto.AlgorithmBoth)
	}
	passphrase := ""

	channel := "stable"
	policy := update.PolicyNotifyOnly
	if IsHomebrewInstall(executablePath) {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Step 5 of 6 — Update notifications")
		fmt.Fprintln(out, "─────────────────────────────────────")
		fmt.Fprintln(out, "  Homebrew install detected — updates are managed by Homebrew.")
		fmt.Fprintln(out, "  Run 'brew upgrade cenvero-fleet' to update when a new version is available.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  Fleet can remind you when a new version is available:")
		fmt.Fprintln(out, "    [1] Notify me (show reminder on each fleet command)")
		fmt.Fprintln(out, "    [2] Disabled  (no reminders)")
		notifyChoice, err := prompt(reader, out, "  Choice [1]: ", "1")
		if err != nil {
			return InitResult{}, err
		}
		if notifyChoice == "2" {
			policy = update.PolicyDisabled
		}
	} else {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Step 5 of 6 — Update channel & policy")
		fmt.Fprintln(out, "─────────────────────────────────────")
		fmt.Fprintln(out, "  Channel:")
		fmt.Fprintln(out, "    [1] stable (recommended)")
		fmt.Fprintln(out, "    [2] beta   (early features)")
		channelChoice, err := prompt(reader, out, "  Choice [1]: ", "1")
		if err != nil {
			return InitResult{}, err
		}
		if channelChoice == "2" {
			channel = "beta"
		}
		fmt.Fprintln(out, "  Policy:")
		fmt.Fprintln(out, "    [1] Auto-update  (download and apply automatically)")
		fmt.Fprintln(out, "    [2] Notify only  (tell you when an update is available, you apply it)")
		fmt.Fprintln(out, "    [3] Disabled     (no update checks)")
		policyChoice, err := prompt(reader, out, "  Choice [2]: ", "2")
		if err != nil {
			return InitResult{}, err
		}
		switch policyChoice {
		case "1":
			policy = update.PolicyAutoUpdate
		case "3":
			policy = update.PolicyDisabled
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Step 6 of 6 — Database backend")
	fmt.Fprintln(out, "─────────────────────────────────────")
	fmt.Fprintln(out, "  [1] SQLite   (recommended, separate local files)")
	fmt.Fprintln(out, "  [2] PostgreSQL")
	fmt.Fprintln(out, "  [3] MySQL")
	fmt.Fprintln(out, "  [4] MariaDB")
	backendChoice, err := prompt(reader, out, "  Choice [1]: ", "1")
	if err != nil {
		return InitResult{}, err
	}
	backend := store.BackendSQLite
	dsn := ""
	switch backendChoice {
	case "2":
		backend = store.BackendPostgres
	case "3":
		backend = store.BackendMySQL
	case "4":
		backend = store.BackendMariaDB
	}
	if backend != store.BackendSQLite {
		dsn, err = prompt(reader, out, "  DSN: ", "")
		if err != nil {
			return InitResult{}, err
		}
		if dsn == "" {
			return InitResult{}, fmt.Errorf("dsn is required for %s", backend)
		}
	}

	return Initialize(InitOptions{
		ConfigDir:        configDir,
		Alias:            alias,
		DefaultMode:      mode,
		CryptoAlgorithm:  algo,
		Passphrase:       passphrase,
		UpdateChannel:    channel,
		UpdatePolicy:     policy,
		DatabaseBackend:  backend,
		DatabaseDSN:      dsn,
		ExecutablePath:   executablePath,
		DefaultAgentPort: defaultAgentPort,
		ListenAddress:    listenAddress,
	})
}

func prompt(reader *bufio.Reader, out io.Writer, label, fallback string) (string, error) {
	fmt.Fprint(out, label)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = fallback
	}
	return line, nil
}

func generateInstanceID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate instance id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func maybeCreateAlias(executablePath, alias string) error {
	if executablePath == "" {
		return nil
	}
	if _, err := os.Stat(executablePath); err != nil {
		return err
	}
	targetDir := filepath.Dir(executablePath)
	linkPath := filepath.Join(targetDir, alias)
	if _, err := os.Lstat(linkPath); err == nil {
		return nil
	}
	return os.Symlink(executablePath, linkPath)
}
