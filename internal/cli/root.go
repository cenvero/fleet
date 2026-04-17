// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

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
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !core.IsInitialized(configDir) {
				fmt.Fprintln(cmd.OutOrStdout(), "Welcome to Cenvero Fleet.")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "Fleet is not initialized yet. Run the setup wizard first:")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "  fleet init")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "To report an issue: fleet report")
				return nil
			}
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if configDir == "" {
				configDir = core.DefaultConfigDir("")
			}
			// Commands that are always allowed before init
			switch cmd.Name() {
			case "init", "help", "report", "version", "self-uninstall", "completion", "fleet",
				"check", "apply", "rollback", "channel",
				"backup", "recover":
				return nil
			}
			if cmd.HasParent() {
				switch cmd.Parent().Name() {
				case "help", "completion", "update":
					return nil
				}
			}
			if !core.IsInitialized(configDir) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Fleet is not initialized yet.\n")
				fmt.Fprintf(cmd.ErrOrStderr(), "Run 'fleet init' first to set up the controller.\n")
				os.Exit(1)
			}
			// Background Homebrew update hint — checks manifest at most once per 10 minutes
			if core.RuntimeIsHomebrewInstall() {
				if cfg, err := core.LoadConfig(core.ConfigPath(configDir)); err == nil {
					if hint := core.HomebrewUpdateHint(configDir, cfg.ManifestURL, cfg.Updates.Policy); hint != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "\nUpdate available (%s). To upgrade:\n\n  brew update && brew upgrade cenvero-fleet\n\n", hint)
					}
				}
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
	root.AddCommand(newExecCommand(&configDir))
	root.AddCommand(newSSHCommand(&configDir))
	root.AddCommand(newPortCommand(&configDir))
	root.AddCommand(newFirewallCommand(&configDir))
	root.AddCommand(newAlertsCommand(&configDir))
	root.AddCommand(newDatabaseCommand(&configDir))
	root.AddCommand(newConfigCommand(&configDir))
	root.AddCommand(newTemplateCommand(&configDir))
	root.AddCommand(newKeyCommand(&configDir))
	root.AddCommand(newUpdateCommand(&configDir))
	root.AddCommand(newSyncAgentCommand(&configDir))
	root.AddCommand(newBackupCommand(&configDir))
	root.AddCommand(newRecoverCommand(&configDir))
	root.AddCommand(newSelfUninstallCommand(&configDir))
	root.AddCommand(newReportCommand())
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
		agentPort      int
		listenAddress  string
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
					ConfigDir:        *configDir,
					Alias:            alias,
					DefaultMode:      parsedMode,
					CryptoAlgorithm:  algorithm,
					Passphrase:       passphrase,
					UpdateChannel:    channel,
					UpdatePolicy:     update.Policy(policy),
					DatabaseBackend:  store.Backend(strings.TrimSpace(strings.ToLower(dbBackend))),
					DatabaseDSN:      dbDSN,
					ExecutablePath:   executable,
					DefaultAgentPort: agentPort,
					ListenAddress:    listenAddress,
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
	cmd.Flags().IntVar(&agentPort, "agent-port", 0, "default agent SSH port for direct mode (0 = use 2222)")
	cmd.Flags().StringVar(&listenAddress, "listen-address", "", "controller listen address for reverse-mode agents (e.g. 0.0.0.0:9443)")
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
	var loginPassword string
	var useSudo bool
	var noAgent bool

	add := &cobra.Command{
		Use:   "add [name] [ip]",
		Short: "Add a server to the fleet",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			scanner := bufio.NewScanner(os.Stdin)

			prompt := func(label, def string) string {
				if def != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s [%s]: ", label, def)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: ", label)
				}
				scanner.Scan()
				v := strings.TrimSpace(scanner.Text())
				if v == "" {
					return def
				}
				return v
			}

			name := ""
			address := ""
			if len(args) >= 1 {
				name = args[0]
			}
			if len(args) >= 2 {
				address = args[1]
			}

			// Enter interactive mode when name or address are missing.
			interactive := name == "" || address == ""
			if interactive {
				fmt.Fprintln(cmd.OutOrStdout(), "Adding a server — press Enter to accept defaults.")
				fmt.Fprintln(cmd.OutOrStdout())
				if name == "" {
					name = prompt("Server name", "")
					if name == "" {
						return fmt.Errorf("server name is required")
					}
				}
				if address == "" {
					address = prompt("IP address or hostname", "")
					if address == "" {
						return fmt.Errorf("address is required")
					}
				}
				if !cmd.Flags().Changed("mode") {
					mode = prompt("Transport mode (direct/reverse)", "direct")
				}
			}

			parsedMode, err := transport.ParseMode(mode)
			if err != nil {
				return err
			}

			// For direct mode: prompt for login credentials interactively
			// unless --no-agent was passed or credentials were given as flags.
			if parsedMode == transport.ModeDirect && !noAgent {
				if loginUser == "" {
					if interactive {
						loginUser = prompt("Login user for agent install (leave blank to skip)", "root")
					} else {
						// non-interactive: ask on stdin since --login-user was not given
						fmt.Fprintf(cmd.OutOrStdout(), "Login user for agent install [root] (--no-agent to skip): ")
						scanner.Scan()
						v := strings.TrimSpace(scanner.Text())
						if v == "" {
							v = "root"
						}
						loginUser = v
					}
				}
				if loginUser != "" && loginKey == "" && loginPassword == "" {
					if interactive {
						authChoice := prompt("Auth method — [K]ey or [P]assword", "K")
						if strings.EqualFold(strings.TrimSpace(authChoice), "p") ||
							strings.HasPrefix(strings.ToLower(authChoice), "p") {
							fmt.Fprintf(cmd.OutOrStdout(), "Password: ")
							if pw, err := term.ReadPassword(0); err == nil {
								loginPassword = string(pw)
								fmt.Fprintln(cmd.OutOrStdout())
							} else {
								// fallback if stdin is not a real tty
								scanner.Scan()
								loginPassword = strings.TrimSpace(scanner.Text())
							}
						} else {
							loginKey = prompt("Path to SSH private key", "~/.ssh/id_ed25519")
							if loginKey == "~/.ssh/id_ed25519" {
								home, _ := os.UserHomeDir()
								loginKey = filepath.Join(home, ".ssh", "id_ed25519")
							}
						}
					}
				}
				if loginUser != "" && loginPort == 22 && interactive {
					lps := prompt("Login SSH port", "22")
					lpi, err := strconv.Atoi(lps)
					if err == nil {
						loginPort = lpi
					}
				}
				if loginUser != "" && !cmd.Flags().Changed("sudo") && interactive {
					s := prompt("Use sudo? (yes/no)", "no")
					useSudo = strings.HasPrefix(strings.ToLower(s), "y")
				}
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			if err := app.AddServer(core.ServerRecord{
				Name:    name,
				Address: address,
				Mode:    parsedMode,
				Port:    port,
				User:    user,
				KeyPath: keyPath,
			}); err != nil {
				return err
			}

			if loginUser != "" && parsedMode == transport.ModeDirect && !noAgent {
				lp := loginPort
				if lp == 0 {
					lp = 22
				}
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintf(cmd.OutOrStdout(), "The following will be installed on %s (%s:%d):\n", name, address, lp)
				fmt.Fprintln(cmd.OutOrStdout(), "  • fleet-agent binary  →  /opt/cenvero-fleet/fleet-agent")
				fmt.Fprintln(cmd.OutOrStdout(), "  • cenvero-fleet-agent.service  →  systemd unit (enabled on boot)")
				fmt.Fprintln(cmd.OutOrStdout(), "  • authorized_keys entry for this controller's public key")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "To skip, press Ctrl+C now or re-run with --no-agent.")
				fmt.Fprintf(cmd.OutOrStdout(), "Press Enter to continue: ")
				scanner.Scan()

				fmt.Fprintf(cmd.OutOrStdout(), "\nInstalling agent on %s...\n", name)
				if err := app.AutoInstallAgent(name, loginUser, loginKey, loginPassword, lp, useSudo); err != nil {
					return fmt.Errorf("agent install failed (server was added): %w\n"+
						"Run 'fleet server bootstrap %s --login-user %s' to retry manually", err, name, loginUser)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Agent installed and running. Connect with: fleet server reconnect %s\n", name)
			} else if parsedMode == transport.ModeDirect && (noAgent || loginUser == "") {
				record, _ := app.GetServer(name)
				fmt.Fprintln(cmd.OutOrStdout(), core.AgentInstallInstructions(record, parsedMode))
			}
			return nil
		},
	}
	add.Flags().StringVar(&mode, "mode", "direct", "server transport mode (direct/reverse)")
	add.Flags().IntVar(&port, "port", 2222, "agent SSH port (after install)")
	add.Flags().StringVar(&user, "user", "root", "agent SSH username")
	add.Flags().StringVar(&keyPath, "key", "", "SSH key path override")
	add.Flags().StringVar(&loginUser, "login-user", "", "login user for agent auto-install (e.g. root, ubuntu)")
	add.Flags().IntVar(&loginPort, "login-port", 22, "SSH port for login")
	add.Flags().StringVar(&loginKey, "login-key", "", "SSH private key for login")
	add.Flags().StringVar(&loginPassword, "login-password", "", "SSH password for login (use key auth when possible)")
	add.Flags().BoolVar(&useSudo, "sudo", false, "use sudo for agent install commands")
	add.Flags().BoolVar(&noAgent, "no-agent", false, "skip agent auto-install")

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
	removeCmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a server (and tear down its agent if managed)",
		Long: `Removes a server from the fleet.

If the agent is managed, fleet will SSH to the server using the stored login
credentials and remove the systemd service, binary, and state directory.

If the server is unreachable or credentials have changed, use one of:

  --force          Delete the server record without touching the agent.
  --via-ssh        Retry teardown with different SSH credentials.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			viaSSH, _ := cmd.Flags().GetBool("via-ssh")
			loginUser, _ := cmd.Flags().GetString("login-user")
			loginKey, _ := cmd.Flags().GetString("login-key")
			loginPassword, _ := cmd.Flags().GetString("login-password")
			loginPort, _ := cmd.Flags().GetInt("login-port")

			if viaSSH && loginUser == "" {
				// Prompt for missing login user interactively
				scanner := bufio.NewScanner(os.Stdin)
				fmt.Fprintf(cmd.OutOrStdout(), "Login user for SSH teardown: ")
				scanner.Scan()
				loginUser = strings.TrimSpace(scanner.Text())
				if loginUser == "" {
					return fmt.Errorf("--login-user is required with --via-ssh")
				}
			}
			if viaSSH && loginKey == "" && loginPassword == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Auth method — [K]ey or [P]assword [K]: ")
				scanner := bufio.NewScanner(os.Stdin)
				scanner.Scan()
				choice := strings.TrimSpace(scanner.Text())
				if strings.EqualFold(choice, "p") || strings.HasPrefix(strings.ToLower(choice), "p") {
					fmt.Fprintf(cmd.OutOrStdout(), "Password: ")
					if pw, err := term.ReadPassword(0); err == nil {
						loginPassword = string(pw)
						fmt.Fprintln(cmd.OutOrStdout())
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Path to SSH private key [~/.ssh/id_ed25519]: ")
					scanner.Scan()
					loginKey = strings.TrimSpace(scanner.Text())
					if loginKey == "" {
						loginKey = "~/.ssh/id_ed25519"
					}
					if strings.HasPrefix(loginKey, "~/") {
						home, _ := os.UserHomeDir()
						loginKey = filepath.Join(home, loginKey[2:])
					}
				}
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			return app.RemoveServerWithOptions(args[0], core.RemoveOptions{
				Force:         force,
				OverrideSSH:   viaSSH,
				LoginUser:     loginUser,
				LoginKey:      loginKey,
				LoginPassword: loginPassword,
				LoginPort:     loginPort,
			})
		},
	}
	removeCmd.Flags().Bool("force", false, "delete server record from controller without touching the agent")
	removeCmd.Flags().Bool("via-ssh", false, "retry agent teardown via SSH with the credentials below")
	removeCmd.Flags().String("login-user", "", "SSH login user for teardown (--via-ssh)")
	removeCmd.Flags().String("login-key", "", "SSH private key path for teardown (--via-ssh)")
	removeCmd.Flags().String("login-password", "", "SSH password for teardown (--via-ssh)")
	removeCmd.Flags().Int("login-port", 0, "SSH port for teardown (--via-ssh, default 22)")
	serverCmd.AddCommand(removeCmd)
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
			return os.WriteFile(exportPath, append(data, '\n'), 0o600)
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
	configCmd.AddCommand(&cobra.Command{
		Use:   "agent-port <port>",
		Short: "Set the default agent SSH port used when installing agents",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := strconv.Atoi(args[0])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %q: must be 1-65535", args[0])
			}
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			app.Config.Runtime.DefaultAgentPort = port
			if err := core.SaveConfig(core.ConfigPath(*configDir), app.Config); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "default agent port set to %d\n", port)
			return nil
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
			latestVersion, binary, err := manifest.BinaryFor(app.Config.Updates.Channel, false)
			if err != nil {
				return err
			}
			if core.RuntimeIsHomebrewInstall() {
				fmt.Fprintf(cmd.OutOrStdout(), "Current version : %s\n", version.Version)
				fmt.Fprintf(cmd.OutOrStdout(), "Latest version  : %s\n", latestVersion)
				if latestVersion != version.Version {
					fmt.Fprintln(cmd.OutOrStdout())
					fmt.Fprintln(cmd.OutOrStdout(), "A newer version is available. To update, run:")
					fmt.Fprintln(cmd.OutOrStdout())
					fmt.Fprintln(cmd.OutOrStdout(), "  brew update && brew upgrade cenvero-fleet")
					fmt.Fprintln(cmd.OutOrStdout())
					fmt.Fprintln(cmd.OutOrStdout(), "Agents on managed servers will be updated automatically once")
					fmt.Fprintln(cmd.OutOrStdout(), "you run fleet update apply after upgrading.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "You are on the latest version.")
				}
				return nil
			}
			return writeJSON(cmd, map[string]any{"version": latestVersion, "binary": binary})
		},
	})
	var targetServers []string
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply the latest controller update and roll it out across managed agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if core.RuntimeIsHomebrewInstall() {
				fmt.Fprintln(cmd.OutOrStdout(), "Controller is managed by Homebrew — the controller binary cannot be")
				fmt.Fprintln(cmd.OutOrStdout(), "self-updated. Use Homebrew to update it:")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "  brew update && brew upgrade cenvero-fleet")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "After upgrading, run 'fleet update apply' again to roll out the")
				fmt.Fprintln(cmd.OutOrStdout(), "new agent version to your managed servers.")
				return nil
			}
			app, err := openApp(*configDir)
			if err != nil {
				// Config not available — can still self-update the binary only.
				// This happens when running as root (sudo) and the config dir is
				// under a different user's home. Suggest --config-dir.
				if !core.IsInitialized(*configDir) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Could not open fleet config at %s.\n\n", *configDir)
					// When running with sudo, $HOME is root's home but SUDO_USER
					// and SUDO_HOME (set by some distros) point to the real user.
					if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
						suggestedDir := core.DefaultConfigDir(os.Getenv("SUDO_HOME"))
						if suggestedDir == *configDir || os.Getenv("SUDO_HOME") == "" {
							suggestedDir = fmt.Sprintf("/home/%s/.cenvero-fleet", sudoUser)
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "You appear to be running with sudo. Pass --config-dir:\n\n")
						fmt.Fprintf(cmd.ErrOrStderr(), "  sudo fleet --config-dir %s update apply\n\n", suggestedDir)
					}
					return err
				}
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
			if core.RuntimeIsHomebrewInstall() {
				fmt.Fprintln(cmd.ErrOrStderr(), "Controller is managed by Homebrew — rollback is handled by Homebrew.")
				fmt.Fprintln(cmd.ErrOrStderr(), "To roll back: brew install cenvero/fleet/cenvero-fleet@<version>")
				return fmt.Errorf("rollback not available for Homebrew installs")
			}
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
			if core.RuntimeIsHomebrewInstall() {
				fmt.Fprintln(cmd.ErrOrStderr(), "Controller is managed by Homebrew — the update channel is determined")
				fmt.Fprintln(cmd.ErrOrStderr(), "by which Homebrew tap/formula you have installed.")
				return fmt.Errorf("update channel not configurable for Homebrew installs")
			}
			return app.UpdateChannel(args[0])
		},
	})
	return updateCmd
}

func newBackupCommand(configDir *string) *cobra.Command {
	var outputPath string
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Back up the fleet config directory to a tar.gz archive",
		Long: `Creates a compressed archive of the entire fleet configuration directory:
