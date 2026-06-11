// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// FL-021 — typed service management (structured).
//
// The existing `fleet service` command (in root.go) tracks services that are
// recorded against a server definition and drives them through the agent RPC.
// This command complements it with ad-hoc, structured systemd control for an
// arbitrary unit, parsed directly from `systemctl`/`journalctl` over
// App.ExecCommand. It does not require the unit to be tracked first.
//
//	fleet svc <server> status  <unit> [--json]
//	fleet svc <server> restart <unit>
//	fleet svc <server> start   <unit>
//	fleet svc <server> stop    <unit>
//	fleet svc <server> enable  <unit>
//	fleet svc <server> disable <unit>
//
// newServiceStatusCommand is exported through this file so root.go can register
// it with root.AddCommand(newServiceStatusCommand(&configDir)).

// serviceStatus is the structured shape returned by `fleet svc <server> status`.
//
//	Active:  active | inactive | failed | activating | ... (systemd ActiveState)
//	Enabled: enabled | disabled | static | ...             (systemd UnitFileState)
//	Failed:  true when ActiveState == failed
//	Since:   ActiveEnterTimestamp
type serviceStatus struct {
	Server      string   `json:"server"`
	Unit        string   `json:"unit"`
	Active      string   `json:"active"`
	Enabled     string   `json:"enabled"`
	Failed      bool     `json:"failed"`
	Since       string   `json:"since,omitempty"`
	Description string   `json:"description,omitempty"`
	SubState    string   `json:"sub_state,omitempty"`
	MainPID     string   `json:"main_pid,omitempty"`
	LogLines    []string `json:"log_lines,omitempty"`
}

// validUnitName rejects unit names that could smuggle extra shell words into the
// remote command line. ExecCommand runs a single command string on the agent, so
// the unit is interpolated as an argument — keep it to characters systemd allows.
func validUnitName(unit string) error {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return fmt.Errorf("unit name is required")
	}
	if len(unit) > 256 {
		return fmt.Errorf("unit name too long")
	}
	for _, r := range unit {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '@', r == ':', r == '\\':
		default:
			return fmt.Errorf("invalid unit name %q: contains %q", unit, string(r))
		}
	}
	return nil
}

func newServiceStatusCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "svc <server>",
		Short: "Structured systemd control for an arbitrary unit",
		Long: `Inspect and control a systemd unit on a server with structured output.

Unlike 'fleet service', this works on any unit without tracking it first; it
parses systemctl/journalctl directly over the live agent transport.

  fleet svc <server> status  <unit> [--json]
  fleet svc <server> restart <unit>
  fleet svc <server> start   <unit>
  fleet svc <server> stop    <unit>
  fleet svc <server> enable  <unit>
  fleet svc <server> disable <unit>`,
		Args: cobra.MinimumNArgs(1),
	}

	statusJSON := false
	statusCmd := &cobra.Command{
		Use:          "status <server> <unit>",
		Short:        "Show active/enabled/failed state plus recent log lines",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			server, unit := args[0], args[1]
			if err := validUnitName(unit); err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			status, err := collectServiceStatus(app, server, unit)
			if err != nil {
				return err
			}
			if statusJSON {
				return writeJSON(cmd, status)
			}
			return writeServiceStatus(cmd, status)
		},
	}
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "emit structured JSON")
	cmd.AddCommand(statusCmd)

	for _, action := range []string{"restart", "start", "stop", "enable", "disable"} {
		action := action
		cmd.AddCommand(&cobra.Command{
			Use:          action + " <server> <unit>",
			Short:        strings.Title(action) + " a systemd unit",
			Args:         cobra.ExactArgs(2),
			SilenceUsage: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				server, unit := args[0], args[1]
				if err := validUnitName(unit); err != nil {
					return err
				}
				app, err := openApp(*configDir)
				if err != nil {
					return err
				}
				defer app.Close()
				if err := runSystemctlAction(app, server, action, unit); err != nil {
					return err
				}
				// Report the resulting state so the caller sees the effect.
				status, err := collectServiceStatus(app, server, unit)
				if err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "%s %s on %s: ok\n", action, unit, server)
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s on %s: active=%s enabled=%s\n", action, unit, server, status.Active, status.Enabled)
				return nil
			},
		})
	}

	return cmd
}

