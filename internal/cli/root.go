// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/tui"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	var configDir string

	root := &cobra.Command{
		Use:   "fleet",
		Short: "Cenvero Fleet controller",
		Long:  "Cenvero Fleet is a self-hosted, decentralized fleet management platform.",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if configDir == "" {
				configDir = core.DefaultConfigDir("")
			}
			return nil
		},
	}

	root.Version = version.Version
	root.PersistentFlags().StringVar(&configDir, "config-dir", "", "configuration directory")
	root.SetVersionTemplate("Cenvero Fleet {{.Version}}\n")

	root.AddCommand(newInitCommand(&configDir))
	root.AddCommand(newStatusCommand(&configDir))
	root.AddCommand(newLifecycleCommand("start", &configDir))
	root.AddCommand(newLifecycleCommand("stop", &configDir))
	root.AddCommand(newLifecycleCommand("daemon", &configDir))
	root.AddCommand(newDashboardCommand(&configDir))
	root.AddCommand(newServerCommand(&configDir))
	root.AddCommand(newServiceCommand(&configDir))
	root.AddCommand(newLogsCommand(&configDir))
	root.AddCommand(newPortCommand(&configDir))
	root.AddCommand(newFirewallCommand(&configDir))
	root.AddCommand(newAlertsCommand(&configDir))
	root.AddCommand(newDatabaseCommand(&configDir))
	root.AddCommand(newConfigCommand(&configDir))
	root.AddCommand(newTemplateCommand(&configDir))
	root.AddCommand(newKeyCommand(&configDir))
	root.AddCommand(newUpdateCommand(&configDir))
	root.AddCommand(newSelfUninstallCommand(&configDir))
	return root
}

func newInitCommand(configDir *string) *cobra.Command {
	var (
		nonInteractive bool
		alias          string
		mode           string
		algorithm      string
		channel        string
		policy         string
		passphrase     string
		dbBackend      string
		dbDSN          string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run the first-time setup flow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			executable, _ := os.Executable()
			if nonInteractive {
				parsedMode, err := transport.ParseMode(mode)
				if err != nil {
					return err
				}
				result, err := core.Initialize(core.InitOptions{
					ConfigDir:       *configDir,
					Alias:           alias,
					DefaultMode:     parsedMode,
					CryptoAlgorithm: algorithm,
					Passphrase:      passphrase,
					UpdateChannel:   channel,
					UpdatePolicy:    update.Policy(policy),
					DatabaseBackend: store.Backend(strings.TrimSpace(strings.ToLower(dbBackend))),
					DatabaseDSN:     dbDSN,
					ExecutablePath:  executable,
				})
				if err != nil {
					return err
				}
				return writeJSON(cmd, result)
			}
			result, err := core.RunInitInteractive(cmd.InOrStdin(), cmd.OutOrStdout(), executable)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nInitialized %s in %s\n", version.ProductName, result.Config.ConfigDir)
			fmt.Fprintf(cmd.OutOrStdout(), "Run `%s dashboard` or `%s status` next.\n", version.BinaryName, version.BinaryName)
			return nil
		},
	}

	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "initialize without prompts")
	cmd.Flags().StringVar(&alias, "alias", "fleet", "short alias for the controller binary")
	cmd.Flags().StringVar(configDir, "init-config-dir", "", "configuration directory to initialize")
	cmd.Flags().StringVar(&mode, "mode", "reverse", "default transport mode")
	cmd.Flags().StringVar(&algorithm, "crypto", "ed25519", "key algorithm (ed25519, rsa-4096, both)")
	cmd.Flags().StringVar(&channel, "channel", "stable", "default update channel")
	cmd.Flags().StringVar(&policy, "policy", string(update.PolicyNotifyOnly), "update policy")
	cmd.Flags().StringVar(&passphrase, "passphrase", "", "passphrase for private keys in non-interactive mode")
	cmd.Flags().StringVar(&dbBackend, "db-backend", "sqlite", "database backend: sqlite, postgres, mysql, mariadb")
	cmd.Flags().StringVar(&dbDSN, "db-dsn", "", "database DSN for postgres, mysql, or mariadb")
	return cmd
}

func newStatusCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show controller status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			status, err := app.Status()
			if err != nil {
				return err
			}
			return writeJSON(cmd, status)
		},
	}
}

