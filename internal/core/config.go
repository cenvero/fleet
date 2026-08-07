// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9]{2,8}$`)

// safeNamePattern allows alphanumeric characters, hyphens, underscores, and dots
// but not sequences that could escape a directory (no "..", no slashes).
var safeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// validateSafeName returns an error if name could be used for directory traversal
// or filesystem injection when embedded in a file path.
func validateSafeName(name string) error {
	if !safeNamePattern.MatchString(name) {
		return fmt.Errorf("%q contains characters not allowed in names (use letters, digits, hyphens, underscores, dots; max 63 chars)", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%q contains '..' which is not allowed", name)
	}
	return nil
}

func DefaultConfigDir(home string) string {
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ".cenvero-fleet"
	}
	return filepath.Join(home, ".cenvero-fleet")
}

func DefaultConfig(configDir string) Config {
	return Config{
		SchemaVersion: 1,
		ProductName:   version.ProductName,
		Domain:        version.Domain,
		Alias:         version.BinaryName,
		ConfigDir:     configDir,
		DefaultMode:   "reverse",
		ManifestURL:   update.DefaultManifestURL,
		InitializedAt: time.Now().UTC(),
		Crypto: CryptoConfig{
			Algorithm:         "ed25519",
			PrimaryKey:        "id_ed25519",
			KnownHostsPath:    filepath.Join(configDir, "keys", "known_hosts"),
			RotationDirectory: filepath.Join(configDir, "keys", "rotations"),
		},
		Updates: UpdateConfig{
			Channel: "stable",
			Policy:  update.PolicyNotifyOnly,
		},
		Database: store.DefaultDatabaseConfig(configDir),
		Runtime: RuntimeConfig{
			ListenAddress:         "127.0.0.1:9443",
			ControlAddress:        "127.0.0.1:9444",
			DataDir:               filepath.Join(configDir, "data"),
			LogDir:                filepath.Join(configDir, "logs"),
			AggregatedLogDir:      filepath.Join(configDir, "logs", "_aggregated"),
			AggregatedLogMaxSize:  logs.DefaultAggregatedLogMaxSize,
			AggregatedLogMaxFiles: logs.DefaultAggregatedLogMaxFiles,
			AggregatedLogMaxAge:   logs.DefaultAggregatedLogMaxAge.String(),
			AlertNotifyCooldown:   "6h",
			MetricsPollInterval:   "1m",
			DesktopNotifications:  true,
			JobLogRetention:       DefaultJobLogRetention,
			SessionReconnectGrace: DefaultSessionReconnectGrace,
			FileTransfer: FileTransferDefaults{
				ParallelStreams: DefaultParallelStreams,
				ChunkSizeBytes:  DefaultChunkSizeBytes,
			},
		},
	}
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Dir(path)
	}
	cfg.Database = store.WithDefaults(cfg.Database, cfg.ConfigDir)
	cfg.Runtime = withRuntimeDefaults(cfg.Runtime, DefaultConfig(cfg.ConfigDir).Runtime)
	return cfg, cfg.Validate()
}

func withRuntimeDefaults(cfg RuntimeConfig, defaults RuntimeConfig) RuntimeConfig {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = defaults.ListenAddress
	}
	if cfg.ControlAddress == "" {
		cfg.ControlAddress = defaults.ControlAddress
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaults.DataDir
	}
	if cfg.LogDir == "" {
		cfg.LogDir = defaults.LogDir
	}
	if cfg.AggregatedLogDir == "" {
		cfg.AggregatedLogDir = defaults.AggregatedLogDir
	}
	if cfg.AggregatedLogMaxSize == 0 {
		cfg.AggregatedLogMaxSize = defaults.AggregatedLogMaxSize
	}
	if cfg.AggregatedLogMaxFiles == 0 {
		cfg.AggregatedLogMaxFiles = defaults.AggregatedLogMaxFiles
	}
	if cfg.AggregatedLogMaxAge == "" {
		cfg.AggregatedLogMaxAge = defaults.AggregatedLogMaxAge
	}
	if cfg.AlertNotifyCooldown == "" {
		cfg.AlertNotifyCooldown = defaults.AlertNotifyCooldown
	}
	if cfg.MetricsPollInterval == "" {
		cfg.MetricsPollInterval = defaults.MetricsPollInterval
	}
	if cfg.JobLogRetention == "" {
		cfg.JobLogRetention = defaults.JobLogRetention
	}
	if cfg.SessionReconnectGrace == "" {
		cfg.SessionReconnectGrace = defaults.SessionReconnectGrace
	}
	return cfg
}

