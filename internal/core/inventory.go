// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// InventorySchemaVersion is the stable version of the JSON inventory schema.
// Bump it when the on-disk/--json shape changes incompatibly.
const InventorySchemaVersion = 1

// InventoryItem is the per-server machine-readable inventory record. The JSON
// tags form a stable schema consumed by `fleet inventory --json` and the cache.
type InventoryItem struct {
	Server       string            `json:"server"`
	Hostname     string            `json:"hostname,omitempty"`
	PublicIPs    []string          `json:"public_ips,omitempty"`
	PrivateIPs   []string          `json:"private_ips,omitempty"`
	OS           string            `json:"os,omitempty"`
	OSVersion    string            `json:"os_version,omitempty"`
	Arch         string            `json:"arch,omitempty"`
	CPUCount     int               `json:"cpu_count,omitempty"`
	MemoryTotal  string            `json:"memory_total,omitempty"`
	DiskTotal    string            `json:"disk_total,omitempty"`
	DiskUsed     string            `json:"disk_used,omitempty"`
	ListenPorts  []int             `json:"listen_ports,omitempty"`
	Services     []string          `json:"services,omitempty"`
	AgentPort    int               `json:"agent_port,omitempty"`
	AgentVersion string            `json:"agent_version,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
	Reachable    bool              `json:"reachable"`
	Error        string            `json:"error,omitempty"`
	ProbedAt     time.Time         `json:"probed_at"`
}

// Inventory is the top-level machine-readable document.
type Inventory struct {
	SchemaVersion int             `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	Items         []InventoryItem `json:"items"`
}

// inventoryProber is the minimal surface inventory needs from *App. Defining it
// as an interface keeps Inventory testable without an app.go change and lets the
// CLI pass the live *App directly.
type inventoryProber interface {
	ListServers() ([]ServerRecord, error)
	GetServer(name string) (ServerRecord, error)
	ExecCommand(serverName, command string) (ExecResultLike, error)
}

// ExecResultLike mirrors proto.ExecResult so this file does not need to import
// pkg/proto just for the field shape; the CLI adapter converts.
type ExecResultLike struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// InventoryPath returns the on-disk cache location for a config dir.
func InventoryPath(configDir string) string {
	return filepath.Join(configDir, "data", "inventory.json")
}

// inventoryProbeScript is a single shell pipeline that emits KEY=VALUE lines we
// parse into an InventoryItem. Keeping it one round-trip avoids many ExecCommand
// calls per server. Each probe is fault-tolerant (2>/dev/null, fallbacks).
const inventoryProbeScript = `
echo "HOSTNAME=$(hostname 2>/dev/null)"
if [ -r /etc/os-release ]; then . /etc/os-release 2>/dev/null; echo "OS=${NAME}"; echo "OSVERSION=${VERSION_ID:-$VERSION}"; fi
echo "ARCH=$(uname -m 2>/dev/null)"
echo "CPU=$(nproc 2>/dev/null)"
echo "MEM=$(free -h 2>/dev/null | awk '/^Mem:/{print $2}')"
echo "DISKTOTAL=$(df -h / 2>/dev/null | awk 'NR==2{print $2}')"
echo "DISKUSED=$(df -h / 2>/dev/null | awk 'NR==2{print $3}')"
echo "IPS=$(ip -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | tr '\n' ' ')"
PORTS=$(ss -tlnH 2>/dev/null | awk '{print $4}' | sed 's/.*://' | sort -un | tr '\n' ' ')
if [ -z "$PORTS" ]; then PORTS=$(netstat -tln 2>/dev/null | awk 'NR>2{print $4}' | sed 's/.*://' | sort -un | tr '\n' ' '); fi
echo "PORTS=$PORTS"
echo "SERVICES=$(systemctl list-units --type=service --state=running --no-legend --no-pager 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
`

// privateCIDRs are the RFC1918 / loopback / link-local ranges used to classify
// an address as private vs public.
var privatePrefixes = []string{
	"10.", "192.168.", "127.", "169.254.",
	"172.16.", "172.17.", "172.18.", "172.19.", "172.20.", "172.21.", "172.22.", "172.23.",
	"172.24.", "172.25.", "172.26.", "172.27.", "172.28.", "172.29.", "172.30.", "172.31.",
	"::1", "fe80:", "fc", "fd",
}

