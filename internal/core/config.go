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
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
)

var aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9]{2,8}$`)

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
	if err := c.Database.Validate(); err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	return nil
}

func SaveConfig(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config file: %w", err)
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(cfg)
}

func ConfigPath(configDir string) string {
	return filepath.Join(configDir, "config.toml")
}

func EnsureLayout(configDir string) error {
	dirs := []string{
		filepath.Join(configDir, "keys"),
		filepath.Join(configDir, "keys", "rotations"),
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
		if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return "", fmt.Errorf("create backup output directory: %w", err)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return "", fmt.Errorf("create backup file: %w", err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filepath.Clean(path) == filepath.Clean(outputPath) {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil || rel == "." {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = rel
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("archive config directory: %w", err)
	}
	return outputPath, nil
}

func RestoreBackup(inputPath, outputDir string) error {
	f, err := os.Open(inputPath)
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
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read backup archive: %w", err)
		}
		target := filepath.Join(outputDir, filepath.Clean(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return fmt.Errorf("create restore directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create restore parent: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return fmt.Errorf("create restore file: %w", err)
			}
			if _, err := io.Copy(file, tr); err != nil {
				file.Close()
				return fmt.Errorf("restore file contents: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close restore file: %w", err)
			}
		}
	}
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
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func ReadExport(path string) (ConfigExport, error) {
	data, err := os.ReadFile(path)
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