func (c Config) Validate() error {
	if c.ProductName == "" {
		return fmt.Errorf("product name is required")
	}
	if c.ConfigDir == "" {
		return fmt.Errorf("config dir is required")
	}
	if !aliasPattern.MatchString(c.Alias) {
		return fmt.Errorf("alias must be 2-8 alphanumeric characters")
	}
	if c.DefaultMode == "" {
		return fmt.Errorf("default transport mode is required")
	}
	if c.Crypto.Algorithm == "" {
		return fmt.Errorf("crypto algorithm is required")
	}
	if c.Updates.Channel == "" {
		return fmt.Errorf("update channel is required")
	}
	if c.Updates.Policy == "" {
		return fmt.Errorf("update policy is required")
	}
	if c.Runtime.ListenAddress == "" {
		return fmt.Errorf("runtime listen address is required")
	}
	if c.Runtime.ControlAddress == "" {
		return fmt.Errorf("runtime control address is required")
	}
	if c.Runtime.AggregatedLogDir == "" {
		return fmt.Errorf("runtime aggregated log dir is required")
	}
	if c.Runtime.AggregatedLogMaxSize <= 0 {
		return fmt.Errorf("runtime aggregated log max size must be positive")
	}
	if c.Runtime.AggregatedLogMaxFiles <= 0 {
		return fmt.Errorf("runtime aggregated log max files must be positive")
	}
	if c.Runtime.AggregatedLogMaxAge != "" {
		duration, err := time.ParseDuration(c.Runtime.AggregatedLogMaxAge)
		if err != nil {
			return fmt.Errorf("runtime aggregated log max age: %w", err)
		}
		if duration <= 0 {
			return fmt.Errorf("runtime aggregated log max age must be positive")
		}
	}
	if c.Runtime.AlertNotifyCooldown != "" {
		if _, err := time.ParseDuration(c.Runtime.AlertNotifyCooldown); err != nil {
			return fmt.Errorf("runtime alert notify cooldown: %w", err)
		}
	}
	if c.Runtime.MetricsPollInterval != "" {
		if _, err := time.ParseDuration(c.Runtime.MetricsPollInterval); err != nil {
			return fmt.Errorf("runtime metrics poll interval: %w", err)
		}
	}
	if v := strings.TrimSpace(c.Runtime.JobLogRetention); v != "" {
		switch strings.ToLower(v) {
		case "0", "off", "never", "disabled": // explicit "no pruning" — valid
		default:
			if _, err := ParseFlexDuration(v); err != nil {
				return fmt.Errorf("runtime job log retention: %w", err)
			}
		}
	}
	if c.Runtime.SessionReconnectGrace != "" {
		if _, err := ParseFlexDuration(c.Runtime.SessionReconnectGrace); err != nil {
			return fmt.Errorf("runtime session reconnect grace: %w", err)
		}
	}
	if err := c.Database.Validate(); err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	return nil
}

func SaveConfig(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	// Atomic write: temp file → rename so a crash never leaves a partial config.
	// Explicit 0o600 regardless of umask — config.toml may contain database DSNs.
	tmp, err := os.CreateTemp(dir, ".config.*.toml")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set config permissions: %w", err)
	}
	if err := toml.NewEncoder(tmp).Encode(cfg); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("flush config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}

func ConfigPath(configDir string) string {
	return filepath.Join(configDir, "config.toml")
}

// StampLastSeenVersion writes the running binary's version into the config file
// so that 'fleet recover' can detect version mismatches later.
// It is a best-effort write — errors are silently ignored to avoid disrupting
// normal command execution.
func StampLastSeenVersion(configPath string, cfg Config) {
	if cfg.LastSeenVersion == version.Version {
		return // already up to date, avoid a pointless write
	}
	cfg.LastSeenVersion = version.Version
	_ = SaveConfig(configPath, cfg)
}

// IsInitialized reports whether the config directory has been set up by `fleet init`.
func IsInitialized(configDir string) bool {
	_, err := os.Stat(ConfigPath(configDir))
	return err == nil
}

func EnsureLayout(configDir string) error {
	// keys/ (and its rotations/ subdir) hold the controller PRIVATE key and the
	// secret-encryption key, so they are created 0700 — owner-only, never group/
	// world traversable. The remaining layout dirs are 0750.
	keyDirs := []string{
		filepath.Join(configDir, "keys"),
		filepath.Join(configDir, "keys", "rotations"),
	}
	for _, dir := range keyDirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
		// MkdirAll is a no-op on an already-existing dir, so tighten an existing
		// keys/ that a prior version created 0750.
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- directory needs the owner execute bit
			return fmt.Errorf("secure directory %s: %w", dir, err)
		}
	}
	dirs := []string{
		filepath.Join(configDir, "servers"),
		filepath.Join(configDir, "templates"),
		filepath.Join(configDir, "logs"),
		filepath.Join(configDir, "logs", "_aggregated"),
		filepath.Join(configDir, "alerts"),
		filepath.Join(configDir, "data"),
		filepath.Join(configDir, "backups"),
		filepath.Join(configDir, "tmp"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	knownHosts := filepath.Join(configDir, "keys", "known_hosts")
	if _, err := os.Stat(knownHosts); os.IsNotExist(err) {
		if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
			return fmt.Errorf("create known_hosts: %w", err)
		}
	}
	return nil
}

