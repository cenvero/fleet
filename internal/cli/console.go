// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/webui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The web console is the localhost web UI with every view: the fleet
// overview, the file manager, and (unless turned off) the Control view, which
// runs fleet commands, and the Terminal view, which opens shells. `fleet
// console` starts it; `fleet file ui` starts the same UI on the file manager,
// with Control and Terminal off unless asked for.
//
// On a terminal it shows a small panel (open it in the browser, show the
// link, move it to the background, stop it) instead of a bare URL. A console
// can keep running in the background after the terminal closes; then
// `fleet console open|status|stop` (and running `fleet console` again) reach
// that one. Elsewhere (a script, a pipe) it prints the URL and serves until
// interrupted, as `fleet file ui` always did.

type consoleOptions struct {
	addr       string
	open       string
	control    bool
	terminal   bool
	background bool
	// view is where the browser lands ("files" for `fleet file ui`).
	view string
	// starter names the command, for messages.
	starter string
	// controlSet/terminalSet record whether the flags were given, for
	// background mode, which never keeps Control or Terminal on by default.
	controlSet, terminalSet bool
}

const consoleRefusedUnderToken = "a console in the background runs with the controller's full authority, so it cannot be started or opened with --token / FLEET_TOKEN; run it in the foreground (`fleet console`), or without a token"

func newConsoleCommand(configDir *string) *cobra.Command {
	var o consoleOptions
	cmd := &cobra.Command{
		Use:     "console",
		Aliases: []string{"web"},
		Short:   "Open the web console: fleet overview, files, commands and terminals in your browser",
		Long: "Start the Fleet web console on this machine (loopback only) and open it in\n" +
			"your browser. It has the fleet overview, the file manager, the Control view\n" +
			"(run fleet commands) and the Terminal view (shells on direct-mode servers).\n" +
			"Control and Terminal act with this invocation's authority: turn them off with\n" +
			"--control=false / --terminal=false, or bound them with a scoped --token.\n\n" +
			"On a terminal it shows a panel: open it in the browser, show the access link,\n" +
			"keep it running in the background, or stop it. A background console stops by\n" +
			"itself after an hour without use and after 8 hours at most (30 minutes and 1\n" +
			"hour with Control or Terminal on, which it keeps only if you say so). Reach it\n" +
			"with `fleet console open`, `fleet console status` and `fleet console stop`, or\n" +
			"by running `fleet console` again.\n\n" +
			"The browser is opened with a one-time code, never the session token, so the\n" +
			"token does not show up in the process list. `fleet file ui` starts the same\n" +
			"console on the file manager. `fleet start` is unrelated: it runs the\n" +
			"controller daemon.",
		Example: "  fleet console\n  fleet console --control=false --terminal=false\n  fleet console --background\n  fleet console open --files\n  fleet console stop",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			o.controlSet = cmd.Flags().Changed("control")
			o.terminalSet = cmd.Flags().Changed("terminal")
			o.starter = "fleet console"
			return runConsole(cmd, *configDir, o)
		},
	}
	cmd.Flags().StringVar(&o.addr, "addr", webui.DefaultAddr, "loopback address to serve the console on")
	cmd.Flags().StringVar(&o.open, "open", "auto", "open it in a browser: auto (ask on a terminal), yes, or no")
	cmd.Flags().BoolVar(&o.control, "control", true, "include the Control view (run fleet commands from the browser)")
	cmd.Flags().BoolVar(&o.terminal, "terminal", true, "include the Terminal view (shells on direct-mode servers)")
	cmd.Flags().BoolVar(&o.background, "background", false, "start it in the background and return (Control and Terminal only with --control / --terminal)")
	cmd.AddCommand(newConsoleStatusCommand(configDir), newConsoleOpenCommand(configDir), newConsoleStopCommand(configDir), newConsoleServeCommand(configDir))
	return cmd
}

