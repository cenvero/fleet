// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/spf13/cobra"
)

// FL-029 — `fleet fw`: the SAFETY-CRITICAL, agent-safe firewall front end.
//
// cobra dispatches the SUBCOMMAND first, so the server is the subcommand's
// argument (subcommand-first routing):
//
//	fleet fw status <server>
//	fleet fw allow <server> <port>[/<proto>]
//	fleet fw enable <server> --safe [--force-i-have-console] [--undo-after 60] [--json]
//
// `enable --safe` reads the host's rules first and refuses (naming the command
// to run first, e.g. `fleet fw allow web-01 2222/tcp`) unless the agent stays
// reachable, then arms a self-reverting timer before applying. The same
// agent-lockout gate guards `fleet firewall enable|add` and `fleet port close`.
//
// The heavy lifting lives in core.FirewallEngine, which is driven here through
// app.FirewallExec. That same engine is unit-tested with a fake exec.
func newFirewallSafeCommand(configDir *string) *cobra.Command {
	fwCmd := &cobra.Command{
		Use:   "fw",
		Short: "Agent-safe firewall control (never locks out the agent)",
		Long: "Agent-safe firewall control. Before any change that could block traffic, `fleet fw`\n" +
			"reads the server's rules and refuses unless the agent stays reachable, naming the\n" +
			"command to run first.\n\n" +
			"  fleet fw status <server>               detect + normalize nft/iptables/firewalld/ufw\n" +
			"  fleet fw allow <server> 443/tcp        open a port (defaults to tcp)\n" +
			"  fleet fw enable <server> --safe        enable default-drop, guarded + auto-reverting\n\n" +
			"In direct mode `enable --safe` needs the agent's port allowed first (`fleet fw allow\n" +
			"<server> <port>/tcp`); in reverse mode it needs established/related and outgoing traffic\n" +
			"kept. It then arms a timer that restores the previous firewall state after --undo-after\n" +
			"seconds unless you cancel it, and prints the cancel command.",
	}

	fwCmd.AddCommand(newFirewallSafeStatusCommand(configDir))
	fwCmd.AddCommand(newFirewallSafeAllowCommand(configDir))
	fwCmd.AddCommand(newFirewallSafeEnableCommand(configDir))
	return fwCmd
}

// addForceConsoleFlag registers the deliberate agent-lockout override.
func addForceConsoleFlag(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVar(target, "force-i-have-console", false,
		"DANGEROUS: skip the agent-lockout refusal. Only for a person with out-of-band console access to the server "+
			"who accepts losing the agent; never for scripts or AI agents. Every use is audit-logged")
}

// newFirewallSafeEngine opens the app, loads the server, and returns a
// FirewallEngine bound to the server's agent and exec transport.
func newFirewallSafeEngine(configDir, server string) (*core.App, *core.FirewallEngine, error) {
	app, err := openApp(configDir)
	if err != nil {
		return nil, nil, err
	}
	record, err := app.GetServer(server)
	if err != nil {
		_ = app.Close()
		return nil, nil, err
	}
	// Guard the REAL agent port, from the record, never a guessed default: an
	// allow for the wrong port would leave the actual agent connection to be
	// dropped. A direct record without a valid port makes `enable` refuse.
	engine := &core.FirewallEngine{
		AgentPort: record.Port,
		Server:    record.Name,
		Mode:      record.Mode,
		Exec:      app.FirewallExec(server),
	}
	return app, engine, nil
}

func newFirewallSafeStatusCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <server>",
		Short: "Detect and normalize the server's firewall state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, engine, err := newFirewallSafeEngine(*configDir, args[0])
			if err != nil {
				return err
			}
			defer app.Close()
			report, err := engine.Status()
			if err != nil {
				return err
			}
			return writeJSON(cmd, report)
		},
	}
}

func newFirewallSafeAllowCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "allow <server> <port>[/<proto>]",
		Short: "Allow inbound traffic to a port (tcp by default)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := core.ParsePortSpec(args[1])
			if err != nil {
				return err
			}
			app, engine, err := newFirewallSafeEngine(*configDir, args[0])
			if err != nil {
				return err
			}
			defer app.Close()
			backend, err := engine.DetectBackend()
			if err != nil {
				return err
			}
			if backend == core.FirewallBackendUnknown {
				return fmt.Errorf("no supported firewall backend (nft/iptables/firewalld/ufw) detected on %q", args[0])
			}
			if _, err := engine.AllowPort(backend, spec); err != nil {
				return err
			}
			if err := app.AuditLog.Append(logs.AuditEntry{
				Action:   "port.open",
				Target:   fmt.Sprintf("%s/%s", args[0], spec),
				Operator: app.Operator(),
				Details:  fmt.Sprintf("fw allow backend=%s", backend),
			}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "allowed %s on %q via %s\n", spec, args[0], backend)
			return nil
		},
	}
}