func BackupDir(sourceDir, outputPath string) (string, error) {
	if outputPath == "" {
		outputPath = filepath.Join(sourceDir, "backups", "fleet-backup-"+time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return "", fmt.Errorf("create backup output directory: %w", err)
	}
	result, err := Backup(sourceDir, BackupOptions{OutputPath: outputPath})
	if err != nil {
		return "", err
	}
	return result.OutputPath, nil
}

const (
	maxRestoreFileSize    int64 = 512 * 1024 * 1024
	maxRestoreTotalSize   int64 = 2 * 1024 * 1024 * 1024
	maxRestoreTrailerSize int64 = 1 * 1024 * 1024
	maxRestoreMembers           = 100_000
)

type restoreLimits struct {
	fileSize  int64
	totalSize int64
	members   int
}

func defaultRestoreLimits() restoreLimits {
	return restoreLimits{
		fileSize:  maxRestoreFileSize,
		totalSize: maxRestoreTotalSize,
		members:   maxRestoreMembers,
	}
}

func RestoreBackup(inputPath, outputDir string) error {
	return restoreBackupWithLimits(inputPath, outputDir, defaultRestoreLimits(), os.Rename)
}

func restoreBackupWithLimits(inputPath, outputDir string, limits restoreLimits, rename func(string, string) error) error {
	if limits.fileSize <= 0 || limits.totalSize <= 0 || limits.members <= 0 {
		return fmt.Errorf("restore limits must be positive")
	}
	absOutput, err := filepath.Abs(outputDir)
	if err != nil {
		return fmt.Errorf("resolve restore directory: %w", err)
	}
	absOutput = filepath.Clean(absOutput)
	if absOutput == filepath.Clean(filepath.VolumeName(absOutput)+string(filepath.Separator)) {
		return fmt.Errorf("refusing to restore over filesystem root")
	}
	if info, err := os.Lstat(absOutput); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("restore destination must not be a symlink")
		}
		if !info.IsDir() {
			return fmt.Errorf("restore destination is not a directory")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect restore destination: %w", err)
	}

	parent := filepath.Dir(absOutput)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create restore parent: %w", err)
	}
	txnDir, err := os.MkdirTemp(parent, ".fleet-restore-*")
	if err != nil {
		return fmt.Errorf("create restore staging directory: %w", err)
	}
	if err := os.Chmod(txnDir, 0o700); err != nil { // #nosec G302 -- directory is private but requires the owner execute bit for traversal
		_ = os.RemoveAll(txnDir)
		return fmt.Errorf("secure restore staging directory: %w", err)
	}
	defer os.RemoveAll(txnDir)
	stage := filepath.Join(txnDir, "new")
	if err := os.Mkdir(stage, 0o700); err != nil {
		return fmt.Errorf("create restore staging tree: %w", err)
	}
	if err := extractBackupToStage(inputPath, stage, limits); err != nil {
		return err
	}
	if err := syncRestoreTree(stage); err != nil {
		return fmt.Errorf("sync restore staging tree: %w", err)
	}
	if err := commitRestore(stage, absOutput, txnDir, rename); err != nil {
		return err
	}
	return nil
}

