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
	"github.com/cenvero/fleet/internal/webui"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	var configDir string
	var tokenID string

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
				"backup", "recover", "adjust-init",
				// context/ai/skill describe the CLI and install agent integrations;
				// they never touch controller state, so they must work pre-init.
				"context", "ai", "skill",
				// shell helpers + local-store commands operate on local files only
				// (config dir + shell rc), so they work before init.
				"automation", "shell-init", "autocomplete",
				// `token` (bare) only touches tokens.json in the config dir, so it
				// works before full init (FL-030).
				"token",
				"jobs", "cmd-policy", "approvals", "approve":
				return enforceToken(cmd, configDir, tokenID)
			}
			if cmd.HasParent() {
				switch cmd.Parent().Name() {
				case "help", "completion", "update", "skill", "automation", "autocomplete":
					return nil
				// token subcommands (create/list/revoke) only touch tokens.json, so
				// they work before full init (FL-030).
				case "token":
					return enforceToken(cmd, configDir, tokenID)
				}
			}
			if !core.IsInitialized(configDir) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Fleet is not initialized yet.\n")
				fmt.Fprintf(cmd.ErrOrStderr(), "Run 'fleet init' first to set up the controller.\n")
				os.Exit(1)
			}
			// Check for pending config migrations and show a one-line hint.
			if cfg, err := core.LoadConfig(core.ConfigPath(configDir)); err == nil {
				if hint := core.AdjustInitHint(cfg); hint != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "\n⚠  %s\n\n", hint)
				}
				// Background Homebrew update hint.
				if core.RuntimeIsHomebrewInstall() {
					if hint := core.HomebrewUpdateHint(configDir, cfg.ManifestURL, cfg.Updates.Policy); hint != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "\nUpdate available (%s). To upgrade:\n\n  brew update && brew upgrade cenvero-fleet\n\n", hint)
					}
				}
				// Stamp last-seen version so 'fleet recover' can detect mismatches.
				core.StampLastSeenVersion(core.ConfigPath(configDir), cfg)
			}
			// FL-030: controller-side RBAC enforcement. After the init check (so a
			// scoped token can resolve servers/groups), if a token is presented,
			// load it and authorize this invocation against its scope.
			return enforceToken(cmd, configDir, tokenID)
		},
	}

	root.Version = version.Version
	root.PersistentFlags().StringVar(&configDir, "config-dir", "", "configuration directory")
	root.PersistentFlags().StringVar(&tokenID, "token", "", "scoped RBAC token id (FL-030); falls back to FLEET_TOKEN")
	root.SetVersionTemplate("Cenvero Fleet {{.Version}}\n")

	root.AddCommand(newInitCommand(&configDir))
	root.AddCommand(newStatusCommand(&configDir))
	root.AddCommand(newLifecycleCommand("start", &configDir))
	root.AddCommand(newLifecycleCommand("stop", &configDir))
	root.AddCommand(newLifecycleCommand("daemon", &configDir))
	root.AddCommand(newDashboardCommand(&configDir))
	root.AddCommand(newFilesCommand(&configDir))
	root.AddCommand(newFileManagerCommand(&configDir))
	root.AddCommand(newServerCommand(&configDir))
	root.AddCommand(newServiceCommand(&configDir))
	root.AddCommand(newFileCommand(&configDir))
	root.AddCommand(newSyncCommand(&configDir))
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
	root.AddCommand(newAdjustInitCommand(&configDir))
	root.AddCommand(newSelfUninstallCommand(&configDir))
	root.AddCommand(newReportCommand())
	root.AddCommand(newContextCommand())
	root.AddCommand(newAutomationCommand(&configDir))
	root.AddCommand(newShellInitCommand())
	root.AddCommand(newAutocompleteCommand())
	root.AddCommand(newTagCommand(&configDir))
	root.AddCommand(newTokenCommand(&configDir))
	root.AddCommand(newInventoryCommand(&configDir))
	root.AddCommand(newServiceStatusCommand(&configDir))
	root.AddCommand(newJournalCommand(&configDir))
	root.AddCommand(newTopCommand(&configDir))
	root.AddCommand(newCpCommand(&configDir))
	root.AddCommand(newNotifyCommand(&configDir))
	root.AddCommand(newCronCommand(&configDir))
	root.AddCommand(newDriftCommand(&configDir))
	root.AddCommand(newPolicyCommand(&configDir))
	agentCmd := &cobra.Command{Use: "agent", Short: "Manage fleet agents (version, consistency)"}
	agentCmd.AddCommand(newAgentVersionCommand(&configDir))
	root.AddCommand(agentCmd)
	root.AddCommand(newRunCommand(&configDir))
	root.AddCommand(newGuardCommand(&configDir))
	root.AddCommand(newConfirmCommand(&configDir))
	root.AddCommand(newRevertCommand(&configDir))
	root.AddCommand(newDoctorCommand(&configDir))
	root.AddCommand(newJobCommand(&configDir))
	root.AddCommand(newJobsListCommand(&configDir))
	root.AddCommand(newCmdPolicyCommand(&configDir))
	root.AddCommand(newHealthCommand(&configDir))
	root.AddCommand(newFirewallSafeCommand(&configDir))
	root.AddCommand(newApprovalsCommand(&configDir))
	root.AddCommand(newApproveCommand(&configDir))
	root.AddCommand(newAICommand())
	root.AddCommand(newSkillCommand())
	return root
}