func newFirewallSafeEnableCommand(configDir *string) *cobra.Command {
	var safe bool
	var forceConsole bool
	var undoAfter int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "enable <server> --safe",
		Short: "Enable a default-drop firewall without locking out the agent",
		Long: "Enable a default-drop firewall safely. fleet first reads the server's rules and refuses\n" +
			"unless the agent stays reachable: in direct mode the agent's port must already be allowed\n" +
			"(the refusal names `fleet fw allow <server> <port>/tcp`); in reverse mode established/related\n" +
			"and outgoing traffic must be kept. For nft and iptables, established/related and loopback\n" +
			"traffic are accepted before the drop policy.\n\n" +
			"Before applying, a self-revert timer is armed. After --undo-after seconds it restores the\n" +
			"previous firewall state unless you cancel it; nothing cancels it automatically. The output\n" +
			"prints the exact cancel command.\n\n" +
			"--safe is required as an explicit acknowledgement. --force-i-have-console skips the\n" +
			"refusals; only a person with out-of-band console access should pass it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !safe {
				return fmt.Errorf("refusing to enable the firewall without --safe (this is a guarded, agent-safe operation)")
			}
			app, engine, err := newFirewallSafeEngine(*configDir, args[0])
			if err != nil {
				return err
			}
			defer app.Close()
			engine.BeforeOverride = app.AuditLockoutOverride
			backend, err := engine.DetectBackend()
			if err != nil {
				return err
			}
			if backend == core.FirewallBackendUnknown {
				return fmt.Errorf("no supported firewall backend (nft/iptables/firewalld/ufw) detected on %q", args[0])
			}
			result, err := engine.EnableSafe(backend, forceConsole, undoAfter)
			if err != nil {
				var refusal *core.AgentLockoutError
				if errors.As(err, &refusal) {
					if auditErr := app.AuditLockoutRefusal(refusal); auditErr != nil {
						return fmt.Errorf("%w (and the refusal could not be audit-logged: %v)", err, auditErr)
					}
				}
				return err
			}
			// The firewall is on and the timer may be armed: print the result
			// (with its cancel command) before anything else can fail.
			for _, skipped := range result.Overrode {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: --force-i-have-console skipped a refusal: %s\n", skipped)
			}
			if !result.UndoScheduled {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"WARNING: no self-revert timer is armed (%s). If you lose the agent, you must recover via console.\n",
					result.UndoScheduleErr)
			}
			if asJSON {
				if err := writeJSON(cmd, result); err != nil {
					return err
				}
			} else {
				writeEnableSafeSummary(cmd.OutOrStdout(), args[0], result)
			}
			return app.AuditLog.Append(logs.AuditEntry{
				Action:   "firewall.enable",
				Target:   args[0],
				Operator: app.Operator(),
				Details: fmt.Sprintf("fw --safe backend=%s mode=%s undo_scheduled=%t revert=%s overrode=%d",
					result.Backend, result.Mode, result.UndoScheduled, result.RevertID, len(result.Overrode)),
			})
		},
	}
	cmd.Flags().BoolVar(&safe, "safe", false, "required acknowledgement that this is a guarded, agent-safe enable")
	addForceConsoleFlag(cmd, &forceConsole)
	cmd.Flags().IntVar(&undoAfter, "undo-after", 60, "seconds before the self-revert timer restores the previous firewall state unless cancelled")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON instead of the summary")
	return cmd
}

// writeEnableSafeSummary prints the human summary of `fw enable --safe`. It
// states what actually happens: the timer fires unless the operator cancels it.
func writeEnableSafeSummary(w io.Writer, server string, r core.EnableSafeResult) {
	switch {
	case r.Mode == transport.ModeReverse.String():
		fmt.Fprintf(w, "firewall enabled on %s via %s. Reverse mode: the agent dials out, so no inbound port was opened; "+
			"established/related traffic stays accepted.\n", server, r.Backend)
	case r.AgentPortConfirmed:
		fmt.Fprintf(w, "firewall enabled on %s via %s; agent port %d/tcp was already allowed.\n", server, r.Backend, r.AgentPort)
	default:
		fmt.Fprintf(w, "firewall enabled on %s via %s; agent port %d/tcp was allowed first (--force-i-have-console).\n", server, r.Backend, r.AgentPort)
	}
	if !r.UndoScheduled {
		return
	}
	fmt.Fprintf(w, "Self-revert timer armed (%s): in %ds it restores %s's previous firewall state unless you cancel it. "+
		"Nothing cancels it automatically.\n", r.RevertMethod, r.UndoDelaySecs, server)
	if r.UndoCmd == "true" {
		fmt.Fprintf(w, "  undo:   nothing to revert (the firewall was already in this state)\n")
	} else {
		fmt.Fprintf(w, "  undo:   %s\n", r.UndoCmd)
	}
	fmt.Fprintf(w, "  cancel: fleet exec %s '%s'\n", server, r.CancelCmd)
	if r.Mode == transport.ModeReverse.String() {
		fmt.Fprintf(w, "Cancel it once `fleet server reconnect %s` shows the agent still connected.\n", server)
	} else {
		fmt.Fprintf(w, "Cancel it once `fleet server reconnect %s` succeeds (it opens a fresh connection to the agent port).\n", server)
	}
}