server records, keys, audit logs, and SQLite database files.

Ephemeral files (lock files, WAL journals, in-progress temp files) are excluded.
The archive can be used to restore or migrate a fleet controller installation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if *configDir == "" {
				*configDir = core.DefaultConfigDir("")
			}
			if !core.IsInitialized(*configDir) {
				return fmt.Errorf("fleet is not initialized at %s — nothing to back up", *configDir)
			}
			result, err := core.Backup(*configDir, core.BackupOptions{OutputPath: outputPath})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup created: %s\n", result.OutputPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  %d files, %s\n", result.FilesCount, formatBytes(result.SizeBytes))
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output path for the backup archive (default: fleet-backup-<timestamp>.tar.gz)")
	return cmd
}

func newSelfUninstallCommand(configDir *string) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "self-uninstall",
		Short: "Remove fleet binary and config directory",
		Long: `Removes the fleet binary from its install location and deletes
the local config directory (servers, keys, logs, etc.).

All managed agents on remote servers are left untouched.
Run 'fleet server remove <name>' first if you want to tear those down.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprintln(cmd.OutOrStdout(), "This will remove:")
				fmt.Fprintln(cmd.OutOrStdout(), "  • the fleet binary from its install location")
				fmt.Fprintln(cmd.OutOrStdout(), "  • the fleet config directory (servers, keys, logs, etc.)")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "Remote agents on managed servers are NOT affected.")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "Re-run with --yes to confirm.")
				return nil
			}

			if *configDir == "" {
				*configDir = core.DefaultConfigDir("")
			}

			// Remove config directory
			if err := os.RemoveAll(*configDir); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove config dir %s: %v\n", *configDir, err)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed config directory: %s\n", *configDir)
			}

			// Find and remove the fleet binary
			binaryPath, err := exec.LookPath("fleet")
			if err != nil {
				// Try the path of the running executable as fallback
				binaryPath, err = os.Executable()
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not locate fleet binary; remove it manually")
					return nil
				}
				binaryPath, _ = filepath.EvalSymlinks(binaryPath)
			}

			if err := os.Remove(binaryPath); err != nil {
				if os.IsPermission(err) {
					// Try with sudo on unix
					sudoErr := exec.Command("sudo", "rm", "-f", binaryPath).Run()
					if sudoErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove %s (permission denied). Run: sudo rm -f %s\n", binaryPath, binaryPath)
						return nil
					}
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not remove %s: %v\n", binaryPath, err)
					return nil
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed binary: %s\n", binaryPath)
			fmt.Fprintln(cmd.OutOrStdout(), "Fleet uninstalled.")
			return nil
		},
		Args: cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm uninstall")
	return cmd
}

func newReportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Show where to report bugs and get support",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "Cenvero Fleet — bug reports & support")
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "  GitHub Issues  https://github.com/cenvero/fleet/issues")
			fmt.Fprintln(cmd.OutOrStdout(), "  Email          support@cenvero.org")
			fmt.Fprintln(cmd.OutOrStdout(), "  Docs           https://fleet.cenvero.org/docs")
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "When reporting, please include:")
			fmt.Fprintf(cmd.OutOrStdout(), "  • Fleet version  fleet version\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  • OS and arch    %s\n", goRuntimeInfo())
			fmt.Fprintln(cmd.OutOrStdout(), "  • Steps to reproduce the issue")
			fmt.Fprintln(cmd.OutOrStdout(), "  • Relevant logs  fleet logs audit")
			return nil
		},
	}
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

func goRuntimeInfo() string {
	return runtime.GOOS + "/" + runtime.GOARCH
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
	return os.WriteFile(exportPath, []byte(output), 0o600)
}

func followServiceLogs(ctx context.Context, cmd *cobra.Command, app *core.App, serverName, serviceName, search string, tailLines int, exportPath string) error {
	if exportPath != "" {
		if err := os.WriteFile(exportPath, nil, 0o600); err != nil {
			return err
		}
	}
	return app.FollowServiceLogs(ctx, serverName, serviceName, search, tailLines, core.DefaultLogFollowInterval, func(line proto.LogLine) error {
		formatted := fmt.Sprintf("%6d  %s\n", line.Number, line.Text)
		if exportPath == "" {
			_, err := fmt.Fprint(cmd.OutOrStdout(), formatted)
			return err
		}
		f, err := os.OpenFile(exportPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(formatted)
		return err
	})
}

func newExecCommand(configDir *string) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "exec <server> <command>",
		Short: "Run a shell command on one server or all servers",
		Long: `Run a shell command on a managed server via the fleet agent.