// enforceToken implements FL-030 CONTROLLER-side RBAC enforcement.
//
// When a scoped token is presented (via --token or the FLEET_TOKEN env var) it
// is loaded from the TokenStore and this invocation is authorized against the
// token's scope. If the token is unknown/revoked or the invocation is out of
// scope, a clear error is printed to stderr and the process exits 1.
//
// This is controller-side enforcement only: it constrains what THIS controller
// process will attempt. Agent-side enforcement — where the agent validates the
// presented token per-RPC and refuses out-of-scope work even if the controller
// is patched or bypassed — is a future hardening (FL-030 server-side).
func enforceToken(cmd *cobra.Command, configDir, tokenFlag string) error {
	tokenID := strings.TrimSpace(tokenFlag)
	if tokenID == "" {
		tokenID = strings.TrimSpace(os.Getenv("FLEET_TOKEN"))
	}
	if tokenID == "" {
		return nil // no token presented: unscoped invocation
	}

	store := core.NewTokenStore(configDir)
	token, err := store.Get(tokenID)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "denied: unknown or revoked token")
		os.Exit(1)
	}

	top, sub := topLevelCommand(cmd)

	// A scoped token must never be able to mint or modify tokens: that would let
	// a constrained credential escalate its own (or others') scope.
	if top == "token" && (sub == "create" || sub == "revoke") {
		fmt.Fprintf(cmd.ErrOrStderr(), "denied: a scoped token cannot run 'token %s'\n", sub)
		os.Exit(1)
	}

	args := commandPositionalArgs(cmd)
	targetServer := bestEffortTargetServer(top, args)
	isDestructive := core.IsDestructiveCommand(top, args)

	// RBAC v1 enforces a single best-effort target server. Cross-server commands
	// (cp, file copy/move/diff) touch two servers the single-target check cannot
	// fully vet, so a server-scoped token is DENIED them (fail-closed) rather than
	// allowed to slip past the scope check with an empty/partial target.
	if (len(token.Servers) > 0 || len(token.Groups) > 0) &&
		(top == "cp" || (top == "file" && (sub == "copy" || sub == "move" || sub == "diff"))) {
		fmt.Fprintf(cmd.ErrOrStderr(), "denied: a server-scoped token cannot run cross-server %q in RBAC v1\n", strings.TrimSpace(top+" "+sub))
		os.Exit(1)
	}

	var allServerNames []string
	if core.IsInitialized(configDir) && len(token.Groups) > 0 {
		if app, aerr := openApp(configDir); aerr == nil {
			if servers, serr := app.ListServers(); serr == nil {
				for _, s := range servers {
					allServerNames = append(allServerNames, s.Name)
				}
			}
			_ = app.Close()
		}
	}
	tags := core.NewTagStore(configDir)

	if authErr := core.Authorize(token, top, targetServer, isDestructive, allServerNames, tags); authErr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), authErr.Error())
		os.Exit(1)
	}
	return nil
}