func newLifecycleCommand(action string, configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   action,
		Short: strings.Title(action) + " the controller runtime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if action == "daemon" {
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				fmt.Fprintf(cmd.OutOrStdout(), "Cenvero Fleet daemon listening for reverse agents on %s\n", app.Config.Runtime.ListenAddress)
				fmt.Fprintf(cmd.OutOrStdout(), "Cenvero Fleet local control listening on %s\n", app.Config.Runtime.ControlAddress)
				if strings.TrimSpace(app.Config.Runtime.MetricsPollInterval) != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Cenvero Fleet metrics polling every %s\n", app.Config.Runtime.MetricsPollInterval)
				}
				if app.Config.Runtime.DesktopNotifications {
					fmt.Fprintln(cmd.OutOrStdout(), "Cenvero Fleet desktop notifications enabled")
				}
				return app.RunDaemon(ctx)
			}
			now := time.Now().UTC().Format(time.RFC3339)
			if err := app.StateDB.PutState("controller."+action, now); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "controller %s state recorded at %s\n", action, now)
			return nil
		},
	}
}

func newDashboardCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Launch the terminal dashboard",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return tui.RunDashboard(*configDir)
		},
	}
}

func newServerCommand(configDir *string) *cobra.Command {
	serverCmd := &cobra.Command{Use: "server", Short: "Manage servers"}

	var mode string
	var port int
	var keyPath string
	var user string
	var loginUser string
	var loginPort int
	var loginKey string
	var useSudo bool

	add := &cobra.Command{
		Use:   "add <name> <ip>",
		Short: "Add a server to the fleet",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			parsedMode, err := transport.ParseMode(mode)
			if err != nil {
				return err
			}
			if err := app.AddServer(core.ServerRecord{
				Name:    args[0],
				Address: args[1],
				Mode:    parsedMode,
				Port:    port,
				User:    user,
				KeyPath: keyPath,
			}); err != nil {
				return err
			}

			if loginUser != "" && parsedMode == transport.ModeDirect {
				fmt.Fprintf(cmd.OutOrStdout(), "Installing agent on %s...\n", args[0])
				lp := loginPort
				if lp == 0 {
					lp = 22
				}
				if err := app.AutoInstallAgent(args[0], loginUser, loginKey, lp, useSudo); err != nil {
					return fmt.Errorf("agent install failed (server was added): %w\n"+
						"Run 'fleet server bootstrap %s --login-user %s' to retry manually", err, args[0], loginUser)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Agent installed and running. Connect with: fleet server reconnect %s\n", args[0])
			} else if parsedMode == transport.ModeDirect && loginUser == "" {
				record, _ := app.GetServer(args[0])
				fmt.Fprintln(cmd.OutOrStdout(), core.AgentInstallInstructions(record, parsedMode))
			}
			return nil
		},
	}
	add.Flags().StringVar(&mode, "mode", "direct", "server transport mode")
	add.Flags().IntVar(&port, "port", 2222, "agent SSH port (after install)")
	add.Flags().StringVar(&user, "user", "root", "agent SSH username")
	add.Flags().StringVar(&keyPath, "key", "", "SSH key path override")
	add.Flags().StringVar(&loginUser, "login-user", "", "initial SSH login user for agent install (e.g. root, ubuntu)")
	add.Flags().IntVar(&loginPort, "login-port", 22, "SSH port for initial login")
	add.Flags().StringVar(&loginKey, "login-key", "", "SSH key for initial login (defaults to fleet key)")
	add.Flags().BoolVar(&useSudo, "sudo", false, "use sudo for agent install commands")

	serverCmd.AddCommand(add)
	var listFormat string
	list := &cobra.Command{
		Use:   "list",
		Short: "List servers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			servers, err := app.ListServers()
			if err != nil {
				return err
			}
			if strings.EqualFold(listFormat, "table") {
				return writeServerTable(cmd, servers)
			}
			return writeJSON(cmd, servers)
		},
	}
	list.Flags().StringVar(&listFormat, "format", "table", "output format: table or json")
	serverCmd.AddCommand(list)
	serverCmd.AddCommand(&cobra.Command{
		Use:   "show <name>",
		Short: "Show a server definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			server, err := app.GetServer(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, server)
		},
	})
	serverCmd.AddCommand(&cobra.Command{
		Use:   "metrics <name>",
		Short: "Collect a live metrics snapshot from a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			snapshot, err := app.CollectMetrics(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, snapshot)
		},
	})
	serverCmd.AddCommand(&cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.RemoveServer(args[0])
		},
	})
	reconnect := &cobra.Command{
		Use:   "reconnect <name>",
		Short: "Reconnect to a server and optionally accept a new host key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			accept, _ := cmd.Flags().GetBool("accept-new-host-key")
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.ReconnectServer(args[0], accept); err != nil {
				return err
			}
			server, err := app.GetServer(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, server)
		},
	}
	reconnect.Flags().Bool("accept-new-host-key", false, "accept a replacement host key after manual verification")
	serverCmd.AddCommand(reconnect)
	serverCmd.AddCommand(&cobra.Command{
		Use:   "mode <name> <mode>",
		Short: "Change the transport mode for a server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			mode, err := transport.ParseMode(args[1])
			if err != nil {
				return err
			}
			return app.SetServerMode(args[0], mode)
		},
	})

	var (
		bootstrapLoginUser   string
		bootstrapLoginPort   int
		bootstrapLoginKey    string
		bootstrapAgentBinary string
		bootstrapListenAddr  string
		bootstrapController  string
		bootstrapServiceName string
		bootstrapUseSudo     bool
		bootstrapAcceptHost  bool
		bootstrapPrintScript bool
	)
	bootstrapCmd := &cobra.Command{
		Use:   "bootstrap <name>",
		Short: "Install and configure a Linux fleet-agent service on a remote host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			result, err := app.BootstrapServer(args[0], core.BootstrapOptions{
				LoginUser:         bootstrapLoginUser,
				LoginPort:         bootstrapLoginPort,
				LoginKeyPath:      bootstrapLoginKey,
				AgentBinaryPath:   bootstrapAgentBinary,
				AgentListenAddr:   bootstrapListenAddr,
				ControllerAddress: bootstrapController,
				ServiceName:       bootstrapServiceName,
				AcceptNewHostKey:  bootstrapAcceptHost,
				UseSudo:           bootstrapUseSudo,
				PrintScript:       bootstrapPrintScript,
			})
			if err != nil {
				return err
			}
			if bootstrapPrintScript {
				_, err = fmt.Fprint(cmd.OutOrStdout(), result.Script)
				return err
			}
			return writeJSON(cmd, result)
		},
	}
	bootstrapCmd.Flags().StringVar(&bootstrapLoginUser, "login-user", "", "SSH login user used to bootstrap the remote host")
	bootstrapCmd.Flags().IntVar(&bootstrapLoginPort, "login-port", 22, "SSH port used for the bootstrap login")
	bootstrapCmd.Flags().StringVar(&bootstrapLoginKey, "login-key", "", "SSH private key used for the bootstrap login")
	bootstrapCmd.Flags().StringVar(&bootstrapAgentBinary, "agent-binary", "", "local fleet-agent binary to upload")
	bootstrapCmd.Flags().StringVar(&bootstrapListenAddr, "listen", "", "agent listen address for direct mode")
	bootstrapCmd.Flags().StringVar(&bootstrapController, "controller", "", "reachable controller address for reverse mode")
	bootstrapCmd.Flags().StringVar(&bootstrapServiceName, "service-name", "", "systemd service name for the deployed agent")
	bootstrapCmd.Flags().BoolVar(&bootstrapUseSudo, "sudo", true, "run install steps with sudo on the remote host")
	bootstrapCmd.Flags().BoolVar(&bootstrapAcceptHost, "accept-new-host-key", false, "accept a replacement bootstrap SSH host key after manual verification")
	bootstrapCmd.Flags().BoolVar(&bootstrapPrintScript, "print-script", false, "print the generated bootstrap script instead of executing it")
	serverCmd.AddCommand(bootstrapCmd)
	return serverCmd
}