// newUICommand is `fleet file ui` / `fleet filemanager ui`: the console,
// landing on the file manager, with Control and Terminal off by default.
func newUICommand(configDir *string) *cobra.Command {
	var o consoleOptions
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web console on the file manager (localhost; Control and Terminal opt-in)",
		Long: "Start the Fleet web console on the file manager. It binds loopback only, needs\n" +
			"a per-process token (the browser gets it through a one-time code), restricts\n" +
			"changes to same-origin POSTs and sets a strict CSP — nothing is reachable from\n" +
			"other machines. Browse a server, drag files onto the page to upload them, and\n" +
			"click to download. On a terminal it shows the same panel as `fleet console`;\n" +
			"if a console already runs in the background, this opens it on the files.\n\n" +
			"Two more views are off unless you ask for them, because they let whoever holds\n" +
			"the session token act on the whole fleet with this invocation's authority:\n" +
			"  --control   the Control view runs non-interactive fleet commands (servers,\n" +
			"              services, logs, exec, alerts, updates…) as child processes under\n" +
			"              the same token, policy and audit as the CLI. Commands that reach\n" +
			"              the controller's keys, tokens or secrets stay terminal-only.\n" +
			"  --terminal  the Terminal view opens `fleet ssh` shells on direct-mode servers.\n" +
			"Launch the UI with a scoped --token to bound what those views can do.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			o.controlSet = cmd.Flags().Changed("control")
			o.terminalSet = cmd.Flags().Changed("terminal")
			o.view = "files"
			o.starter = "fleet " + cmd.Parent().Name() + " ui"
			return runConsole(cmd, *configDir, o)
		},
	}
	cmd.Flags().StringVar(&o.addr, "addr", webui.DefaultAddr, "loopback bind address for the web UI")
	cmd.Flags().StringVar(&o.open, "open", "auto", "open the web UI in a browser: auto (prompt when interactive), yes, or no")
	cmd.Flags().BoolVar(&o.control, "control", false, "enable the Control view (run fleet commands from the browser)")
	cmd.Flags().BoolVar(&o.terminal, "terminal", false, "enable the Terminal view (fleet ssh shells in the browser)")
	return cmd
}

func runConsole(cmd *cobra.Command, configDir string, o consoleOptions) error {
	tokenID := resolveTokenID(cmd)
	if st := core.ReadConsole(configDir); st.Running {
		if tokenID != "" {
			return errors.New(consoleRefusedUnderToken)
		}
		return attachConsole(cmd, configDir, st, o)
	}
	if o.background {
		if tokenID != "" {
			return errors.New(consoleRefusedUnderToken)
		}
		control, terminal := o.control && o.controlSet, o.terminal && o.terminalSet
		st, err := core.StartConsole(core.ConsoleStartOptions{ConfigDir: configDir, Args: consoleServeArgs(o.addr, control, terminal)})
		if errors.Is(err, core.ErrConsoleRunning) && st.Running {
			return attachConsole(cmd, configDir, st, o) // another start won the race
		}
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "The console runs in the background at %s (pid %d).\n", st.Address, st.PID)
		describeBackgroundConsole(out, st)
		if decideOpenBrowser(cmd, o.open) {
			if err := openBackgroundConsole(cmd, configDir, st, o.view, false); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "could not open a browser: %v\n", err)
			}
		}
		return nil
	}
	return runForegroundConsole(cmd, configDir, o, tokenID)
}

func consoleServeArgs(addr string, control, terminal bool) []string {
	args := []string{"--addr", addr}
	if control {
		args = append(args, "--control")
	}
	if terminal {
		args = append(args, "--terminal")
	}
	return args
}

// humanLimit renders a recorded duration ("1h0m0s") as "1 hour".
func humanLimit(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return s
	}
	switch {
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return plural(int(d/time.Minute), "minute")
	}
	return d.String()
}