// topLevelCommand walks cmd up to the first-level command under root and returns
// its name plus the immediate subcommand name (the leaf, when it differs from
// the top-level command). For `fleet server remove web-01` it returns
// ("server", "remove"); for `fleet status` it returns ("status", "").
func topLevelCommand(cmd *cobra.Command) (top, sub string) {
	chain := []*cobra.Command{}
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		chain = append([]*cobra.Command{c}, chain...)
	}
	if len(chain) == 0 {
		return cmd.Name(), ""
	}
	top = chain[0].Name()
	if len(chain) > 1 {
		sub = chain[1].Name()
	}
	return top, sub
}

// commandPositionalArgs returns the positional (non-flag) args for cmd.
func commandPositionalArgs(cmd *cobra.Command) []string {
	if cmd.Flags() != nil {
		return cmd.Flags().Args()
	}
	return nil
}

// serverFirstArgCommands is the set of top-level commands whose first positional
// argument is conventionally a server name. Used for the best-effort target
// resolution in token enforcement; it is deliberately conservative.
// serverSecondArgCommands are top-level commands that take a SUBCOMMAND as
// their first positional and the server as the SECOND (e.g. `file rm <server>
// <path>`, `service start <server> <name>`, `server remove <server>`,
// `firewall enable <server>`, `port open <server> <port>`).
var serverSecondArgCommands = map[string]bool{
	"server":   true,
	"service":  true,
	"file":     true,
	"firewall": true,
	"fw":       true,
	"port":     true,
}

// serverFirstArgCommands are top-level commands whose FIRST positional argument
// is conventionally a server name (e.g. `exec <server> <cmd>`, `journal
// <server>`, `guard <server> <cmd>`, `drift <server>`, `svc <server>`).
var serverFirstArgCommands = map[string]bool{
	"exec":    true,
	"journal": true,
	"guard":   true,
	"drift":   true,
	"svc":     true,
}

// bestEffortTargetServer extracts the targeted server name for a command, if it
// can be determined cheaply. It is conservative: commands not known to take a
// server as a positional arg (e.g. `health`/`top` which use flags, `revert
// <id>` which takes an id, `cp <src:path> <dst:path>` which encodes servers in
// a colon form) return "" so no server-scope check is applied.
func bestEffortTargetServer(top string, args []string) string {
	switch {
	case serverSecondArgCommands[top]:
		if len(args) >= 2 {
			return args[1]
		}
	case serverFirstArgCommands[top]:
		if len(args) >= 1 {
			return args[0]
		}
	}
	return ""
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

func newFilesCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "files [source...]",
		Short: "Launch the desktop-grade dual-pane file manager",
		Long: "Open a full-screen, desktop-application-grade dual-pane file manager. Each\n" +
			"pane has a source: the local filesystem (\"Local\") or a managed server, so you\n" +
			"can browse and transfer local↔server AND server↔server.\n\n" +
			"  fleet files          Local on the left, the first server on the right\n" +
			"  fleet files a        server 'a' on the left, Local on the right\n" +
			"  fleet files a b      server 'a' on the left, server 'b' on the right\n\n" +
			"Single-click selects, double-click / Enter / → opens a folder, ← goes up.\n" +
			"Drag between panes to copy or move (Finder-style menu), right-click for a\n" +
			"context menu, and use the toolbar or keyboard for every operation. Transfers\n" +
			"use the same chunked, resumable engine as 'fleet file'.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.RunFiles(*configDir, args...)
		},
	}
}

// newFileManagerCommand is the friendly-named entry point: `fleet filemanager`
// (alias `fm`) opens the same terminal file manager as `fleet files`, and
// `fleet filemanager ui` launches the browser file manager (like `fleet file ui`).
func newFileManagerCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "filemanager [source...]",
		Aliases: []string{"fm"},
		Short:   "Dual-pane file manager (terminal); 'filemanager ui' opens the web UI",
		Long: "Friendly alias of `fleet files`: open the desktop-grade dual-pane terminal file\n" +
			"manager. Run `fleet filemanager ui` to launch the localhost browser file manager\n" +
			"instead (same as `fleet file ui`).",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.RunFiles(*configDir, args...)
		},
	}
	cmd.AddCommand(newUICommand(configDir))
	return cmd
}