func newServiceCommand(configDir *string) *cobra.Command {
	serviceCmd := &cobra.Command{Use: "service", Short: "Manage tracked services"}
	var logPath string
	var critical bool

	serviceCmd.AddCommand(&cobra.Command{
		Use:   "list <server>",
		Short: "List services discovered on a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			services, err := app.ListServices(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, services)
		},
	})

	add := &cobra.Command{
		Use:   "add <server> <name>",
		Short: "Add a service to a server definition",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.AddService(args[0], args[1], logPath, critical)
		},
	}
	add.Flags().StringVar(&logPath, "log", "", "service log path")
	add.Flags().BoolVar(&critical, "critical", false, "mark the service as critical")
	serviceCmd.AddCommand(add)

	for _, action := range []string{"start", "stop", "restart"} {
		action := action
		serviceCmd.AddCommand(&cobra.Command{
			Use:   action + " <server> <name>",
			Short: strings.Title(action) + " a tracked service",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				app, err := openApp(*configDir)
				if err != nil {
					return err
				}
				defer app.Close()
				return app.ControlService(args[0], args[1], action)
			},
		})
	}

	logsCmd := &cobra.Command{
		Use:   "logs <server> <name>",
		Short: "Read a tracked service log over the live transport",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			search, _ := cmd.Flags().GetString("search")
			exportPath, _ := cmd.Flags().GetString("export")
			tailLines, _ := cmd.Flags().GetInt("lines")
			follow, _ := cmd.Flags().GetBool("follow")
			cached, _ := cmd.Flags().GetBool("cached")
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if cached && follow {
				return fmt.Errorf("--cached cannot be combined with --follow")
			}
			if cached {
				result, err := app.ReadCachedServiceLogs(args[0], args[1], search, tailLines)
				if err != nil {
					return err
				}
				return writeLogOutput(cmd, result, exportPath)
			}
			if follow {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
				defer stop()
				return followServiceLogs(ctx, cmd, app, args[0], args[1], search, tailLines, exportPath)
			}
			result, err := app.ReadServiceLogs(args[0], args[1], search, tailLines, follow)
			if err != nil {
				return err
			}
			return writeLogOutput(cmd, result, exportPath)
		},
	}
	logsCmd.Flags().Bool("follow", false, "follow the remote log")
	logsCmd.Flags().String("search", "", "filter log lines by substring")
	logsCmd.Flags().Int("lines", 200, "maximum number of lines to return")
	logsCmd.Flags().String("export", "", "write log output to a file")
	logsCmd.Flags().Bool("cached", false, "read the controller's aggregated cached copy instead of the live remote log")
	serviceCmd.AddCommand(logsCmd)
	return serviceCmd
}