func describeBackgroundConsole(w io.Writer, st core.ConsoleState) {
	views := "overview and files"
	switch {
	case st.Control && st.Terminal:
		views = "overview, files, Control and Terminal"
	case st.Control:
		views = "overview, files and Control"
	case st.Terminal:
		views = "overview, files and Terminal"
	}
	fmt.Fprintf(w, "Views: %s. It stops after %s without use, and by %s at the latest.\n", views, humanLimit(st.IdleLimit), st.StopsBy.Local().Format("15:04 Mon"))
	fmt.Fprintln(w, "Open it with `fleet console open`, stop it with `fleet console stop`.")
}

// newConsoleServer builds the web UI with the views o asks for, bound to the
// token (if any) this invocation runs under.
func newConsoleServer(cmd *cobra.Command, app *core.App, control, terminal bool, tokenID string) (*webui.Server, error) {
	srv, err := webui.New(app)
	if err != nil {
		return nil, err
	}
	// The UI acts with the authority of this invocation. When it was launched
	// under an RBAC token (already vetted by the pre-run gate), the extra
	// read-only views it offers — the Fleet overview's server list, alerts and
	// tags — are held to the same token, exactly as `server list` / `alerts` /
	// `tag` would be.
	if actingOperator != "" {
		srv.SetOperator(actingOperator)
	}
	if authz := uiCommandAuthorizer(cmd, app.ConfigDir); authz != nil {
		srv.SetCommandAuthorizer(authz)
	}
	// The Control view runs non-interactive fleet commands as children of this
	// binary, pinned to this config dir and to the token the UI was launched
	// with, so every run is checked by the same RBAC, cmd-policy and audit
	// code as in a terminal. The Terminal view runs `fleet ssh` the same way,
	// under a pty.
	if control {
		catalog := webConsoleCatalog(cmd.Root())
		srv.SetConsole(catalog, webConsoleLauncher(cmd.Root(), app.ConfigDir, tokenID, catalog))
	}
	if terminal {
		srv.SetTerminal(webTerminalLauncher(app.ConfigDir, tokenID))
	}
	return srv, nil
}

func consoleSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// addrInUse reports a listen failure because something else holds the port.
func addrInUse(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") || strings.Contains(msg, "Only one usage of each socket address")
}

func listenConsole(addr string) (net.Listener, error) {
	l, err := webui.Listen(addr)
	if addrInUse(err) {
		return nil, fmt.Errorf("%w (is another `fleet console` or `fleet file ui` running? pick another port with --addr 127.0.0.1:0)", err)
	}
	return l, err
}