func extractBackupToStage(inputPath, stage string, limits restoreLimits) error {
	f, err := os.Open(inputPath) // #nosec G304 -- operator-selected backup input
	if err != nil {
		return fmt.Errorf("open backup: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("open gzip backup: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := make(map[string]struct{})
	var total int64
	members := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			// tar.Reader stops at the tar terminator. Drain a bounded tail so the
			// gzip trailer and late corruption are validated without allowing a
			// concatenated-stream decompression bomb.
			drained, drainErr := io.Copy(io.Discard, io.LimitReader(gz, maxRestoreTrailerSize+1))
			if drainErr != nil {
				return fmt.Errorf("validate backup gzip trailer: %w", drainErr)
			}
			if drained > maxRestoreTrailerSize {
				return fmt.Errorf("backup gzip contains excessive data after tar terminator")
			}
			if err := gz.Close(); err != nil {
				return fmt.Errorf("close gzip backup: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read backup archive: %w", err)
		}
		members++
		if members > limits.members {
			return fmt.Errorf("backup archive exceeds %d member limit", limits.members)
		}
		cleanName, err := safeRestoreMemberName(header.Name)
		if err != nil {
			return err
		}
		if _, duplicate := seen[cleanName]; duplicate {
			return fmt.Errorf("backup archive contains duplicate member %q", header.Name)
		}
		seen[cleanName] = struct{}{}
		target := filepath.Join(stage, filepath.FromSlash(cleanName))
		if !pathWithinBase(stage, target) {
			return fmt.Errorf("backup archive entry %q escapes the restore directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create staged restore directory: %w", err)
			}
			if err := os.Chmod(target, 0o700); err != nil { // #nosec G302 -- directory is private but requires the owner execute bit for traversal
				return fmt.Errorf("secure staged restore directory: %w", err)
			}
		case tar.TypeReg:
			if header.Size < 0 || header.Size > limits.fileSize {
				return fmt.Errorf("backup archive entry %q exceeds %d byte file limit", header.Name, limits.fileSize)
			}
			if header.Size > limits.totalSize-total {
				return fmt.Errorf("backup archive exceeds %d byte aggregate limit", limits.totalSize)
			}
			total += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("create staged restore parent: %w", err)
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600) // #nosec G304 -- target is validated within private staging
			if err != nil {
				return fmt.Errorf("create staged restore file %q: %w", header.Name, err)
			}
			_, copyErr := io.CopyN(out, tr, header.Size)
			if copyErr == nil {
				copyErr = out.Sync()
			}
			closeErr := out.Close()
			if copyErr != nil {
				return fmt.Errorf("restore file %q contents: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close staged restore file %q: %w", header.Name, closeErr)
			}
			mode := os.FileMode(header.Mode & 0o700) // #nosec G115 -- masked to three owner permission bits before conversion
			if mode == 0 {
				mode = 0o600
			}
			if err := os.Chmod(target, mode); err != nil {
				return fmt.Errorf("set staged restore file mode %q: %w", header.Name, err)
			}
		default:
			// In particular, reject symlink and hard-link members rather than
			// attempting to validate or follow their targets.
			return fmt.Errorf("backup archive entry %q has unsupported type %d", header.Name, header.Typeflag)
		}
	}
}

func safeRestoreMemberName(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\\`) {
		return "", fmt.Errorf("backup archive contains unsafe path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(name) {
		return "", fmt.Errorf("backup archive contains unsafe path %q", name)
	}
	return clean, nil
}

func pathWithinBase(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func syncRestoreTree(root string) error {
	var dirs []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir) // #nosec G304 -- path is an operator-selected export or verified controller directory
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func commitRestore(stage, output, txnDir string, rename func(string, string) error) error {
	old := filepath.Join(txnDir, "old")
	_, statErr := os.Lstat(output)
	hadOld := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect live config before commit: %w", statErr)
	}
	if hadOld {
		if err := rename(output, old); err != nil {
			return fmt.Errorf("stage live config for rollback: %w", err)
		}
	}
	if err := rename(stage, output); err != nil {
		if hadOld {
			if rollbackErr := rename(old, output); rollbackErr != nil {
				return fmt.Errorf("commit restored config: %v; rollback failed: %w", err, rollbackErr)
			}
		}
		return fmt.Errorf("commit restored config: %w", err)
	}
	if err := syncDirectory(filepath.Dir(output)); err != nil {
		// Keep the reported-failure contract transactional: put the complete new
		// tree back in staging and restore the untouched old directory.
		if moveErr := rename(output, stage); moveErr != nil {
			return fmt.Errorf("sync restored config parent: %v; prepare rollback failed: %w", err, moveErr)
		}
		if hadOld {
			if rollbackErr := rename(old, output); rollbackErr != nil {
				return fmt.Errorf("sync restored config parent: %v; rollback failed: %w", err, rollbackErr)
			}
		}
		return fmt.Errorf("sync restored config parent: %w", err)
	}
	return nil
}

func WriteExport(path string, export ConfigExport) error {
	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config export: %w", err)
	}
	if path == "" || path == "-" {
		_, err = os.Stdout.Write(append(data, '\n'))
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func ReadExport(path string) (ConfigExport, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-selected export or verified controller directory
	if err != nil {
		return ConfigExport{}, fmt.Errorf("read config export: %w", err)
	}
	var export ConfigExport
	if err := json.Unmarshal(data, &export); err != nil {
		return ConfigExport{}, fmt.Errorf("decode config export: %w", err)
	}
	return export, nil
}

func ValidateAlias(alias string) error {
	if !aliasPattern.MatchString(strings.TrimSpace(alias)) {
		return fmt.Errorf("alias must be 2-8 alphanumeric characters")
	}
	return nil
}