func newLogsCommand(configDir *string) *cobra.Command {
	var exportPath string
	var search string
	var server string
	var service string
	var last string
	var follow bool
	var cached bool

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Read audit logs or remote service logs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			if cached && (server == "" || service == "") {
				return fmt.Errorf("--cached requires both --server and --service")
			}
			if server != "" && service != "" {
				if cached && follow {
					return fmt.Errorf("--cached cannot be combined with --follow")
				}
				if cached {
					result, err := app.ReadCachedServiceLogs(server, service, search, 200)
					if err != nil {
						return err
					}
					return writeLogOutput(cmd, result, exportPath)
				}
				if follow {
					ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
					defer stop()
					return followServiceLogs(ctx, cmd, app, server, service, search, 200, exportPath)
				}
				result, err := app.ReadServiceLogs(server, service, search, 200, false)
				if err != nil {
					return err
				}
				return writeLogOutput(cmd, result, exportPath)
			}
			if follow {
				return fmt.Errorf("--follow currently requires both --server and --service")
			}
			entries, err := app.AuditEntries()
			if err != nil {
				return err
			}
			if search != "" {
				filtered := entries[:0]
				for _, entry := range entries {
					payload, _ := json.Marshal(entry)
					match := strings.Contains(strings.ToLower(string(payload)), strings.ToLower(search))
					if server != "" {
						match = match && strings.Contains(strings.ToLower(string(payload)), strings.ToLower(server))
					}
					if service != "" {
						match = match && strings.Contains(strings.ToLower(string(payload)), strings.ToLower(service))
					}
					if match {
						filtered = append(filtered, entry)
					}
				}
				entries = filtered
			}
			_ = last
			if exportPath == "" {
				return writeJSON(cmd, entries)
			}
			data, err := json.MarshalIndent(entries, "", "  ")
			if err != nil {
				return err
			}
			return os.WriteFile(exportPath, append(data, '\n'), 0o644)
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "filter by server name")
	cmd.Flags().StringVar(&service, "service", "", "filter by service name")
	cmd.Flags().StringVar(&search, "search", "", "search term")
	cmd.Flags().StringVar(&last, "last", "", "reserved for duration-based filtering")
	cmd.Flags().StringVar(&exportPath, "export", "", "write results to a file")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow a remote service log when --server and --service are set")
	cmd.Flags().BoolVar(&cached, "cached", false, "read the controller's aggregated cached copy when --server and --service are set")
	return cmd
}