func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func runForegroundConsole(cmd *cobra.Command, configDir string, o consoleOptions, tokenID string) error {
	app, err := openApp(configDir)
	if err != nil {
		return err
	}
	defer app.Close()
	srv, err := newConsoleServer(cmd, app, o.control, o.terminal, tokenID)
	if err != nil {
		return err
	}
	listener, err := listenConsole(o.addr)
	if err != nil {
		return err
	}
	base := webui.BaseURL(listener)
	openTab := func() error {
		code, err := srv.NewLaunchCode()
		if err != nil {
			return err
		}
		return openBrowser(webui.LaunchURL(base, code, o.view))
	}

	if !isInteractive() || cmd.OutOrStdout() != os.Stdout {
		// The shape `fleet file ui` always had: the link, then serve until
		// interrupted.
		out := cmd.OutOrStdout()
		if o.control {
			fmt.Fprintln(cmd.ErrOrStderr(), "Control view on: the session token can run fleet commands with this invocation's authority.")
		}
		if o.terminal {
			fmt.Fprintln(cmd.ErrOrStderr(), "Terminal view on: the session token can open shells on direct-mode servers.")
		}
		fmt.Fprintf(out, "Cenvero Fleet web UI: %s\n", consoleLink(srv, listener, o.view))
		fmt.Fprintln(out, "Bound to loopback only. Keep the token private; press Ctrl-C to stop.")
		// Decide whether to open the browser BEFORE installing the signal
		// handler, so Ctrl-C during the prompt still quits normally.
		shouldOpen := decideOpenBrowser(cmd, o.open)
		ctx, stop := consoleSignalContext()
		defer stop()
		var ready func()
		if shouldOpen {
			ready = func() {
				if err := openTab(); err != nil {
					fmt.Fprintf(out, "could not open a browser automatically (%v) — open the link above manually\n", err)
				}
			}
		}
		return srv.Serve(ctx, listener, ready)
	}

	ctx, stop := consoleSignalContext()
	defer stop()
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(serveCtx, listener, nil) }()
	finish := func() error {
		stopServe()
		return <-served
	}

	out := cmd.OutOrStdout()
	lines := readLines(os.Stdin)
	reset := make(chan struct{}, 1)
	srv.SetOnTokenReset(func() {
		select {
		case reset <- struct{}{}:
		default:
		}
	})
	status := ""
	if strings.EqualFold(o.open, "yes") {
		time.Sleep(300 * time.Millisecond)
		status = openedStatus(openTab())
	}
	for {
		renderConsolePanel(out, configDir, consolePanel{
			address: base, control: o.control, terminal: o.terminal,
			background: tokenID == "", status: status,
		})
		var line string
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nStopping the console…")
			return finish()
		case err := <-served:
			return err
		case <-reset:
			status = "⚠  A link to this console was used twice — if not by you, then by another account on this computer\n" +
				"   that saw it — so the console replaced its access token. Choose 1 to open it again."
			continue
		case l, ok := <-lines:
			if !ok {
				fmt.Fprintln(out, "\nStopping the console…")
				return finish()
			}
			line = l
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "1", "o", "open":
			status = openedStatus(openTab())
		case "2", "l", "link":
			status = "Access link — anyone with it can use this console, so keep it private:\n  " + consoleLink(srv, listener, o.view)
		case "3", "b", "background":
			if tokenID != "" {
				status = "Not available under --token: " + consoleRefusedUnderToken + "."
				continue
			}
			keep := false
			if o.control || o.terminal {
				fmt.Fprint(out, "\nKeep the Control and Terminal views on in the background? Whoever has the link\n"+
					"could then run fleet commands and open shells until it stops (within an hour). [y/N]: ")
				select {
				case l, ok := <-lines:
					keep = ok && isYes(l)
				case <-ctx.Done():
					return finish()
				}
			}
			args := consoleServeArgs(listener.Addr().String(), o.control && keep, o.terminal && keep)
			// Hand the listening socket itself to the background console,
			// with the session token, so the port is never free in between:
			// were it, another account on this machine could bind it and
			// collect the token from the tabs reconnecting to it. Both serve
			// the socket for a moment; then this one stops.
			if sock, ferr := listenerFile(listener); ferr == nil {
				st, err := core.StartConsole(core.ConsoleStartOptions{
					ConfigDir:     configDir,
					Args:          append(args, "--listen-fd", "3"),
					HandoverToken: srv.Token(),
					ExtraFiles:    []*os.File{sock},
				})
				_ = sock.Close()
				if err != nil {
					status = fmt.Sprintf("Could not keep it running in the background (%v); it is still running here.", err)
					continue
				}
				if err := finish(); err != nil {
					return err
				}
				fmt.Fprintf(out, "\nThe console now runs in the background at %s (pid %d); tabs you opened keep working.\n", st.Address, st.PID)
				describeBackgroundConsole(out, st)
				return nil
			}
			// No way to pass the socket (Windows): stop here first, then start
			// one with its own token, so a token that could be collected
			// while the port is free is worthless, and open a fresh tab.
			if err := finish(); err != nil {
				return err
			}
			st, err := core.StartConsole(core.ConsoleStartOptions{ConfigDir: configDir, Args: args})
			if err != nil {
				return fmt.Errorf("the console stopped, but could not keep running in the background: %w", err)
			}
			fmt.Fprintf(out, "\nThe console now runs in the background at %s (pid %d).\n", st.Address, st.PID)
			describeBackgroundConsole(out, st)
			if err := openBackgroundConsole(cmd, configDir, st, o.view, false); err != nil {
				fmt.Fprintf(out, "Tabs you opened before need a new link: %v\n", err)
			} else {
				fmt.Fprintln(out, "Opened it in a new tab; tabs you opened before need this new one.")
			}
			return nil
		case "4", "q", "s", "stop", "quit", "exit":
			fmt.Fprintln(out, "Stopping the console…")
			return finish()
		case "":
			status = ""
		default:
			status = "Choose 1, 2, 3 or 4, then Enter."
		}
	}
}

