// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// inventoryAppAdapter adapts *core.App to core.inventoryProber, converting
// proto.ExecResult to core.ExecResultLike so core/inventory.go needs no
// pkg/proto import.
type inventoryAppAdapter struct{ app *core.App }

func (a inventoryAppAdapter) ListServers() ([]core.ServerRecord, error) { return a.app.ListServers() }

func (a inventoryAppAdapter) GetServer(name string) (core.ServerRecord, error) {
	return a.app.GetServer(name)
}

func (a inventoryAppAdapter) ExecCommand(server, command string) (core.ExecResultLike, error) {
	res, err := a.app.ExecCommand(server, command)
	if err != nil {
		return core.ExecResultLike{}, err
	}
	return core.ExecResultLike{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// newInventoryCommand builds `fleet inventory`.
//
//	fleet inventory [--json] [--refresh] [server]
//
// Default reads the cache (probing if absent); --refresh always re-probes and
// rewrites the cache; --json prints the stable schema; otherwise a table.
func newInventoryCommand(configDir *string) *cobra.Command {
	var asJSON, refresh bool
	cmd := &cobra.Command{
		Use:   "inventory [server]",
		Short: "Machine-readable inventory of the fleet (hostname, IPs, OS, resources, ports, services, tags)",
		Long: "Collect a per-server inventory: hostname, public/private IPs, OS/version, arch,\n" +
			"CPU/memory/disk, listening ports, running services, agent port/version, and tags.\n\n" +
			"Results are cached at <config>/data/inventory.json. By default the cache is shown\n" +
			"(probing once if it doesn't exist yet); --refresh re-probes every server over the\n" +
			"agent channel and rewrites the cache. --json emits the stable schema.\n\n" +
			"Examples:\n" +
			"  fleet inventory                 # table from cache (probe if empty)\n" +
			"  fleet inventory --refresh       # re-probe all servers\n" +
			"  fleet inventory --json web-01   # one server, JSON",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			only := ""
			if len(args) == 1 {
				only = args[0]
			}

			inv, err := loadOrBuildInventory(*configDir, only, refresh)
			if err != nil {
				return err
			}

			// When a single server is requested but we read a full cache, narrow it.
			if only != "" {
				inv = filterInventory(inv, only)
				if len(inv.Items) == 0 {
					return fmt.Errorf("server %q not found in inventory — try --refresh", only)
				}
			}

			if asJSON {
				return writeJSON(cmd, inv)
			}
			return writeInventoryTable(cmd, inv)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the stable JSON inventory schema")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "re-probe servers and rewrite the cache")
	return cmd
}

// loadOrBuildInventory returns cached inventory, or probes when --refresh is set
// or the cache is missing. A fresh probe is always written back to the cache.
func loadOrBuildInventory(configDir, only string, refresh bool) (core.Inventory, error) {
	if !refresh {
		inv, err := core.LoadInventory(configDir)
		if err == nil {
			return inv, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return core.Inventory{}, err
		}
		// Cache absent: fall through to a probe.
	}

	app, err := openApp(configDir)
	if err != nil {
		return core.Inventory{}, err
	}
	defer app.Close()

	store := core.NewTagStore(configDir)
	inv, err := core.BuildInventory(inventoryAppAdapter{app: app}, store, only)
	if err != nil {
		return core.Inventory{}, err
	}

	// When probing a single server, merge into the existing cache so we don't
	// drop the other servers' records.
	toSave := inv
	if only != "" {
		if cached, lerr := core.LoadInventory(configDir); lerr == nil {
			toSave = mergeInventory(cached, inv)
		}
	}
	if err := core.SaveInventory(configDir, toSave); err != nil {
		return core.Inventory{}, err
	}
	return inv, nil
}

// filterInventory keeps only the named server.
func filterInventory(inv core.Inventory, server string) core.Inventory {
	out := inv
	out.Items = nil
	for _, it := range inv.Items {
		if it.Server == server {
			out.Items = append(out.Items, it)
		}
	}
	return out
}

// mergeInventory overlays fresh items onto a cached inventory by server name.
func mergeInventory(base, fresh core.Inventory) core.Inventory {
	byName := map[string]core.InventoryItem{}
	for _, it := range base.Items {
		byName[it.Server] = it
	}
	for _, it := range fresh.Items {
		byName[it.Server] = it
	}
	merged := fresh // keep fresh schema version + generated-at
	merged.Items = make([]core.InventoryItem, 0, len(byName))
	for _, it := range byName {
		merged.Items = append(merged.Items, it)
	}
	sort.Slice(merged.Items, func(a, b int) bool { return merged.Items[a].Server < merged.Items[b].Server })
	return merged
}

func writeInventoryTable(cmd *cobra.Command, inv core.Inventory) error {
	out := cmd.OutOrStdout()
	if len(inv.Items) == 0 {
		fmt.Fprintln(out, "no servers in inventory")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SERVER\tHOSTNAME\tOS\tARCH\tCPU\tMEM\tDISK\tPUBLIC IP\tPORTS\tAGENT\tTAGS"); err != nil {
		return err
	}
	for _, it := range inv.Items {
		hostname := dash(it.Hostname)
		osStr := strings.TrimSpace(it.OS + " " + it.OSVersion)
		if !it.Reachable {
			osStr = "unreachable"
		}
		disk := dash(it.DiskTotal)
		if it.DiskUsed != "" && it.DiskTotal != "" {
			disk = it.DiskUsed + "/" + it.DiskTotal
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			it.Server,
			hostname,
			dash(osStr),
			dash(it.Arch),
			intDash(it.CPUCount),
			dash(it.MemoryTotal),
			disk,
			dash(strings.Join(it.PublicIPs, ",")),
			dash(joinPorts(it.ListenPorts)),
			dash(it.AgentVersion),
			dash(formatTags(it.Tags)),
		); err != nil {
			return err
		}
	}
	return w.Flush()
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func intDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func joinPorts(ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}