Examples:
  fleet exec web-01 uptime
  fleet exec web-01 "df -h /"
  fleet exec --all uptime`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			if all {
				command := strings.Join(args, " ")
				results := app.ExecCommandAll(command)
				for _, r := range results {
					if r.Error != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "=== %s [error] ===\n%s\n", r.Server, r.Error)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "=== %s [exit %d] ===\n", r.Server, r.Result.ExitCode)
					if r.Result.Stdout != "" {
						fmt.Fprint(cmd.OutOrStdout(), r.Result.Stdout)
					}
					if r.Result.Stderr != "" {
						fmt.Fprint(cmd.ErrOrStderr(), r.Result.Stderr)
					}
				}
				return nil
			}

			if len(args) < 2 {
				return fmt.Errorf("usage: fleet exec <server> <command>")
			}
			serverName := args[0]
			command := strings.Join(args[1:], " ")
			result, err := app.ExecCommand(serverName, command)
			if err != nil {
				return err
			}
			if result.Stdout != "" {
				fmt.Fprint(cmd.OutOrStdout(), result.Stdout)
			}
			if result.Stderr != "" {
				fmt.Fprint(cmd.ErrOrStderr(), result.Stderr)
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("exit status %d", result.ExitCode)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "run on all servers concurrently")
	return cmd
}

func newSyncAgentCommand(configDir *string) *cobra.Command {
	var targetServers []string
	cmd := &cobra.Command{
		Use:   "sync-agent",
		Short: "Sync agent version to match the controller, restarting the service if updated",
		Long: `Checks the agent version on every managed server (or the servers you specify
with --server) against the currently installed controller version.