func newPortCommand(configDir *string) *cobra.Command {
	portCmd := &cobra.Command{Use: "port", Short: "Manage live firewall-exposed ports"}
	portCmd.AddCommand(&cobra.Command{
		Use:   "list <server>",
		Short: "List open ports for a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			ports, err := app.ListPorts(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, ports)
		},
	})
	for _, action := range []string{"open", "close"} {
		action := action
		portCmd.AddCommand(&cobra.Command{
			Use:   action + " <server> <port>",
			Short: strings.Title(action) + " a tracked port",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				port, err := strconv.Atoi(args[1])
				if err != nil {
					return err
				}
				app, err := openApp(*configDir)
				if err != nil {
					return err
				}
				defer app.Close()
				return app.SetPort(args[0], port, action == "open")
			},
		})
	}
	return portCmd
}

func newFirewallCommand(configDir *string) *cobra.Command {
	firewallCmd := &cobra.Command{Use: "firewall", Short: "Manage live firewall state"}
	firewallCmd.AddCommand(&cobra.Command{
		Use:   "status <server>",
		Short: "Show firewall state for a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			state, err := app.FirewallStatus(args[0])
			if err != nil {
				return err
			}
			return writeJSON(cmd, state)
		},
	})
	for _, action := range []string{"enable", "disable"} {
		action := action
		firewallCmd.AddCommand(&cobra.Command{
			Use:   action + " <server>",
			Short: strings.Title(action) + " a server firewall",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				app, err := openApp(*configDir)
				if err != nil {
					return err
				}
				defer app.Close()
				return app.SetFirewall(args[0], action == "enable")
			},
		})
	}
	firewallCmd.AddCommand(&cobra.Command{
		Use:   "add <server> <rule>",
		Short: "Add a firewall rule to the tracked state",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.AddFirewallRule(args[0], args[1])
		},
	})
	return firewallCmd
}

func newAlertsCommand(configDir *string) *cobra.Command {
	var severity string
	var server string
	alertsCmd := &cobra.Command{
		Use:   "alerts",
		Short: "List alerts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			alerts, err := app.ListAlerts(server, severity)
			if err != nil {
				return err
			}
			return writeJSON(cmd, alerts)
		},
	}
	alertsCmd.Flags().StringVar(&severity, "severity", "", "filter by severity")
	alertsCmd.Flags().StringVar(&server, "server", "", "filter by server name")
	alertsCmd.AddCommand(&cobra.Command{
		Use:   "ack <alert-id>",
		Short: "Acknowledge an alert",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.AckAlert(args[0])
		},
	})
	var suppressFor time.Duration
	suppressCmd := &cobra.Command{
		Use:   "suppress <alert-id>",
		Short: "Suppress alert notifications for a period of time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.SuppressAlert(args[0], suppressFor)
		},
	}
	suppressCmd.Flags().DurationVar(&suppressFor, "for", 6*time.Hour, "how long to suppress notifications for this alert")
	alertsCmd.AddCommand(suppressCmd)
	alertsCmd.AddCommand(&cobra.Command{
		Use:   "unsuppress <alert-id>",
		Short: "Remove a suppression window from an alert",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.UnsuppressAlert(args[0])
		},
	})
	return alertsCmd
}

func newDatabaseCommand(configDir *string) *cobra.Command {
	databaseCmd := &cobra.Command{Use: "database", Short: "Manage controller database backends"}

	databaseCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the current database backend configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return writeJSON(cmd, app.Config.Database)
		},
	})

	var backend string
	var dsn string

	shiftCmd := &cobra.Command{
		Use:   "shift",
		Short: "Copy controller data into a new backend and switch the config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			result, err := core.ShiftDatabase(*configDir, store.Backend(strings.TrimSpace(strings.ToLower(backend))), strings.TrimSpace(dsn))
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	}
	shiftCmd.Flags().StringVar(&backend, "backend", "", "target database backend: sqlite, postgres, mysql, mariadb")
	shiftCmd.Flags().StringVar(&dsn, "dsn", "", "database DSN for postgres, mysql, or mariadb")
	_ = shiftCmd.MarkFlagRequired("backend")
	databaseCmd.AddCommand(shiftCmd)

	return databaseCmd
}