func newUICommand(configDir *string) *cobra.Command {
	var addr string
	var open string
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Launch the localhost web file manager (browser, drag-and-drop)",
		Long: "Serve a browser-based file manager from the controller. It binds loopback only,\n" +
			"requires a per-process token printed at startup, restricts mutations to POST\n" +
			"with an Origin/CSRF check, and sets a strict CSP — there is no remote-reachable\n" +
			"surface. Browse a server, drag files from your desktop onto the page to upload\n" +
			"them (with live progress), and click to download. Prompts to open your browser\n" +
			"when interactive; use --open=yes/no to skip the prompt.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			srv, err := webui.New(app)
			if err != nil {
				return err
			}
			// Decide whether to open the browser BEFORE installing the signal
			// handler, so Ctrl-C during the prompt still quits normally.
			shouldOpen := decideOpenBrowser(cmd, open)

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			var onReady func(string)
			if shouldOpen {
				out := cmd.OutOrStdout()
				onReady = func(url string) {
					if err := openBrowser(url); err != nil {
						fmt.Fprintf(out, "could not open a browser automatically (%v) — open the link above manually\n", err)
					}
				}
			}
			return srv.ListenAndServe(ctx, addr, cmd.OutOrStdout(), onReady)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", webui.DefaultAddr, "loopback bind address for the web UI")
	cmd.Flags().StringVar(&open, "open", "auto", "open the web UI in a browser: auto (prompt when interactive), yes, or no")
	return cmd
}

// decideOpenBrowser resolves the --open flag: "yes"/"no" are explicit; "auto"
// prompts on an interactive terminal and otherwise declines (just show the link).
func decideOpenBrowser(cmd *cobra.Command, mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "yes", "y", "true":
		return true
	case "no", "n", "false":
		return false
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Fprint(cmd.OutOrStdout(), "Open the web UI in your browser now? [Y/n]: ")
	answer := strings.ToLower(strings.TrimSpace(transport.ReadLine(bufio.NewReader(os.Stdin))))
	return answer == "" || answer == "y" || answer == "yes"
}

// openBrowser opens url in the platform default browser.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	return exec.Command(name, args...).Start()
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
		Short: "Rotate controller keys and roll them out to all managed servers (direct and reverse)",
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
				fmt.Fprintf(cmd.OutOrStdout(), "Current version : %s\n", version.Canonical(version.Version))
				fmt.Fprintf(cmd.OutOrStdout(), "Latest version  : %s\n", version.Canonical(latestVersion))
				if version.Canonical(latestVersion) != version.Canonical(version.Version) {
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
			// For Homebrew installs, tell the user how to update the controller
			// binary, but DO NOT return early — ApplyFleetUpdate skips the
			// self-update step internally and still rolls out to agents.
			if core.RuntimeIsHomebrewInstall() {
				fmt.Fprintln(cmd.OutOrStdout(), "Controller is managed by Homebrew — to update the controller binary:")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "  brew update && brew upgrade cenvero-fleet")
				fmt.Fprintln(cmd.OutOrStdout())
				fmt.Fprintln(cmd.OutOrStdout(), "Continuing to roll out the agent update to managed servers...")
				fmt.Fprintln(cmd.OutOrStdout())
			}
			app, err := openApp(*configDir)
			if err != nil {
				if !core.IsInitialized(*configDir) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Could not open fleet config at %s.\n\n", *configDir)
					// On Linux, sudo changes $HOME so config is not found.
					// macOS users use Homebrew (handled above) so this is Linux-only.
					if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
						suggestedDir := core.DefaultConfigDir(os.Getenv("SUDO_HOME"))
						if suggestedDir == *configDir || os.Getenv("SUDO_HOME") == "" {
							suggestedDir = "/home/" + sudoUser + "/.cenvero-fleet"
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "You appear to be running with sudo. Pass --config-dir:\n\n")
						fmt.Fprintf(cmd.ErrOrStderr(), "  sudo fleet --config-dir %s update apply\n\n", suggestedDir)
					}
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

