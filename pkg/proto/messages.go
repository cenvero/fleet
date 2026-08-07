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

	// Binary is a raw byte attachment carried after the JSON envelope rather
	// than base64-encoded inside it — see binaryFrameFlag in codec.go. It is
	// deliberately not a JSON field: the codec moves it on and off the wire.
	// Only ever populated for peers that advertise CapabilityBinaryFrames.
	Binary []byte `json:"-"`
}

// CapabilityBinaryFrames is advertised by an agent that understands binary
// frame attachments, letting the controller ship file chunks as raw bytes
// instead of base64 inside JSON. Absent it, transfers fall back to the original
// encoding, so a new controller keeps working against an older agent.
const CapabilityBinaryFrames = "file.binary-frames"

// CapabilityReverseMultiplex is advertised by an agent whose reverse-mode
// connection accepts additional fleet-rpc channels opened by the controller.
// SSH lets either end open a channel, but a reverse agent historically opened
// one and served only that, so every RPC to it — including every chunk of a file
// transfer — queued behind a single serialised channel. Agents advertising this
// let the controller run reverse transfers with real parallelism. Without it the
// controller keeps to the single-channel behaviour.
const CapabilityReverseMultiplex = "reverse.multiplex"

// BinaryCarrier is implemented by payloads whose bulk bytes can travel as a
// binary frame attachment instead of a base64 JSON field.
type BinaryCarrier interface {
	// TakeBinary removes the bulk bytes from the payload and returns them.
	TakeBinary() []byte
	// PutBinary installs bulk bytes back onto the payload.
	PutBinary([]byte)
}

// BinaryRequester is implemented by request payloads that can ask for their
// response's bulk bytes as a binary frame.
type BinaryRequester interface {
	WantsBinary() bool
}

// PeerWantsBinary reports whether the sender of a request understands binary
// frames. A request that itself arrived framed proves it; otherwise the payload
// may ask explicitly. Anything else is treated as an older peer and answered
// with the original base64-in-JSON encoding.
func PeerWantsBinary[T any](request Envelope, payload T) bool {
	if request.Binary != nil {
		return true
	}
	if req, ok := any(payload).(BinaryRequester); ok {
		return req.WantsBinary()
	}
	if req, ok := any(&payload).(BinaryRequester); ok {
		return req.WantsBinary()
	}
	return false
}

// DetachBinary moves a payload's bulk bytes into the envelope as a binary frame
// when the payload supports it. Returns the envelope unchanged otherwise, so
// callers can apply it unconditionally once a peer's capability is known.
func DetachBinary(env Envelope) Envelope {
	carrier, ok := env.Payload.(BinaryCarrier)
	if !ok {
		return env
	}
	if blob := carrier.TakeBinary(); blob != nil {
		env.Binary = blob
	}
	return env
}

// AttachBinary is the inverse of DetachBinary: it puts a received attachment
// back onto a decoded payload so handlers see a fully-populated struct whether
// or not the bytes travelled in the JSON.
func AttachBinary[T any](payload *T, env Envelope) {
	if env.Binary == nil {
		return
	}
	if carrier, ok := any(payload).(BinaryCarrier); ok {
		carrier.PutBinary(env.Binary)
	}
}

// UnmarshalJSON decodes an envelope while keeping Payload as raw JSON bytes
// instead of materialising it into a map[string]any.
//
// This is a hot path, not a style choice. Payload is `any`, so the default
// decoder built a map[string]any for every message; DecodePayload then had to
// marshal that map back to JSON and unmarshal it a second time to reach the
// concrete struct. For a bulk file chunk that meant four full passes over a
// multi-megabyte base64 string (decode → map → re-encode → decode) and roughly
// 25 MB of garbage per 4 MiB chunk. Holding the raw bytes lets DecodePayload
// unmarshal once, straight into the target type.
//
// The wire format is unchanged, so this is compatible in both directions with
// peers running the previous decoder.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	// envelopeAlias drops the method set, so unmarshalling the shadow struct
	// cannot recurse back into this method.
	type envelopeAlias Envelope
	var shadow struct {
		*envelopeAlias
		// Declared at depth 0 so encoding/json prefers it over the embedded
		// Payload field at depth 1.
		Payload json.RawMessage `json:"payload,omitempty"`
	}
	shadow.envelopeAlias = (*envelopeAlias)(e)
	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}
	if len(shadow.Payload) == 0 {
		e.Payload = nil
		return nil
	}
	e.Payload = shadow.Payload
	return nil
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
	switch p := payload.(type) {
	case nil:
		// Absent payload decodes to the zero value, matching the previous
		// marshal("null") → unmarshal behaviour.
		return decoded, nil
	case json.RawMessage:
		// The common case: an envelope off the wire, whose payload bytes were
		// preserved verbatim by Envelope.UnmarshalJSON. One pass, no re-encode.
		if len(p) == 0 {
			return decoded, nil
		}
		if err := json.Unmarshal(p, &decoded); err != nil {
			return decoded, fmt.Errorf("unmarshal payload: %w", err)
		}
		return decoded, nil
	}
	// Fallback: a live Go value supplied in-process (tests, loopback callers
	// that never touch the wire). Round-trip it through JSON so field mapping
	// stays identical to the wire path.
	data, err := json.Marshal(payload)
	if err != nil {
		return decoded, fmt.Errorf("marshal payload: %w", err)
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return decoded, fmt.Errorf("unmarshal payload: %w", err)
	}
	return decoded, nil
}