func consoleLink(srv *webui.Server, l net.Listener, view string) string {
	link := srv.TokenURL(l)
	if view != "" {
		link += "#" + view
	}
	return link
}

func openedStatus(err error) string {
	if err != nil {
		return fmt.Sprintf("Could not open a browser (%v). Choose 2 to see the link to open yourself.", err)
	}
	return "Opened in your browser."
}

func isYes(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}

// readLines delivers stdin a line at a time; the channel closes at EOF.
func readLines(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

type consolePanel struct {
	address            string
	control, terminal  bool
	background         bool // offer "keep it running in the background"
	attached           bool // the console runs in the background already
	stopsBy, idleLimit string
	status             string
}

func renderConsolePanel(w io.Writer, configDir string, p consolePanel) {
	fmt.Fprint(w, "\x1b[H\x1b[2J") // clear the screen
	fmt.Fprintln(w, "Cenvero Fleet console")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Address    %s\n", p.address)
	views := []string{"Overview", "Files"}
	if p.control {
		views = append(views, "Control")
	}
	if p.terminal {
		views = append(views, "Terminal")
	}
	fmt.Fprintf(w, "  Views      %s\n", strings.Join(views, ", "))
	if p.attached {
		fmt.Fprintf(w, "  Running    in the background; stops after %s unused, by %s at the latest\n", p.idleLimit, p.stopsBy)
	}
	if cfg, err := core.LoadConfigShared(core.ConfigPath(configDir)); err == nil {
		fmt.Fprintf(w, "  Daemon     %s\n", core.DaemonStatus(configDir, cfg.Runtime.ControlAddress).Describe())
	}
	if n := len(core.CollectNotices(configDir)); n > 0 {
		fmt.Fprintf(w, "  Attention  %s — run `fleet errors`\n", plural(n, "thing"))
	}
	if p.control || p.terminal {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Control/Terminal are on: whoever has the access link can run fleet commands")
		fmt.Fprintln(w, "  and open shells with your authority. Keep the link private.")
	}
	fmt.Fprintln(w)
	if p.attached {
		fmt.Fprintln(w, "  1  Open in your browser")
		fmt.Fprintln(w, "  2  Stop the background console")
		fmt.Fprintln(w, "  3  Leave it running and exit")
	} else {
		fmt.Fprintln(w, "  1  Open in your browser")
		fmt.Fprintln(w, "  2  Show the access link")
		if p.background {
			fmt.Fprintln(w, "  3  Keep it running in the background and return to the shell")
		} else {
			fmt.Fprintln(w, "  3  (keep running in the background: not available under --token)")
		}
		fmt.Fprintln(w, "  4  Stop the console and exit")
	}
	if p.status != "" {
		fmt.Fprintf(w, "\n%s\n", p.status)
	}
	if p.attached {
		fmt.Fprint(w, "\nChoose [1-3]: ")
	} else {
		fmt.Fprint(w, "\nChoose [1-4]: ")
	}
}

// attachConsole is `fleet console` (or `fleet file ui`) while a console runs
// in the background: it uses that one instead of starting another.
func attachConsole(cmd *cobra.Command, configDir string, st core.ConsoleState, o consoleOptions) error {
	out := cmd.OutOrStdout()
	if st.Starting {
		return errors.New("a console is starting in the background; try again in a moment (`fleet console status`)")
	}
	var notes []string
	if (o.controlSet && o.control && !st.Control) || (o.terminalSet && o.terminal && !st.Terminal) {
		notes = append(notes, "The console in the background runs without some of the views you asked for; stop it (`fleet console stop`) and start it again to change that.")
	}
	if o.view == "files" && (st.Control || st.Terminal) {
		notes = append(notes, "Note: that console has the Control/Terminal views on, which `"+o.starter+"` leaves off by default.")
	}
	if !isInteractive() || cmd.OutOrStdout() != os.Stdout {
		fmt.Fprintf(out, "A console already runs in the background at %s (pid %d).\n", st.Address, st.PID)
		for _, n := range notes {
			fmt.Fprintln(out, n)
		}
		if strings.EqualFold(o.open, "yes") {
			return openBackgroundConsole(cmd, configDir, st, o.view, false)
		}
		fmt.Fprintln(out, "Open it with `fleet console open`, stop it with `fleet console stop`.")
		return nil
	}
	lines := readLines(os.Stdin)
	status := strings.Join(notes, "\n")
	for {
		renderConsolePanel(out, configDir, consolePanel{
			address: st.Address, control: st.Control, terminal: st.Terminal, attached: true,
			idleLimit: humanLimit(st.IdleLimit), stopsBy: st.StopsBy.Local().Format("15:04 Mon"), status: status,
		})
		l, ok := <-lines
		if !ok {
			fmt.Fprintln(out)
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "1", "o", "open":
			status = openedStatus(openBackgroundConsole(cmd, configDir, st, o.view, false))
		case "2", "s", "stop":
			res, err := stopBackgroundConsole(configDir)
			if err != nil {
				return err
			}
			if res.WasRunning {
				fmt.Fprintf(out, "Stopped the background console (pid %d).\n", res.PID)
			}
			return nil
		case "3", "q", "quit", "exit":
			fmt.Fprintln(out, "The console keeps running in the background.")
			return nil
		case "":
			status = ""
		default:
			status = "Choose 1, 2 or 3, then Enter."
		}
	}
}

// openBackgroundConsole asks the background console for a one-time launch
// code and opens (or, with printOnly, prints) the link.
func openBackgroundConsole(cmd *cobra.Command, configDir string, st core.ConsoleState, view string, printOnly bool) error {
	key, err := core.ReadConsoleLaunchKey(configDir)
	if err != nil {
		return err
	}
	code, err := requestLaunchCode(st.Address, key)
	if err != nil {
		return err
	}
	link := webui.LaunchURL(st.Address, code, view)
	if printOnly {
		fmt.Fprintln(cmd.OutOrStdout(), link)
		fmt.Fprintln(cmd.ErrOrStderr(), "This link works once, within 30 seconds.")
		return nil
	}
	if err := openBrowser(link); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "could not open a browser (%v); open this link within 30 seconds (it works once):\n%s\n", err, link)
	}
	return nil
}