func newConfigCommand(configDir *string) *cobra.Command {
	configCmd := &cobra.Command{Use: "config", Short: "Inspect controller configuration"}
	configCmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the current configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return writeJSON(cmd, app.Config)
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "edit",
		Short: "Open the configuration in $EDITOR",
		RunE: func(cmd *cobra.Command, _ []string) error {
			editor := os.Getenv("EDITOR")
			if editor == "" {
				return errors.New("$EDITOR is not set")
			}
			path := core.ConfigPath(*configDir)
			c := exec.Command(editor, path)
			c.Stdin = cmd.InOrStdin()
			c.Stdout = cmd.OutOrStdout()
			c.Stderr = cmd.ErrOrStderr()
			return c.Run()
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Validate the current configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := core.LoadConfig(core.ConfigPath(*configDir))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "configuration valid for %s\n", cfg.ProductName)
			return nil
		},
	})
	backupCmd := &cobra.Command{
		Use:   "backup",
		Short: "Create a backup archive of the config directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			output, _ := cmd.Flags().GetString("output")
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			path, err := app.Backup(output)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	backupCmd.Flags().String("output", "", "backup path")
	configCmd.AddCommand(backupCmd)
	configCmd.AddCommand(&cobra.Command{
		Use:   "restore <file>",
		Short: "Restore the config directory from a backup archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return core.RestoreBackup(args[0], *configDir)
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "export",
		Short: "Export the controller state as JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			export, err := app.Export()
			if err != nil {
				return err
			}
			return core.WriteExport("-", export)
		},
	})
	configCmd.AddCommand(&cobra.Command{
		Use:   "import <file>",
		Short: "Import controller state from a JSON export",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			export, err := core.ReadExport(args[0])
			if err != nil {
				return err
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.Import(export)
		},
	})
	return configCmd
}

func newTemplateCommand(configDir *string) *cobra.Command {
	templateCmd := &cobra.Command{Use: "template", Short: "Manage templates"}
	templateCmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List available templates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			templates, err := app.ListTemplates()
			if err != nil {
				return err
			}
			return writeJSON(cmd, templates)
		},
	})
	templateCmd.AddCommand(&cobra.Command{
		Use:   "apply <server> <template>",
		Short: "Apply a template to a server record",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.ApplyTemplate(args[0], args[1])
		},
	})
	return templateCmd
}

func newKeyCommand(configDir *string) *cobra.Command {
	keyCmd := &cobra.Command{Use: "key", Short: "Manage controller keys"}
	keyCmd.AddCommand(&cobra.Command{
		Use:   "rotate",
		Short: "Rotate controller keys and roll them out to managed direct-mode servers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			result, err := app.RotateKeys()
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	})
	keyCmd.AddCommand(&cobra.Command{
		Use:   "fingerprint",
		Short: "Show public key fingerprints",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			fingerprints, err := crypto.Fingerprints(filepath.Join(app.ConfigDir, "keys"))
			if err != nil {
				return err
			}
			return writeJSON(cmd, fingerprints)
		},
	})
	keyCmd.AddCommand(&cobra.Command{
		Use:   "export-pub",
		Short: "Export controller public keys",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			keys, err := crypto.ExportPublicKeys(filepath.Join(app.ConfigDir, "keys"))
			if err != nil {
				return err
			}
			return writeJSON(cmd, keys)
		},
	})
	keyCmd.AddCommand(&cobra.Command{
		Use:   "audit",
		Short: "Show key-related audit events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			entries, err := app.AuditEntries()
			if err != nil {
				return err
			}
			filtered := entries[:0]
			for _, entry := range entries {
				if strings.Contains(entry.Action, "key") || strings.Contains(entry.Action, "controller.init") {
					filtered = append(filtered, entry)
				}
			}
			return writeJSON(cmd, filtered)
		},
	})
	return keyCmd
}