// execJSON is the --json shape: remote stdout/stderr/exit_code stay distinct, and
// fleet-layer/transport failures land in agent_error so they never pollute stdout.
type execJSON struct {
	Server     string `json:"server"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	AgentError string `json:"agent_error,omitempty"`
}

// classifyAgentError labels a transport/agent failure: unreachable | auth | agent-error.
func classifyAgentError(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "dial"), strings.Contains(s, "refused"), strings.Contains(s, "no route"),
		strings.Contains(s, "unreachable"), strings.Contains(s, "timeout"), strings.Contains(s, "i/o"):
		return "unreachable"
	case strings.Contains(s, "auth"), strings.Contains(s, "unauthorized"), strings.Contains(s, "permission"),
		strings.Contains(s, "host key"), strings.Contains(s, "handshake"):
		return "auth"
	default:
		return "agent-error"
	}
}

func newExecCommand(configDir *string) *cobra.Command {
	var all, asJSON, propagateExit bool
	var timeout, backoff time.Duration
	var retries int
	// Exec-time enforcement flags (FL-003/007/008/010/012/013/027/032).
	var (
		dryRun         bool
		onFail         string
		group          string
		guard          bool
		guardWarn      bool
		confirm        bool
		requireApprove bool
		idempotencyKey string
	)
	cmd := &cobra.Command{
		Use:   "exec <server> <command>",
		Short: "Run a shell command on one server or all servers",
		Long: `Run a shell command on a managed server via the fleet agent.

Flags:
  --json            structured output: {stdout, stderr, exit_code, duration_ms, timed_out, agent_error}
  --timeout 30s     abort the command after a duration (reports timed_out)
  --retry N         retry ONLY transport failures (never re-runs a command that ran)
  --backoff 2s      delay between transport retries
  --propagate-exit  exit the fleet process with the remote command's exit code

Enforcement flags:
  --dry-run             print 'would run: <cmd>' for the target(s) and exit without running
  --group EXPR          run on all servers whose tags match EXPR (like --all, filtered)
  --guard               block the command if it could lock out the controller
  --guard-warn          downgrade --guard to a warning (run anyway)
  --confirm             confirm a command that the cmd-policy marks confirm-required
  --require-approval    stage the command for approval instead of running it
  --idempotency-key KEY return the cached result for KEY instead of re-running
  --on-fail '<cmd>'     run this command on the same server if the command fails

Examples:
  fleet exec web-01 uptime
  fleet exec web-01 --json --timeout 10s "df -h /"
  fleet exec --all --json uptime
  fleet exec --group role=web uptime`,
		Args:         cobra.MinimumNArgs(1),
		SilenceUsage: true, // a remote non-zero exit / transport error must not dump usage text
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			// Enforcement stores are loaded best-effort: a missing/fresh file is
			// treated as "no policy" so plain `fleet exec` never breaks. The two
			// stores whose ctor can error (cmd-policy, redact) are nil on error.
			var cmdPolicy *core.CmdPolicyStore
			if p, perr := core.NewCmdPolicyStore(*configDir); perr == nil {
				cmdPolicy = p
			}
			var redactor *core.RedactStore
			if rstore, rerr := core.NewRedactStore(*configDir); rerr == nil {
				redactor = rstore
			}
			approvals := core.NewApprovalStore(*configDir)
			idemStore := core.NewIdempotencyStore(*configDir)
			tagStore := core.NewTagStore(*configDir)

			// redact applies the configured output redaction (no-op when unset).
			redact := func(s string) string {
				if redactor == nil {
					return s
				}
				return redactor.Redact(s)
			}

			// agentPortFor resolves a server's agent SSH port for the safety guard,
			// matching firewallsafe.go: ServerRecord.Port, then the controller
			// default, then 2222. A lookup failure yields 0 (AnalyzeCommandSafety
			// then assumes 2222 itself).
			agentPortFor := func(server string) int {
				record, gerr := app.GetServer(server)
				if gerr != nil {
					return 0
				}
				if record.Port != 0 {
					return record.Port
				}
				if app.Config.Runtime.DefaultAgentPort != 0 {
					return app.Config.Runtime.DefaultAgentPort
				}
				return 2222
			}

			// runOnce applies the optional controller-side timeout to a single attempt.
			// TODO: agent-side kill so the remote process also stops on timeout.
			runOnce := func(server, command string) (proto.ExecResult, bool, error) {
				if timeout <= 0 {
					r, e := app.ExecCommand(server, command)
					return r, false, e
				}
				type out struct {
					r proto.ExecResult
					e error
				}
				ch := make(chan out, 1)
				go func() { r, e := app.ExecCommand(server, command); ch <- out{r, e} }()
				select {
				case o := <-ch:
					return o.r, false, o.e
				case <-time.After(timeout):
					return proto.ExecResult{}, true, fmt.Errorf("timed out after %s", timeout)
				}
			}
			// run retries only transport-class failures (agentErr set, not a timeout,
			// and the command never reached execution), never a command that ran.
			run := func(server, command string) (res proto.ExecResult, timedOut bool, agentErr error, dur time.Duration) {
				for attempt := 0; ; attempt++ {
					start := time.Now()
					res, timedOut, agentErr = runOnce(server, command)
					dur = time.Since(start)
					if agentErr == nil || timedOut || attempt >= retries {
						return
					}
					time.Sleep(backoff)
				}
			}
			toJSON := func(server string, r proto.ExecResult, timedOut bool, agentErr error, dur time.Duration) execJSON {
				j := execJSON{Server: server, Stdout: redact(r.Stdout), Stderr: redact(r.Stderr), ExitCode: r.ExitCode, DurationMs: dur.Milliseconds(), TimedOut: timedOut}
				if agentErr != nil {
					j.AgentError = agentErr.Error()
				}
				return j
			}

			// preflight runs the policy/safety checks that must pass before a command
			// executes on a server, in the fixed order:
			//   deny-list -> guard -> confirm-required -> require-approval.
			// It returns (blocked=true, err) to refuse the command, or
			// (false, nil) to proceed. A non-nil err on block carries the reason.
			preflight := func(server, command string) (bool, error) {
				// 1. deny-list (always on).
				if cmdPolicy != nil {
					if denied, pat := cmdPolicy.MatchDeny(command); denied {
						return true, fmt.Errorf("command blocked by cmd-policy deny pattern %q", pat)
					}
				}
				// 2. guard — detect self-lockout risk.
				if guard || guardWarn {
					if warnings := core.AnalyzeCommandSafety(command, agentPortFor(server)); len(warnings) > 0 {
						for _, w := range warnings {
							fmt.Fprintf(cmd.ErrOrStderr(), "guard [%s]: %s\n", server, w)
						}
						if !guardWarn {
							return true, fmt.Errorf("command blocked by --guard on %s (pass --guard-warn to run anyway)", server)
						}
					}
				}
				// 3. confirm-required.
				if cmdPolicy != nil {
					if needs, pat := cmdPolicy.MatchConfirm(command); needs && !confirm {
						return true, fmt.Errorf("command matches cmd-policy confirm pattern %q — pass --confirm to run it", pat)
					}
				}
				// 4. require-approval — stage and refuse.
				if requireApprove {
					id, serr := approvals.Stage(server, command, core.DefaultApprovalTTL)
					if serr != nil {
						return true, fmt.Errorf("stage approval: %w", serr)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "staged approval %s for %s — run: fleet approve %s\n", id, server, id)
					return true, nil
				}
				return false, nil
			}

			// execOne runs the full per-server pipeline for one target and prints the
			// result in human mode (used by single-server and --all/--group human
			// paths). It returns the execJSON it produced (for --json aggregation),
			// a skip flag (preflight short-circuited: approval staged, dry-run, or
			// idempotency hit), and a fatal error (deny/guard/confirm block).
			execOne := func(server, command string, printHeader, human bool) (execJSON, bool, error) {
				if blocked, berr := preflight(server, command); blocked {
					return execJSON{}, true, berr
				}
				// idempotency-hit: return the cached result instead of running.
				if idempotencyKey != "" {
					if cached, ok := idemStore.Get(idempotencyKey); ok {
						if human {
							fmt.Fprintf(cmd.OutOrStdout(), "idempotency-key %s: cached result\n%s\n", idempotencyKey, redact(cached))
						}
						var cj execJSON
						if uerr := json.Unmarshal([]byte(cached), &cj); uerr == nil {
							cj.Stdout = redact(cj.Stdout)
							cj.Stderr = redact(cj.Stderr)
							return cj, true, nil
						}
						return execJSON{Server: server, Stdout: redact(cached)}, true, nil
					}
				}
				// dry-run: print the resolved command and skip execution.
				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "would run: %s [%s]\n", command, server)
					return execJSON{Server: server}, true, nil
				}
				// run, then redact, then handle on-fail.
				r, timedOut, agentErr, dur := run(server, command)
				j := toJSON(server, r, timedOut, agentErr, dur)
				if idempotencyKey != "" {
					if data, merr := json.Marshal(j); merr == nil {
						_ = idemStore.Put(idempotencyKey, string(data), time.Hour)
					}
				}
				// on-fail: a non-zero exit, timeout, or transport error triggers it.
				failed := agentErr != nil || timedOut || r.ExitCode != 0
				if onFail != "" && failed {
					or, oTimedOut, oAgentErr, oDur := run(server, onFail)
					oj := toJSON(server, or, oTimedOut, oAgentErr, oDur)
					if human {
						printExecHuman(cmd, j, printHeader)
						fmt.Fprintf(cmd.OutOrStdout(), "--- on-fail: %s ---\n", onFail)
						printExecHuman(cmd, oj, false)
					}
					return j, false, nil
				}
				if human {
					printExecHuman(cmd, j, printHeader)
				}
				return j, false, nil
			}

			// resolveTargets returns the server set for --all / --group, or nil when
			// neither is set. --group takes precedence over --all when both are given.
			resolveTargets := func() ([]string, bool, error) {
				servers, lerr := app.ListServers()
				if lerr != nil {
					return nil, false, lerr
				}
				names := make([]string, len(servers))
				for i, s := range servers {
					names[i] = s.Name
				}
				if group != "" {
					matched, merr := tagStore.ServersMatching(group, names)
					if merr != nil {
						return nil, false, merr
					}
					return matched, true, nil
				}
				if all {
					return names, true, nil
				}
				return nil, false, nil
			}

			targets, multi, err := resolveTargets()
			if err != nil {
				return err
			}

			if multi {
				command := strings.Join(args, " ")
				out := make([]execJSON, len(targets))
				skipped := make([]bool, len(targets))
				errs := make([]error, len(targets))
				for i, name := range targets {
					j, skip, eerr := execOne(name, command, true, !asJSON)
					out[i], skipped[i], errs[i] = j, skip, eerr
				}
				if asJSON {
					emitted := out[:0]
					for i := range out {
						if !skipped[i] && errs[i] == nil {
							emitted = append(emitted, out[i])
						}
					}
					if encErr := json.NewEncoder(cmd.OutOrStdout()).Encode(emitted); encErr != nil {
						return encErr
					}
				}
				// Surface any per-server block reason without aborting the others.
				for i := range errs {
					if errs[i] != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", targets[i], errs[i])
					}
				}
				return nil
			}

			if len(args) < 2 {
				return fmt.Errorf("usage: fleet exec <server> <command>")
			}
			serverName := args[0]
			command := strings.Join(args[1:], " ")

			j, skip, eerr := execOne(serverName, command, false, !asJSON)
			if eerr != nil {
				return eerr
			}
			if skip {
				if asJSON && j.Server != "" {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(j)
				}
				return nil
			}

			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(j); err != nil {
					return err
				}
				if propagateExit && j.AgentError == "" && !j.TimedOut && j.ExitCode != 0 {
					_ = app.Close()
					os.Exit(j.ExitCode)
				}
				return nil // JSON mode never errors on a remote non-zero exit
			}

			// Human mode: execOne already printed stdout/stderr. Mirror the original
			// error semantics (transport error / timeout / non-zero exit).
			if j.AgentError != "" {
				return fmt.Errorf("%s: %s", classifyAgentErrorStr(j.AgentError), j.AgentError)
			}
			if j.TimedOut {
				return fmt.Errorf("timed out after %s", timeout)
			}
			if j.ExitCode != 0 {
				if propagateExit {
					_ = app.Close()
					os.Exit(j.ExitCode)
				}
				return fmt.Errorf("exit status %d", j.ExitCode)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "run on all servers concurrently")
	cmd.Flags().BoolVar(&asJSON, "json", false, "structured JSON output (stdout/stderr/exit_code/duration/agent_error)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "abort the command after this duration (e.g. 30s)")
	cmd.Flags().IntVar(&retries, "retry", 0, "retry transport failures up to this many times")
	cmd.Flags().DurationVar(&backoff, "backoff", 2*time.Second, "delay between transport retries")
	cmd.Flags().BoolVar(&propagateExit, "propagate-exit", false, "exit with the remote command's exit code")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print 'would run: <cmd>' for the target(s) and exit without running")
	cmd.Flags().StringVar(&onFail, "on-fail", "", "run this command on the same server if the command fails")
	cmd.Flags().StringVar(&group, "group", "", "run on all servers whose tags match EXPR (e.g. role=web)")
	cmd.Flags().BoolVar(&guard, "guard", false, "block the command if it could lock out the controller")
	cmd.Flags().BoolVar(&guardWarn, "guard-warn", false, "downgrade --guard to a warning and run anyway")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "confirm a command flagged confirm-required by cmd-policy")
	cmd.Flags().BoolVar(&requireApprove, "require-approval", false, "stage the command for approval instead of running it")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "return the cached result for KEY instead of re-running")
	return cmd
}

// printExecHuman renders a single exec result in human mode, mirroring the
// original --all output format. printHeader adds the "=== server [status] ==="
// banner (used for multi-server output); single-server output omits it.
func printExecHuman(cmd *cobra.Command, j execJSON, printHeader bool) {
	if printHeader {
		switch {
		case j.AgentError != "":
			fmt.Fprintf(cmd.OutOrStdout(), "=== %s [%s] ===\n%s\n", j.Server, classifyAgentErrorStr(j.AgentError), j.AgentError)
			return
		case j.TimedOut:
			fmt.Fprintf(cmd.OutOrStdout(), "=== %s [timed out] ===\n", j.Server)
		default:
			fmt.Fprintf(cmd.OutOrStdout(), "=== %s [exit %d] ===\n", j.Server, j.ExitCode)
		}
	}
	if j.Stdout != "" {
		fmt.Fprint(cmd.OutOrStdout(), j.Stdout)
	}
	if j.Stderr != "" {
		fmt.Fprint(cmd.ErrOrStderr(), j.Stderr)
	}
}

// classifyAgentErrorStr is classifyAgentError for an already-stringified error.
func classifyAgentErrorStr(s string) string { return classifyAgentError(fmt.Errorf("%s", s)) }

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
				TargetConfigDir:  *configDir,
				FromDir:          fromDir,
				DBBackend:        dbBackend,
				DBDSN:            dbDSN,
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

func newAdjustInitCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "adjust-init",
		Short: "Apply pending config migrations after a fleet upgrade",
		Long: `When fleet adds or removes options from the init wizard, your existing
config may be missing new settings or still carry old ones that are no longer used.

adjust-init walks you through every pending change interactively:
  [✕] removed field  — shows what was removed and why; cleans up the config entry
  [+] added field    — prompts you to choose a value for the new option

Run this after upgrading fleet whenever you see the "run adjust-init" hint.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if *configDir == "" {
				*configDir = core.DefaultConfigDir("")
			}
			if !core.IsInitialized(*configDir) {
				return fmt.Errorf("fleet is not initialized at %s — run 'fleet init' first", *configDir)
			}
			return core.AdjustInit(*configDir, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
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
