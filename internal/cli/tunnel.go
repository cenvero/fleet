// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	"github.com/cenvero/fleet/internal/core"
	"github.com/spf13/cobra"
)

// tunnelSpec is a parsed `local:host:port` (or `local:port`) forwarding rule.
type tunnelSpec struct {
	localPort  int
	targetHost string
	targetPort int
}

// parseTunnelSpec parses the forwarding argument into a tunnelSpec.
//
// Two forms are accepted:
//
//	<localPort>:<targetHost>:<targetPort>   forward to targetHost reachable BY
//	                                         the server (e.g. 15432:10.0.0.2:5432)
//	<localPort>:<targetPort>                shorthand: targetHost = localhost on
//	                                         the server (e.g. 15432:5432)
//
// localPort binds on the controller's loopback only; targetHost:targetPort is
// dialed FROM the server.
func parseTunnelSpec(spec string) (tunnelSpec, error) {
	parts := strings.Split(spec, ":")
	var out tunnelSpec
	switch len(parts) {
	case 2:
		out.targetHost = "localhost"
		lp, err := parsePort(parts[0], "local port")
		if err != nil {
			return tunnelSpec{}, err
		}
		tp, err := parsePort(parts[1], "target port")
		if err != nil {
			return tunnelSpec{}, err
		}
		out.localPort, out.targetPort = lp, tp
	case 3:
		lp, err := parsePort(parts[0], "local port")
		if err != nil {
			return tunnelSpec{}, err
		}
		host := strings.TrimSpace(parts[1])
		if host == "" {
			return tunnelSpec{}, fmt.Errorf("target host is empty in %q", spec)
		}
		tp, err := parsePort(parts[2], "target port")
		if err != nil {
			return tunnelSpec{}, err
		}
		out.localPort, out.targetHost, out.targetPort = lp, host, tp
	default:
		return tunnelSpec{}, fmt.Errorf("invalid forward spec %q: expected <localPort>:<targetHost>:<targetPort> or <localPort>:<targetPort>", spec)
	}
	return out, nil
}

// isLoopbackTunnelTarget reports whether host names the server's own loopback —
// the only target a SERVER-SCOPED token may forward to (a `<localPort>:<port>`
// shorthand resolves to "localhost", and an explicit 127.0.0.1/::1 is equivalent).
// A bare hostname like "localhost" is matched by name; an IP is matched by
// net.IP.IsLoopback so 127.0.0.0/8 and ::1 are all covered. Anything else (a
// routable IP or another hostname) is out of scope.
func isLoopbackTunnelTarget(host string) bool {
	h := strings.TrimSpace(host)
	h = strings.Trim(h, "[]") // tolerate a bracketed IPv6 literal
	if strings.EqualFold(h, "localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func parsePort(s, label string) (int, error) {
	s = strings.TrimSpace(s)
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: not a number", label, s)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid %s %d: must be between 1 and 65535", label, p)
	}
	return p, nil
}