// stopBackgroundConsole stops the background console: first by asking it
// (with the launch key) so it ends the runs and terminals it started, then,
// if it has not exited in time, by signalling it (core.StopConsole).
func stopBackgroundConsole(configDir string) (core.ConsoleStopResult, error) {
	st := core.ReadConsole(configDir)
	if st.Running && !st.Starting {
		if key, err := core.ReadConsoleLaunchKey(configDir); err == nil {
			if _, err := consolePost(st.Address, key, "/api/shutdown"); err == nil {
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if !core.ReadConsole(configDir).Running {
						return core.ConsoleStopResult{WasRunning: true, PID: st.PID}, nil
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
	}
	return core.StopConsole(configDir, 0)
}

// requestLaunchCode gets a launch code from the console at address. The
// address comes from the console's own state file and must be loopback; the
// request never goes through a proxy or follows a redirect, since it
// carries the launch key.
func requestLaunchCode(address, key string) (string, error) {
	body, err := consolePost(address, key, "/api/launch-code")
	if err != nil {
		return "", err
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Code == "" {
		return "", errors.New("the background console sent an unexpected answer")
	}
	return out.Code, nil
}

// consolePost sends an authenticated request to a background console's
// launch-key endpoint and returns the answer's body.
func consolePost(address, key, path string) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, fmt.Errorf("the console's recorded address %q is not usable", address)
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		return nil, fmt.Errorf("the console's recorded address %q is not usable", address)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("the console's recorded address %q is not loopback; refusing to send it the launch key", address)
	}
	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+u.Host+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Fleet-Launch-Key", key)
	req.Header.Set("Origin", "http://"+u.Host)
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the background console: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the background console refused (%s: %s)", res.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func newConsoleStatusCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether a console runs in the background, where, and until when",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeJSON(cmd, core.ReadConsole(*configDir))
		},
	}
}

