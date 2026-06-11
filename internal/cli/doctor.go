// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// FL-003 — `fleet doctor <server>`.
//
// Runs a fixed health checklist against a managed server over the agent channel
// (App.ExecCommand) and prints each check as ok/warn/fail. The check logic lives
// in core.RunDoctor, which is driven by a tiny exec function so it can be unit
// tested without an *App. This file is just the cobra wiring + presentation.
//
// newDoctorCommand is exported so root.go can register it with
// root.AddCommand(newDoctorCommand(&configDir)).

// doctorExecAdapter binds a server name to App.ExecCommand and converts
// proto.ExecResult to core.ExecResultLike, matching the exec function shape
// core.RunDoctor expects.
func doctorExecAdapter(app *core.App, server string) func(command string) (core.ExecResultLike, error) {
	return func(command string) (core.ExecResultLike, error) {
		res, err := app.ExecCommand(server, command)
		if err != nil {
			return core.ExecResultLike{}, err
		}
		return core.ExecResultLike{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
	}
}

func newDoctorCommand(configDir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor <server>",
		Short: "Run a health checklist against a server (agent, ports, disk, swap, reboot, clock)",
		Long: "Run a fixed set of health checks against a managed server over the agent\n" +
			"channel and report each as ok/warn/fail:\n\n" +
			"  • agent online        a trivial remote command succeeds\n" +
			"  • agent port reachable something is listening on the recorded agent port\n" +
			"  • sshd reachable       an sshd listener on port 22\n" +
			"  • disk usage           warns when the root filesystem is >90% used\n" +
			"  • swap configured      warns when no swap is present\n" +
			"  • reboot required      warns when the host needs a reboot\n" +
			"  • clock skew           remote 'date +%s' vs the controller clock\n\n" +
			"Exit status is non-zero when any check fails. Use --json for the structured\n" +
			"report.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			server, err := app.GetServer(args[0])
			if err != nil {
				return err
			}

			report := core.RunDoctor(core.DoctorProbe{
				Server:    server.Name,
				AgentPort: server.Port,
				Now:       time.Now(),
			}, doctorExecAdapter(app, server.Name))

			if asJSON {
				return writeJSON(cmd, report)
			}
			if err := writeDoctorReport(cmd, report); err != nil {
				return err
			}
			if report.Failed() {
				return fmt.Errorf("doctor: one or more checks failed for %q", report.Server)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the structured report as JSON")
	return cmd
}

// doctorSymbol maps a status to a compact, copy-paste-safe glyph.
func doctorSymbol(s core.DoctorStatus) string {
	switch s {
	case core.DoctorOK:
		return "✔ ok"
	case core.DoctorWarn:
		return "! warn"
	case core.DoctorFail:
		return "✘ fail"
	default:
		return string(s)
	}
}

func writeDoctorReport(cmd *cobra.Command, report core.DoctorReport) error {
	fmt.Fprintf(cmd.OutOrStdout(), "doctor: %s\n", report.Server)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, c := range report.Checks {
		detail := c.Detail
		if detail == "" {
			detail = "-"
		}
		if _, err := fmt.Fprintf(w, "  %s\t%s\t%s\n", doctorSymbol(c.Status), c.Name, detail); err != nil {
			return err
		}
	}
	return w.Flush()
}