func isPrivateIP(ip string) bool {
	low := strings.ToLower(ip)
	for _, p := range privatePrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// BuildInventory probes every server (or just `only` when non-empty) and returns
// a freshly built Inventory. Tags come from the supplied TagStore. Probes run
// concurrently; an unreachable server yields an item with Reachable=false and an
// Error rather than failing the whole run.
func BuildInventory(prober inventoryProber, tags *TagStore, only string) (Inventory, error) {
	var records []ServerRecord
	if only != "" {
		rec, err := prober.GetServer(only)
		if err != nil {
			return Inventory{}, err
		}
		records = []ServerRecord{rec}
	} else {
		var err error
		records, err = prober.ListServers()
		if err != nil {
			return Inventory{}, err
		}
	}

	allTags := map[string]map[string]string{}
	if tags != nil {
		allTags = tags.AllTags()
	}

	items := make([]InventoryItem, len(records))
	var wg sync.WaitGroup
	for i, rec := range records {
		wg.Add(1)
		go func(i int, rec ServerRecord) {
			defer wg.Done()
			items[i] = probeServer(prober, rec, allTags[rec.Name])
		}(i, rec)
	}
	wg.Wait()

	sort.Slice(items, func(a, b int) bool { return items[a].Server < items[b].Server })
	return Inventory{
		SchemaVersion: InventorySchemaVersion,
		GeneratedAt:   time.Now().UTC(),
		Items:         items,
	}, nil
}

// probeServer runs the probe script on one server and parses the result. Static
// fields (agent port/version, last-observed os/arch) come from the record so an
// unreachable server still yields useful data.
func probeServer(prober inventoryProber, rec ServerRecord, tags map[string]string) InventoryItem {
	item := InventoryItem{
		Server:       rec.Name,
		AgentPort:    rec.Port,
		AgentVersion: rec.Observed.AgentVersion,
		OS:           rec.Observed.OS,
		Arch:         rec.Observed.Arch,
		Hostname:     rec.Observed.NodeName,
		Tags:         tags,
		ProbedAt:     time.Now().UTC(),
	}

	res, err := prober.ExecCommand(rec.Name, inventoryProbeScript)
	if err != nil {
		item.Reachable = false
		item.Error = err.Error()
		return item
	}
	item.Reachable = true
	parseProbeOutput(&item, res.Stdout)
	return item
}

// parseProbeOutput fills an InventoryItem from the KEY=VALUE probe output.
func parseProbeOutput(item *InventoryItem, stdout string) {
	for _, line := range strings.Split(stdout, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "HOSTNAME":
			if val != "" {
				item.Hostname = val
			}
		case "OS":
			if val != "" {
				item.OS = val
			}
		case "OSVERSION":
			if val != "" {
				item.OSVersion = val
			}
		case "ARCH":
			if val != "" {
				item.Arch = val
			}
		case "CPU":
			if n, err := strconv.Atoi(val); err == nil {
				item.CPUCount = n
			}
		case "MEM":
			item.MemoryTotal = val
		case "DISKTOTAL":
			item.DiskTotal = val
		case "DISKUSED":
			item.DiskUsed = val
		case "IPS":
			for _, ip := range strings.Fields(val) {
				if isPrivateIP(ip) {
					item.PrivateIPs = append(item.PrivateIPs, ip)
				} else {
					item.PublicIPs = append(item.PublicIPs, ip)
				}
			}
		case "PORTS":
			seen := map[int]bool{}
			for _, p := range strings.Fields(val) {
				if n, err := strconv.Atoi(p); err == nil && n > 0 && !seen[n] {
					seen[n] = true
					item.ListenPorts = append(item.ListenPorts, n)
				}
			}
			sort.Ints(item.ListenPorts)
		case "SERVICES":
			for _, s := range strings.Fields(val) {
				item.Services = append(item.Services, s)
			}
		}
	}
}

// LoadInventory reads the cached inventory from disk. Returns os.ErrNotExist
// (wrapped) when no cache exists yet.
func LoadInventory(configDir string) (Inventory, error) {
	data, err := os.ReadFile(InventoryPath(configDir))
	if err != nil {
		return Inventory{}, err
	}
	var inv Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return Inventory{}, fmt.Errorf("decode inventory cache: %w", err)
	}
	return inv, nil
}

// SaveInventory writes the inventory to the cache atomically (0600).
func SaveInventory(configDir string, inv Inventory) error {
	path := InventoryPath(configDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inventory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".inventory-*.json")
	if err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write inventory: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write inventory: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write inventory: %w", err)
	}
	return nil
}