func newConsoleOpenCommand(configDir *string) *cobra.Command {
	var files, printOnly bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the console that runs in the background in your browser",
		Long: "Open the background console in a new browser tab, with a one-time code (the\n" +
			"session token never appears on a command line). --print shows that one-time\n" +
			"link instead; it works once, within 30 seconds.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if resolveTokenID(cmd) != "" {
				return errors.New(consoleRefusedUnderToken)
			}
			st := core.ReadConsole(*configDir)
			if !st.Running {
				return errors.New("no console runs in the background; start one with `fleet console`")
			}
			if st.Starting {
				return errors.New("the console is still starting; try again in a moment")
			}
			view := ""
			if files {
				view = "files"
			}
			return openBackgroundConsole(cmd, *configDir, st, view, printOnly)
		},
	}
	cmd.Flags().BoolVar(&files, "files", false, "open it on the file manager")
	cmd.Flags().BoolVar(&printOnly, "print", false, "print a one-time link instead of opening a browser")
	return cmd
}

func newConsoleStopCommand(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the console that runs in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			res, err := stopBackgroundConsole(*configDir)
			if err != nil {
				return err
			}
			return writeJSON(cmd, res)
		},
	}
}

// newConsoleServeCommand is the background console process itself, started
// by core.StartConsole. It is hidden: run `fleet console --background`.
func newConsoleServeCommand(configDir *string) *cobra.Command {
	var addr string
	var control, terminal, tokenStdin bool
	var listenFD int
	cmd := &cobra.Command{
		Use:    "serve",
		Short:  "Serve the console in the background (started by `fleet console --background`)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if resolveTokenID(cmd) != "" {
				return errors.New(consoleRefusedUnderToken)
			}
			inst, err := core.ClaimConsole(*configDir)
			if err != nil {
				return err
			}
			defer inst.Release()
			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()
			srv, err := newConsoleServer(cmd, app, control, terminal, "")
			if err != nil {
				return err
			}
			if tokenStdin {
				token, err := readHandoverToken(cmd.InOrStdin())
				if err != nil {
					return err
				}
				if err := srv.SetToken(token); err != nil {
					return err
				}
			}
			key, err := core.NewConsoleLaunchKey()
			if err != nil {
				return err
			}
			srv.SetLaunchKey(key)
			srv.SetOnTokenReset(func() {
				fmt.Fprintln(cmd.ErrOrStderr(), "a launch link was used twice; the session token was replaced (another account on this computer may have seen the link)")
			})
			var listener net.Listener
			if listenFD > 0 {
				listener, err = inheritedListener(listenFD)
			} else {
				listener, err = listenWithRetry(addr, 5*time.Second)
			}
			if err != nil {
				return err
			}
			idle, lifetime := core.ConsoleLimits(control, terminal)
			started := time.Now()
			state := core.ConsoleState{
				Address: webui.BaseURL(listener), StartedAt: started.UTC(), StopsBy: started.Add(lifetime).UTC(),
				IdleLimit: idle.String(), Control: control, Terminal: terminal,
			}
			if err := inst.Publish(state, key); err != nil {
				_ = listener.Close()
				return err
			}
			ctx, stop := consoleSignalContext()
			defer stop()
			srv.SetShutdown(func() {
				fmt.Fprintln(cmd.ErrOrStderr(), "console asked to stop (fleet console stop)")
				stop()
			})
			go watchConsoleLimits(ctx, stop, srv, idle, state.StopsBy, cmd.ErrOrStderr())
			fmt.Fprintf(cmd.ErrOrStderr(), "console serving at %s (pid %d); stops after %s unused or at %s\n", state.Address, os.Getpid(), idle, state.StopsBy.Format(time.RFC3339))
			err = srv.Serve(ctx, listener, nil)
			fmt.Fprintln(cmd.ErrOrStderr(), "console stopped")
			return err
		},
	}
	cmd.Flags().StringVar(&addr, "addr", webui.DefaultAddr, "loopback address to serve on")
	cmd.Flags().BoolVar(&control, "control", false, "include the Control view")
	cmd.Flags().BoolVar(&terminal, "terminal", false, "include the Terminal view")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the session token to keep from standard input")
	cmd.Flags().IntVar(&listenFD, "listen-fd", 0, "serve on the loopback socket inherited as this file descriptor")
	return cmd
}

