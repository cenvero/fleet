// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

const CurrentProtocolVersion = 1

type EnvelopeType string

const (
	EnvelopeTypeRequest  EnvelopeType = "request"
	EnvelopeTypeResponse EnvelopeType = "response"
	EnvelopeTypeEvent    EnvelopeType = "event"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Retry   bool   `json:"retry,omitempty"`
}

type Envelope struct {
	Type            EnvelopeType `json:"type"`
	ProtocolVersion int          `json:"protocol_version"`
	RequestID       string       `json:"request_id,omitempty"`
	Action          string       `json:"action,omitempty"`
	Timestamp       time.Time    `json:"timestamp"`
	Capabilities    []string     `json:"capabilities,omitempty"`
	Payload         any          `json:"payload,omitempty"`
	Error           *Error       `json:"error,omitempty"`
}

type HelloPayload struct {
	NodeName     string   `json:"node_name"`
	ControllerID string   `json:"controller_id,omitempty"`
	AgentVersion string   `json:"agent_version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Transport    string   `json:"transport"`
	Capabilities []string `json:"capabilities"`
}

type ServiceActionPayload struct {
	Server  string `json:"server"`
	Service string `json:"service"`
	Action  string `json:"action"`
}

type ServiceInfo struct {
	Name        string `json:"name"`
	LoadState   string `json:"load_state,omitempty"`
	ActiveState string `json:"active_state,omitempty"`
	SubState    string `json:"sub_state,omitempty"`
	Description string `json:"description,omitempty"`
}

type MetricsPayload struct {
	Server string `json:"server"`
}

type MetricsReplayResult struct {
	Snapshots []MetricsSnapshot `json:"snapshots"`
}

type MetricsSnapshot struct {
	Timestamp        time.Time `json:"timestamp"`
	Hostname         string    `json:"hostname,omitempty"`
	CPUPercent       float64   `json:"cpu_percent"`
	MemoryPercent    float64   `json:"memory_percent"`
	MemoryUsedBytes  uint64    `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes uint64    `json:"memory_total_bytes,omitempty"`
	DiskPath         string    `json:"disk_path,omitempty"`
	DiskPercent      float64   `json:"disk_percent"`
	DiskUsedBytes    uint64    `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes   uint64    `json:"disk_total_bytes,omitempty"`
	Load1            float64   `json:"load1,omitempty"`
	Load5            float64   `json:"load5,omitempty"`
	Load15           float64   `json:"load15,omitempty"`
	UptimeSeconds    uint64    `json:"uptime_seconds,omitempty"`
	ProcessCount     uint64    `json:"process_count,omitempty"`
}

type FirewallInfo struct {
	Enabled   bool     `json:"enabled"`
	Rules     []string `json:"rules,omitempty"`
	OpenPorts []int    `json:"open_ports,omitempty"`
}

type LogReadPayload struct {
	Server    string `json:"server"`
	Service   string `json:"service,omitempty"`
	Path      string `json:"path"`
	Search    string `json:"search,omitempty"`
	TailLines int    `json:"tail_lines,omitempty"`
	Follow    bool   `json:"follow,omitempty"`
}

type LogLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

type LogReadResult struct {
	Path      string    `json:"path"`
	Lines     []LogLine `json:"lines"`
	Truncated bool      `json:"truncated,omitempty"`
}

type UpdateApplyPayload struct {
	ManifestURL string `json:"manifest_url"`
	Channel     string `json:"channel"`
	ServiceName string `json:"service_name,omitempty"`
}

type UpdateApplyResult struct {
	Channel           string `json:"channel"`
	CurrentVersion    string `json:"current_version"`
	Version           string `json:"version"`
	BackupPath        string `json:"backup_path,omitempty"`
	RollbackState     string `json:"rollback_state,omitempty"`
	ReleaseNotesURL   string `json:"release_notes_url,omitempty"`
	Applied           bool   `json:"applied"`
	SHA256Verified    bool   `json:"sha256_verified"`
	SignatureVerified bool   `json:"signature_verified"`
	RestartScheduled  bool   `json:"restart_scheduled,omitempty"`
	ServiceName       string `json:"service_name,omitempty"`
}

type FirewallRulePayload struct {
	Server string `json:"server"`
	Rule   string `json:"rule"`
}

type PortActionPayload struct {
	Server string `json:"server"`
	Port   int    `json:"port"`
	Open   bool   `json:"open"`
}

type AuthorizedKeysPayload struct {
	AddKeys    []string `json:"add_keys,omitempty"`
	RemoveKeys []string `json:"remove_keys,omitempty"`
}

type AuthorizedKeysResult struct {
	Keys []string `json:"keys"`
}

type ControllerKnownHostsPayload struct {
	Address    string   `json:"address,omitempty"`
	AddKeys    []string `json:"add_keys,omitempty"`
	RemoveKeys []string `json:"remove_keys,omitempty"`
}

type ControllerKnownHostsResult struct {
	Address      string   `json:"address"`
	EntryCount   int      `json:"entry_count"`
	Fingerprints []string `json:"fingerprints,omitempty"`
}

type ExecPayload struct {
	Command string `json:"command"`
}

type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func DecodeHelloPayload(payload any) (HelloPayload, error) {
	return DecodePayload[HelloPayload](payload)
}

func DecodePayload[T any](payload any) (T, error) {
	var decoded T
	data, err := json.Marshal(payload)
	if err != nil {
		return decoded, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return decoded, fmt.Errorf("unmarshal payload: %w", err)
	}
	return decoded, nil
}
