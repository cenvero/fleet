// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/pkg/proto"
)

type Config struct {
	SchemaVersion int                  `toml:"schema_version" json:"schema_version"`
	ProductName   string               `toml:"product_name" json:"product_name"`
	Domain        string               `toml:"domain" json:"domain"`
	Alias         string               `toml:"alias" json:"alias"`
	InstanceID    string               `toml:"instance_id" json:"instance_id"`
	ConfigDir     string               `toml:"config_dir" json:"config_dir"`
	DefaultMode   transport.Mode       `toml:"default_transport_mode" json:"default_transport_mode"`
	ManifestURL   string               `toml:"manifest_url" json:"manifest_url"`
	InitializedAt time.Time            `toml:"initialized_at" json:"initialized_at"`
	Operator      string               `toml:"operator" json:"operator"`
	Crypto        CryptoConfig         `toml:"crypto" json:"crypto"`
	Updates       UpdateConfig         `toml:"updates" json:"updates"`
	Database      store.DatabaseConfig `toml:"database" json:"database"`
	Runtime       RuntimeConfig        `toml:"runtime" json:"runtime"`
}

type CryptoConfig struct {
	Algorithm           string `toml:"algorithm" json:"algorithm"`
	PassphraseProtected bool   `toml:"passphrase_protected" json:"passphrase_protected"`
	PrimaryKey          string `toml:"primary_key" json:"primary_key"`
	KnownHostsPath      string `toml:"known_hosts_path" json:"known_hosts_path"`
	RotationDirectory   string `toml:"rotation_directory" json:"rotation_directory"`
}

type UpdateConfig struct {
	Channel string        `toml:"channel" json:"channel"`
	Policy  update.Policy `toml:"policy" json:"policy"`
}

type RuntimeConfig struct {
	ListenAddress         string `toml:"listen_address" json:"listen_address"`
	ControlAddress        string `toml:"control_address" json:"control_address"`
	DataDir               string `toml:"data_dir" json:"data_dir"`
	LogDir                string `toml:"log_dir" json:"log_dir"`
	AggregatedLogDir      string `toml:"aggregated_log_dir" json:"aggregated_log_dir"`
	AggregatedLogMaxSize  int64  `toml:"aggregated_log_max_size" json:"aggregated_log_max_size"`
	AggregatedLogMaxFiles int    `toml:"aggregated_log_max_files" json:"aggregated_log_max_files"`
	AggregatedLogMaxAge   string `toml:"aggregated_log_max_age" json:"aggregated_log_max_age"`
	AlertNotifyCooldown   string `toml:"alert_notify_cooldown" json:"alert_notify_cooldown"`
	MetricsPollInterval   string `toml:"metrics_poll_interval" json:"metrics_poll_interval"`
	DesktopNotifications  bool   `toml:"desktop_notifications" json:"desktop_notifications"`
}

type Status struct {
	Initialized     bool              `json:"initialized"`
	ConfigDir       string            `json:"config_dir"`
	ProductName     string            `json:"product_name"`
	Version         string            `json:"version"`
	DefaultMode     transport.Mode    `json:"default_mode"`
	Alias           string            `json:"alias"`
	ServerCount     int               `json:"server_count"`
	Channel         string            `json:"channel"`
	Policy          update.Policy     `json:"policy"`
	DatabaseBackend store.Backend     `json:"database_backend"`
	Fingerprints    map[string]string `json:"fingerprints"`
}

type DashboardSummary struct {
	OnlineServers   int `json:"online_servers"`
	OfflineServers  int `json:"offline_servers"`
	CriticalAlerts  int `json:"critical_alerts"`
	WarningAlerts   int `json:"warning_alerts"`
	InfoAlerts      int `json:"info_alerts"`
	MonitoredAlerts int `json:"monitored_alerts"`
}

type DashboardSnapshot struct {
	Status            Status             `json:"status"`
	Summary           DashboardSummary   `json:"summary"`
	Servers           []ServerRecord     `json:"servers"`
	CachedLogs        []CachedLogPreview `json:"cached_logs"`
	RecentAlerts      []alerts.Alert     `json:"recent_alerts"`
	RecentAudit       []logs.AuditEntry  `json:"recent_audit"`
	Templates         []string           `json:"templates"`
	RollbackAvailable bool               `json:"rollback_available"`
	GeneratedAt       time.Time          `json:"generated_at"`
}