// consoleWatchInterval is how often a background console checks its limits.
var consoleWatchInterval = 15 * time.Second

// watchConsoleLimits stops a background console once it has gone unused for
// idle, or at stopsBy (wall-clock time, so a machine that slept past it
// stops on waking).
func watchConsoleLimits(ctx context.Context, stop func(), srv *webui.Server, idle time.Duration, stopsBy time.Time, log io.Writer) {
	tick := time.NewTicker(consoleWatchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			switch {
			case time.Now().After(stopsBy):
				fmt.Fprintln(log, "console reached its time limit; stopping")
				stop()
				return
			case srv.IdleFor() > idle:
				fmt.Fprintf(log, "console unused for %s; stopping\n", idle)
				stop()
				return
			}
		}
	}
}

func readHandoverToken(r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(r, 512)).ReadString('\n')
		ch <- result{strings.TrimSpace(line), err}
	}()
	select {
	case res := <-ch:
		if res.line == "" {
			return "", fmt.Errorf("no session token on standard input: %v", res.err)
		}
		return res.line, nil
	case <-time.After(5 * time.Second):
		return "", errors.New("timed out reading the session token from standard input")
	}
}

// listenerFile returns a duplicate of l's socket to pass to a child process.
// It fails where the OS has no such handover (Windows).
func listenerFile(l net.Listener) (*os.File, error) {
	tl, ok := l.(*net.TCPListener)
	if !ok {
		return nil, errors.New("not a TCP listener")
	}
	return tl.File()
}

// inheritedListener turns the socket a parent console handed over on fd into
// a listener, and checks it is a loopback one.
func inheritedListener(fd int) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "console-listener") // #nosec G115 -- a small fd number from our own parent
	if f == nil {
		return nil, fmt.Errorf("no file descriptor %d", fd)
	}
	defer f.Close()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("use the handed-over socket: %w", err)
	}
	if addr, ok := l.Addr().(*net.TCPAddr); !ok || !addr.IP.IsLoopback() {
		_ = l.Close()
		return nil, fmt.Errorf("the handed-over socket %s is not a loopback TCP socket", l.Addr())
	}
	return l, nil
}

// listenWithRetry binds addr, waiting up to wait for it to come free: when a
// console moves to the background, the foreground one has just let go of it.
func listenWithRetry(addr string, wait time.Duration) (net.Listener, error) {
	deadline := time.Now().Add(wait)
	for {
		l, err := webui.Listen(addr)
		if err == nil || !addrInUse(err) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
