// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"fmt"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// FL-029 — `fleet fw`: the SAFETY-CRITICAL, agent-safe firewall front end.
//
// cobra dispatches the SUBCOMMAND first, so the server is the subcommand's
// argument (subcommand-first routing):
//
//	fleet fw status <server>
//	fleet fw allow <server> <port>[/<proto>]
//	fleet fw enable <server> --safe [--force-i-have-console] [--undo-after 60]
//
// Every tightening operation first injects an allow rule for the agent's OWN
// management port (ServerRecord.Port) so the controller can never lock itself
// out of the agent. `enable --safe` additionally refuses any ruleset that would
// drop the agent port (unless --force-i-have-console) and wraps the apply in a
// self-reverting systemd-run timer that reopens the host if something goes wrong.
//
// The heavy lifting lives in core.FirewallEngine, which is driven here through a
// closure over app.ExecCommand. That same engine is unit-tested with a fake exec.
func newFirewallSafeCommand(configDir *string) *cobra.Command {
	fwCmd := &cobra.Command{
		Use:   "fw",
		Short: "Agent-safe firewall control (never locks out the agent)",
		Long: "Agent-safe firewall control. Unlike a raw firewall, `fleet fw` ALWAYS injects an\n" +
			"allow rule for the agent's own management port before applying any tightening\n" +
			"operation, so the controller can never lock itself out of the agent.\n\n" +
			"  fleet fw status <server>               detect + normalize nft/iptables/firewalld/ufw\n" +
			"  fleet fw allow <server> 443/tcp        open a port (defaults to tcp)\n" +
			"  fleet fw enable <server> --safe        enable default-drop, guarded + auto-reverting\n\n" +
			"`enable --safe` refuses a ruleset that would drop the agent port unless you pass\n" +
			"--force-i-have-console, and schedules a self-reverting timer that reopens the host\n" +
			"after --undo-after seconds in case the apply cuts the agent off.",
	}

	fwCmd.AddCommand(newFirewallSafeStatusCommand(configDir))
	fwCmd.AddCommand(newFirewallSafeAllowCommand(configDir))
	fwCmd.AddCommand(newFirewallSafeEnableCommand(configDir))
	return fwCmd
}

// newFirewallSafeEngine opens the app, loads the server, and returns a
// FirewallEngine bound to the server's agent port and exec transport.
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
	agentPort := record.Port
	if agentPort == 0 {
		// Fall back to the controller default so we still guard *some* port rather
		// than silently guarding port 0 (which would be meaningless).
		agentPort = app.Config.Runtime.DefaultAgentPort
		if agentPort == 0 {
			agentPort = 2222
		}
	}
	engine := &core.FirewallEngine{
		AgentPort: agentPort,
		Exec: func(command string) (string, int, error) {
			res, execErr := app.ExecCommand(server, command)
			if execErr != nil {
				return "", 0, execErr
			}
			// Surface stderr alongside stdout so status normalization and error
			// messages see the full backend output.
			out := res.Stdout
			if res.Stderr != "" {
				if out != "" {
					out += "\n"
				}
				out += res.Stderr
			}
			return out, res.ExitCode, nil
		},
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
			fmt.Fprintf(cmd.OutOrStdout(), "allowed %s on %q via %s\n", spec, args[0], backend)
			return nil
		},
	}
}

func newFirewallSafeEnableCommand(configDir *string) *cobra.Command {
	var safe bool
	var forceConsole bool
	var undoAfter int
	cmd := &cobra.Command{
		Use:   "enable <server> --safe",
		Short: "Enable a default-drop firewall without locking out the agent",
		Long: "Enable a default-drop firewall safely. Before applying, an allow rule for the\n" +
			"agent's own management port is injected, the resulting ruleset is checked to\n" +
			"confirm the agent stays reachable, and a self-reverting timer is scheduled so the\n" +
			"host reopens automatically if the apply cuts the agent off.\n\n" +
			"--safe is required as an explicit acknowledgement. Pass --force-i-have-console\n" +
			"only when you have out-of-band console access and accept the lockout risk.",
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
			backend, err := engine.DetectBackend()
			if err != nil {
				return err
			}
			if backend == core.FirewallBackendUnknown {
				return fmt.Errorf("no supported firewall backend (nft/iptables/firewalld/ufw) detected on %q", args[0])
			}
			result, err := engine.EnableSafe(backend, forceConsole, undoAfter)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"firewall enabled on %q via %s; agent port %d allowed first.\n",
				args[0], result.Backend, result.AgentPort)
			if result.UndoScheduled {
				fmt.Fprintf(cmd.OutOrStdout(),
					"self-revert timer armed: the host auto-reopens in %ds if the agent does not check back in. "+
						"Cancel it once you've confirmed connectivity:\n  systemctl stop %s.timer %s.service\n",
					result.UndoDelaySecs, result.RevertUnit, result.RevertUnit)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"WARNING: could not arm the self-revert timer (%s). If you lose the agent, you must recover via console.\n",
					result.UndoScheduleErr)
			}
			return writeJSON(cmd, result)
		},
	}
	cmd.Flags().BoolVar(&safe, "safe", false, "required acknowledgement that this is a guarded, agent-safe enable")
	cmd.Flags().BoolVar(&forceConsole, "force-i-have-console", false, "bypass the agent-port lockout refusal (only with out-of-band console access)")
	cmd.Flags().IntVar(&undoAfter, "undo-after", 60, "seconds before the self-reverting timer reopens the host")
	return cmd
}