func newUpdateCommand(configDir *string) *cobra.Command {
	updateCmd := &cobra.Command{Use: "update", Short: "Manage self-update configuration"}
	updateCmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Fetch the update manifest for the current channel",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			manifest, err := update.Fetch(context.Background(), app.Config.ManifestURL)
			if err != nil {
				return err
			}
			version, binary, err := manifest.BinaryFor(app.Config.Updates.Channel, false)
			if err != nil {
				return err
			}
			return writeJSON(cmd, map[string]any{"version": version, "binary": binary})
		},
	})
	var targetServers []string
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the latest controller update and roll it out across managed agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			result, err := app.ApplyFleetUpdate(cmd.Context(), targetServers)
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	}
	applyCmd.Flags().StringArrayVar(&targetServers, "server", nil, "limit the rollout to one or more specific servers")
	updateCmd.AddCommand(applyCmd)
	updateCmd.AddCommand(&cobra.Command{
		Use:   "rollback",
		Short: "Restore the previously backed up controller binary after an applied update",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			result, err := app.RollbackUpdate()
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	})
	updateCmd.AddCommand(&cobra.Command{
		Use:   "channel <stable|beta>",
		Short: "Set the update channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.UpdateChannel(args[0])
		},
	})
	return updateCmd
}

func newSelfUninstallCommand(configDir *string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "self-uninstall",
		Short: "Remove the local config directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("refusing to uninstall without --yes")
			}
			if *configDir == "" {
				*configDir = core.DefaultConfigDir("")
			}
			return os.RemoveAll(*configDir)
		},
		Args: cobra.NoArgs,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			yesFlag, _ := cmd.Flags().GetBool("yes")
			yes = yesFlag
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm removal of the config directory")
	return cmd
}

func openApp(configDir string) (*core.App, error) {
	app, err := core.Open(configDir)
	if err != nil {
		if errors.Is(err, core.ErrNotInitialized) {
			return nil, fmt.Errorf("%w; run `fleet init` first", err)
		}
	}
	return app, err
}

func writeJSON(cmd *cobra.Command, payload any) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}

func writeServerTable(cmd *cobra.Command, servers []core.ServerRecord) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tMODE\tADDRESS\tSTATUS\tNODE\tOS/ARCH\tVERSION"); err != nil {
		return err
	}
	for _, server := range servers {
		status := "offline"
		if server.Observed.Reachable {
			status = "online"
		}
		osArch := strings.Trim(strings.Join([]string{server.Observed.OS, server.Observed.Arch}, "/"), "/")
		if osArch == "" {
			osArch = "-"
		}
		node := server.Observed.NodeName
		if node == "" {
			node = "-"
		}
		version := server.Observed.AgentVersion
		if version == "" {
			version = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s:%d\t%s\t%s\t%s\t%s\n", server.Name, server.Mode, server.Address, server.Port, status, node, osArch, version); err != nil {
			return err
		}
	}
	return w.Flush()
}

func writeLogOutput(cmd *cobra.Command, result proto.LogReadResult, exportPath string) error {
	lines := make([]string, 0, len(result.Lines))
	for _, line := range result.Lines {
		lines = append(lines, fmt.Sprintf("%6d  %s", line.Number, line.Text))
	}
	output := strings.Join(lines, "\n")
	if output != "" {
		output += "\n"
	}
	if exportPath == "" {
		_, err := fmt.Fprint(cmd.OutOrStdout(), output)
		return err
	}
	return os.WriteFile(exportPath, []byte(output), 0o644)
}

func followServiceLogs(ctx context.Context, cmd *cobra.Command, app *core.App, serverName, serviceName, search string, tailLines int, exportPath string) error {
	if exportPath != "" {
		if err := os.WriteFile(exportPath, nil, 0o644); err != nil {
			return err
		}
	}
	return app.FollowServiceLogs(ctx, serverName, serviceName, search, tailLines, core.DefaultLogFollowInterval, func(line proto.LogLine) error {
		formatted := fmt.Sprintf("%6d  %s\n", line.Number, line.Text)
		if exportPath == "" {
			_, err := fmt.Fprint(cmd.OutOrStdout(), formatted)
			return err
		}
		f, err := os.OpenFile(exportPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(formatted)
		return err
	})
}