func newTunnelCommand(configDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel <server> <localPort>:<targetHost>:<targetPort>",
		Short: "Forward a local port to a host:port reachable by the server",
		Long: "Open a TCP tunnel that forwards a local loopback port to a host:port that is\n" +
			"reachable BY THE SERVER, using SSH direct-tcpip over the agent transport. The\n" +
			"server (not your controller) makes the outbound connection, so you can reach\n" +
			"hosts that only the server can route to — a private database, an internal\n" +
			"admin port, another host on the server's VPC.\n\n" +
			"The local port binds 127.0.0.1 only. Multiple concurrent connections are\n" +
			"supported. Press Ctrl-C to stop; in-flight connections are closed.\n\n" +
			"Spec forms:\n" +
			"  <localPort>:<targetHost>:<targetPort>   forward to targetHost from the server\n" +
			"  <localPort>:<targetPort>                shorthand for targetHost = localhost\n\n" +
			"Examples:\n" +
			"  fleet tunnel web-01 15432:10.0.0.2:5432   # reach a private DB via web-01\n" +
			"  fleet tunnel web-01 8080:80               # reach web-01's own localhost:80",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			serverName := args[0]
			spec, err := parseTunnelSpec(args[1])
			if err != nil {
				return err
			}

			// FL-030 tunnel-target scoping: enforceToken (PersistentPreRunE) already
			// scope-checks the SERVER (args[0]), but the dialed targetHost:targetPort is
			// dialed FROM that server and was never checked — a SERVER-SCOPED token could
			// forward to ANY host the jump server routes to (an internal DB,
			// 169.254.169.254, another VPC host). Confine a server-scoped token to a
			// LOOPBACK target on the server itself (the legit localhost-forward use). An
			// unscoped/command-only token is unaffected.
			tok, terr := currentVerifiedToken(cmd, *configDir)
			if terr != nil {
				return terr
			}
			if tok != nil && (len(tok.Servers) > 0 || len(tok.Groups) > 0) && !isLoopbackTunnelTarget(spec.targetHost) {
				return fmt.Errorf("denied: a server-scoped token may only tunnel to a loopback target on the server (got %q); forwarding to arbitrary hosts is out of scope in RBAC v1", spec.targetHost)
			}

			app, err := openApp(*configDir)
			if err != nil {
				return err
			}
			defer app.Close()

			dialer, err := app.OpenTunnelDialer(serverName)
			if err != nil {
				return err
			}
			defer dialer.Close()

			localAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.localPort))
			listener, err := net.Listen("tcp", localAddr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", localAddr, err)
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			out := cmd.OutOrStdout()
			target := net.JoinHostPort(spec.targetHost, strconv.Itoa(spec.targetPort))
			fmt.Fprintf(out, "forwarding %s -> %s -> %s (Ctrl-C to stop)\n", localAddr, serverName, target)

			return runTunnel(ctx, listener, dialer, target, out)
		},
	}
	return cmd
}

// runTunnel accepts local connections on listener and proxies each to target,
// dialed FROM the server through dialer. It blocks until ctx is cancelled
// (Ctrl-C), at which point the listener and all in-flight connections are
// closed and it returns nil.
func runTunnel(ctx context.Context, listener net.Listener, dialer *core.TunnelDialer, target string, out io.Writer) error {
	// conns tracks in-flight local connections so Ctrl-C can close them, which
	// unblocks their io.Copy goroutines and lets the WaitGroup drain.
	var (
		connMu sync.Mutex
		conns  = map[net.Conn]struct{}{}
		wg     sync.WaitGroup
	)

	// Closing the listener on ctx cancellation makes Accept return an error,
	// which breaks the accept loop below.
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		connMu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		connMu.Unlock()
	}()

	for {
		local, err := listener.Accept()
		if err != nil {
			// Distinguish a clean Ctrl-C shutdown from a real listener failure.
			if ctx.Err() != nil {
				break
			}
			// Transient accept errors shouldn't kill the tunnel; persistent ones
			// (listener closed) are caught by the ctx check above.
			if errors.Is(err, net.ErrClosed) {
				break
			}
			fmt.Fprintf(out, "accept error: %v\n", err)
			continue
		}

		connMu.Lock()
		// Shutdown race: the ctx-cancel goroutine closes every tracked conn
		// while holding connMu. If we registered a conn after that sweep ran,
		// nothing would ever close it (the sweep is one-shot), orphaning the
		// conn and its handler goroutine. Re-check ctx under the lock: if
		// shutdown began, drop this conn instead of tracking/handling it.
		if ctx.Err() != nil {
			connMu.Unlock()
			_ = local.Close()
			break
		}
		conns[local] = struct{}{}
		connMu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				connMu.Lock()
				delete(conns, local)
				connMu.Unlock()
				_ = local.Close()
			}()
			handleTunnelConn(local, dialer, target, out)
		}()
	}

	wg.Wait()
	fmt.Fprintln(out, "tunnel stopped")
	return nil
}

// handleTunnelConn dials target from the server and pumps bytes in both
// directions until either side closes. local is closed by the caller.
func handleTunnelConn(local net.Conn, dialer *core.TunnelDialer, target string, out io.Writer) {
	remote, err := dialer.Dial("tcp", target)
	if err != nil {
		fmt.Fprintf(out, "dial %s from server failed: %v\n", target, err)
		return
	}
	defer remote.Close()

	// Copy in both directions. When either side hits EOF/error, close both ends
	// so the other copy goroutine unblocks too, then wait for both to finish.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = local.Close()
			_ = remote.Close()
		})
	}

	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(remote, local)
		closeBoth()
	}()
	go func() {
		defer copyWG.Done()
		_, _ = io.Copy(local, remote)
		closeBoth()
	}()
	copyWG.Wait()
}