// runSystemctlAction issues `systemctl <action> <unit>` on the server and fails
// on a non-zero exit, surfacing stderr (e.g. permission denied).
func runSystemctlAction(app *core.App, server, action, unit string) error {
	cmd := fmt.Sprintf("systemctl %s %s", action, shellQuote(unit))
	res, err := app.ExecCommand(server, cmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("systemctl %s exited %d", action, res.ExitCode)
		}
		return fmt.Errorf("%s %s on %s failed: %s", action, unit, server, msg)
	}
	return nil
}

// collectServiceStatus parses systemctl show + is-active/is-enabled and tails the
// journal for the unit, returning a structured snapshot.
func collectServiceStatus(app *core.App, server, unit string) (serviceStatus, error) {
	status := serviceStatus{Server: server, Unit: unit}

	// `systemctl show` is the most reliable single source: it never errors on a
	// missing/inactive unit, returning Key=Value lines we can parse.
	showCmd := fmt.Sprintf("systemctl show %s --property=ActiveState,SubState,UnitFileState,Description,MainPID,ActiveEnterTimestamp --no-pager", shellQuote(unit))
	showRes, err := app.ExecCommand(server, showCmd)
	if err != nil {
		return status, err
	}
	props := parseShowProperties(showRes.Stdout)
	status.Active = props["ActiveState"]
	status.SubState = props["SubState"]
	status.Enabled = props["UnitFileState"]
	status.Description = props["Description"]
	status.MainPID = props["MainPID"]
	status.Since = props["ActiveEnterTimestamp"]

	// Fall back to is-active / is-enabled when `show` returned nothing useful
	// (older systemd, or a unit with a non-standard provider).
	if status.Active == "" {
		if res, err := app.ExecCommand(server, fmt.Sprintf("systemctl is-active %s", shellQuote(unit))); err == nil {
			status.Active = strings.TrimSpace(res.Stdout)
		}
	}
	if status.Enabled == "" {
		if res, err := app.ExecCommand(server, fmt.Sprintf("systemctl is-enabled %s", shellQuote(unit))); err == nil {
			status.Enabled = strings.TrimSpace(res.Stdout)
		}
	}
	status.Failed = status.Active == "failed"

	// Last few log lines for quick triage.
	if res, err := app.ExecCommand(server, fmt.Sprintf("journalctl -u %s -n 5 --no-pager", shellQuote(unit))); err == nil {
		for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			status.LogLines = append(status.LogLines, line)
		}
	}

	return status, nil
}

// parseShowProperties turns `Key=Value` output from `systemctl show` into a map.
func parseShowProperties(out string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := line[:idx]
		val := strings.TrimSpace(line[idx+1:])
		props[key] = val
	}
	return props
}

func writeServiceStatus(cmd *cobra.Command, status serviceStatus) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	active := status.Active
	if active == "" {
		active = "unknown"
	}
	if status.Failed {
		active += " (failed)"
	}
	enabled := status.Enabled
	if enabled == "" {
		enabled = "unknown"
	}
	fmt.Fprintf(w, "UNIT\t%s\n", status.Unit)
	fmt.Fprintf(w, "SERVER\t%s\n", status.Server)
	fmt.Fprintf(w, "ACTIVE\t%s\n", active)
	fmt.Fprintf(w, "ENABLED\t%s\n", enabled)
	if status.SubState != "" {
		fmt.Fprintf(w, "SUBSTATE\t%s\n", status.SubState)
	}
	if status.Description != "" {
		fmt.Fprintf(w, "DESCRIPTION\t%s\n", status.Description)
	}
	if status.MainPID != "" && status.MainPID != "0" {
		fmt.Fprintf(w, "MAIN PID\t%s\n", status.MainPID)
	}
	if status.Since != "" {
		fmt.Fprintf(w, "SINCE\t%s\n", status.Since)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(status.LogLines) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "\nRECENT LOGS")
		for _, line := range status.LogLines {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", line)
		}
	}
	return nil
}

// shellQuote single-quotes a value for safe embedding in a remote /bin/sh command.
// validUnitName already rejects quotes and most metacharacters; this is defense
// in depth for the command string passed to the agent.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