type CachedLogPreview struct {
	Server    string          `json:"server"`
	Service   string          `json:"service"`
	Path      string          `json:"path"`
	Lines     []proto.LogLine `json:"lines,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Available bool            `json:"available"`
}

type ServerRecord struct {
	Name         string                `toml:"name" json:"name"`
	Address      string                `toml:"address" json:"address"`
	Mode         transport.Mode        `toml:"mode" json:"mode"`
	Port         int                   `toml:"port" json:"port"`
	User         string                `toml:"user" json:"user"`
	KeyPath      string                `toml:"key_path,omitempty" json:"key_path,omitempty"`
	Agent        AgentInstall          `toml:"agent" json:"agent"`
	Capabilities []string              `toml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Observed     ServerObservation     `toml:"observed" json:"observed"`
	Services     []ServiceRecord       `toml:"services,omitempty" json:"services,omitempty"`
	Metrics      proto.MetricsSnapshot `toml:"metrics" json:"metrics"`
	OpenPorts    []int                 `toml:"open_ports,omitempty" json:"open_ports,omitempty"`
	Firewall     FirewallState         `toml:"firewall" json:"firewall"`
	LastTemplate string                `toml:"last_template,omitempty" json:"last_template,omitempty"`
	CreatedAt    time.Time             `toml:"created_at" json:"created_at"`
	UpdatedAt    time.Time             `toml:"updated_at" json:"updated_at"`
}

type AgentInstall struct {
	Managed     bool      `toml:"managed" json:"managed"`
	BinaryPath  string    `toml:"binary_path,omitempty" json:"binary_path,omitempty"`
	ServiceName string    `toml:"service_name,omitempty" json:"service_name,omitempty"`
	UpdatedAt   time.Time `toml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

type ServerObservation struct {
	Reachable          bool      `toml:"reachable" json:"reachable"`
	LastSeen           time.Time `toml:"last_seen,omitempty" json:"last_seen,omitempty"`
	LastError          string    `toml:"last_error,omitempty" json:"last_error,omitempty"`
	NodeName           string    `toml:"node_name,omitempty" json:"node_name,omitempty"`
	AgentVersion       string    `toml:"agent_version,omitempty" json:"agent_version,omitempty"`
	OS                 string    `toml:"os,omitempty" json:"os,omitempty"`
	Arch               string    `toml:"arch,omitempty" json:"arch,omitempty"`
	Transport          string    `toml:"transport,omitempty" json:"transport,omitempty"`
	HostKeyFingerprint string    `toml:"host_key_fingerprint,omitempty" json:"host_key_fingerprint,omitempty"`
}

type ServiceRecord struct {
	Name        string `toml:"name" json:"name"`
	LogPath     string `toml:"log_path,omitempty" json:"log_path,omitempty"`
	Critical    bool   `toml:"critical" json:"critical"`
	LastAction  string `toml:"last_action,omitempty" json:"last_action,omitempty"`
	LoadState   string `toml:"load_state,omitempty" json:"load_state,omitempty"`
	ActiveState string `toml:"active_state,omitempty" json:"active_state,omitempty"`
	SubState    string `toml:"sub_state,omitempty" json:"sub_state,omitempty"`
	Description string `toml:"description,omitempty" json:"description,omitempty"`
}

type FirewallState struct {
	Enabled bool     `toml:"enabled" json:"enabled"`
	Rules   []string `toml:"rules,omitempty" json:"rules,omitempty"`
}

type ConfigExport struct {
	Config  Config         `json:"config"`
	Servers []ServerRecord `json:"servers"`
}

type InitOptions struct {
	ConfigDir       string
	Alias           string
	DefaultMode     transport.Mode
	CryptoAlgorithm string
	Passphrase      string
	UpdateChannel   string
	UpdatePolicy    update.Policy
	DatabaseBackend store.Backend
	DatabaseDSN     string
	Operator        string
	ExecutablePath  string
}

type InitResult struct {
	Config     Config
	ConfigPath string
	Keys       []string
}

type BootstrapOptions struct {
	LoginUser         string
	LoginPort         int
	LoginKeyPath      string
	AgentBinaryPath   string
	AgentListenAddr   string
	ControllerAddress string
	ServiceName       string
	AcceptNewHostKey  bool
	UseSudo           bool
	PrintScript       bool
}

type BootstrapResult struct {
	Server            string         `json:"server"`
	LoginAddress      string         `json:"login_address"`
	LoginUser         string         `json:"login_user"`
	LoginPort         int            `json:"login_port"`
	Mode              transport.Mode `json:"mode"`
	AgentBinaryPath   string         `json:"agent_binary_path"`
	AgentListenAddr   string         `json:"agent_listen_addr,omitempty"`
	ControllerAddress string         `json:"controller_address,omitempty"`
	ServiceName       string         `json:"service_name"`
	Executed          bool           `json:"executed"`
	ServiceUnit       string         `json:"service_unit"`
	Script            string         `json:"script"`
}
