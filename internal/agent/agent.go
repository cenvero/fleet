// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
	"github.com/spf13/cobra"
)

func DetectCapabilities() []string {
	caps := []string{
		"auth.keys.manage",
		"logs.read",
		"logs.stream",
		"metrics.collect",
		"updates.apply",
		"inventory.report",
	}
	switch runtime.GOOS {
	case "linux":
		caps = append(caps, "service.manage", "firewall.manage", "port.manage")
	}
	return caps
}

func Hello(mode transport.Mode) proto.HelloPayload {
	hostname, _ := os.Hostname()
	return proto.HelloPayload{
		NodeName:     hostname,
		AgentVersion: version.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Transport:    mode.String(),
		Capabilities: DetectCapabilities(),
	}
}

func NewRootCommand() *cobra.Command {
	var mode string
	var listenAddress string
	var hostKeyPath string
	var authorizedKeysPath string
	var controllerAddress string
	var serverName string
	var knownHostsPath string
	var acceptNewHostKey bool
	var retryMin time.Duration
	var retryMax time.Duration
	var offlineMetricsInterval time.Duration
	var metricsQueuePath string

	root := &cobra.Command{
		Use:   "fleet-agent",
		Short: "Cenvero Fleet agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runAgent(mode)
		},
	}

	root.Flags().StringVar(&mode, "mode", "direct", "transport mode to advertise")
	root.Flags().StringVar(&listenAddress, "listen", "127.0.0.1:2222", "agent listen address for direct mode")
	root.Flags().StringVar(&hostKeyPath, "host-key", DefaultHostKeyPath(), "SSH host key path")
	root.Flags().StringVar(&authorizedKeysPath, "authorized-keys", "", "authorized_keys file that may connect to the agent")
	root.Flags().StringVar(&controllerAddress, "controller", "127.0.0.1:9443", "controller address for reverse mode")
	root.Flags().StringVar(&serverName, "server-name", "", "registered Cenvero Fleet server name for reverse mode")
	root.Flags().StringVar(&knownHostsPath, "known-hosts", DefaultControllerKnownHostsPath(), "known_hosts file used to pin the controller host key in reverse mode")
	root.Flags().BoolVar(&acceptNewHostKey, "accept-new-host-key", false, "accept a replacement controller host key after manual verification")
	root.Flags().DurationVar(&retryMin, "retry-min", time.Second, "minimum reconnect backoff for reverse mode")
	root.Flags().DurationVar(&retryMax, "retry-max", 30*time.Second, "maximum reconnect backoff for reverse mode")
	root.Flags().DurationVar(&offlineMetricsInterval, "offline-metrics-interval", time.Minute, "how often to queue local metrics while the controller is unreachable")
	root.Flags().StringVar(&metricsQueuePath, "metrics-queue", DefaultMetricsQueuePath(), "path used to persist queued reverse-mode metrics while disconnected")
	root.AddCommand(&cobra.Command{
		Use:   "capabilities",
		Short: "Print the detected agent capabilities",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			for _, capability := range DetectCapabilities() {
				fmt.Fprintln(cmd.OutOrStdout(), capability)
			}
			return nil
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "hello",
		Short: "Print the initial hello payload as JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			parsedMode, err := transport.ParseMode(mode)
			if err != nil {
				return err
			}
			payload, err := json.MarshalIndent(Hello(parsedMode), "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(payload))
			return nil
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Run the direct-mode SSH transport listener",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			parsedMode, err := transport.ParseMode(mode)
			if err != nil {
				return err
			}
			if parsedMode != transport.ModeDirect {
				return fmt.Errorf("agent serve currently supports direct mode only")
			}
			listener, err := net.Listen("tcp", listenAddress)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cenvero Fleet agent listening on %s\n", listener.Addr())
			server := Server{
				Mode:               parsedMode,
				HostKeyPath:        hostKeyPath,
				AuthorizedKeysPath: authorizedKeysPath,
			}
			return server.Serve(context.Background(), listener)
		},
	})
	root.AddCommand(&cobra.Command{
		Use:   "reverse",
		Short: "Connect outward to a reverse-mode controller and serve RPCs over the tunnel",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			parsedMode, err := transport.ParseMode(mode)
			if err != nil {
				return err
			}
			if parsedMode != transport.ModeReverse {
				return fmt.Errorf("agent reverse currently requires --mode reverse")
			}
			server := Server{
				Mode:                     parsedMode,
				HostKeyPath:              hostKeyPath,
				ControllerAddress:        controllerAddress,
				ControllerKnownHostsPath: knownHostsPath,
			}
			return RunReverse(context.Background(), ReverseOptions{
				ControllerAddress:      controllerAddress,
				ServerName:             serverName,
				KnownHostsPath:         knownHostsPath,
				AcceptNewHostKey:       acceptNewHostKey,
				MinRetryDelay:          retryMin,
				MaxRetryDelay:          retryMax,
				OfflineMetricsInterval: offlineMetricsInterval,
				MetricsQueuePath:       metricsQueuePath,
			}, server)
		},
	})
	return root
}

func runAgent(mode string) error {
	parsedMode, err := transport.ParseMode(mode)
	if err != nil {
		return err
	}
	hello := Hello(parsedMode)
	fmt.Printf("%s agent %s ready on %s/%s\n", version.ProductName, version.Version, hello.OS, hello.Arch)
	fmt.Printf("capabilities: %s\n", strings.Join(hello.Capabilities, ", "))
	fmt.Println("transport server scaffolding is present; SSH session handling lands in the next transport iteration.")
	return nil
}