If the versions differ, the latest agent binary is downloaded on the remote
server, verified, and the agent service is restarted automatically.
Servers already running the correct version are skipped.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			result, err := app.SyncAgent(cmd.Context(), targetServers)
			if err != nil {
				return err
			}
			return writeJSON(cmd, result)
		},
	}
	cmd.Flags().StringArrayVar(&targetServers, "server", nil, "sync only the specified server(s) instead of all")
	return cmd
}

func newSSHCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <server>",
		Short: "Open an interactive root shell on a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			return app.RunSSHSession(args[0], cmd.OutOrStdout())
		},
	}
}

func newRecoverCommand(configDir *string) *cobra.Command {
	var (
		fromDir    string
		dbBackend  string
		dbDSN      string
		skipVerify bool
	)
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Re-attach fleet to an existing config directory after a reinstall or migration",
		Long: `Recover re-points this fleet installation to an existing config directory.
Use this after reinstalling the OS, moving to a new machine, or after a
broken upgrade that left the binary without its config.

For SQLite (the default), fleet checks that the database files in the
config dir still exist. For PostgreSQL/MySQL/MariaDB, it connects with
the DSN from the existing config and verifies connectivity.

If the controller version does not match what was last used with that
config, fleet will tell you which version to downgrade to before proceeding.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fromDir == "" {
				return fmt.Errorf("--from-dir is required: specify the existing fleet config directory")
			}
			return core.Recover(core.RecoverOptions{
				TargetConfigDir: *configDir,
				FromDir:         fromDir,
				DBBackend:       dbBackend,
				DBDSN:           dbDSN,
				SkipVersionCheck: skipVerify,
			}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&fromDir, "from-dir", "", "existing fleet config directory to recover from (required)")
	cmd.Flags().StringVar(&dbBackend, "db-backend", "", "override the database backend (sqlite, postgres, mysql, mariadb)")
	cmd.Flags().StringVar(&dbDSN, "db-dsn", "", "override the database DSN (for postgres/mysql/mariadb)")
	cmd.Flags().BoolVar(&skipVerify, "skip-version-check", false, "skip the version compatibility check (not recommended)")
	return cmd
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
