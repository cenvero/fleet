// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

// globalUpdateMu ensures only one update.apply runs at a time across all
// concurrent RPC connections. TryLock returns false immediately if another
// update is already in progress, so the caller gets an error rather than
// waiting (which would stall the service restart).
var globalUpdateMu sync.Mutex

type Server struct {
	Mode                     transport.Mode
	HostKeyPath              string
	AuthorizedKeysPath       string
	ControllerAddress        string
	ControllerKnownHostsPath string
	AuthorizedKeysMgr        AuthorizedKeysManager
	ControllerKnownHostsMgr  ControllerKnownHostsManager
	ServiceManager           ServiceManager
	FirewallManager          FirewallManager
	LogReader                LogReader
	MetricsCollector         MetricsCollector
	MetricsQueue             MetricsQueue
	Updater                  Updater
	FileManager              FileManager
}

func (s Server) Serve(ctx context.Context, listener net.Listener) error {
	signer, err := fleetcrypto.EnsureEd25519Signer(s.HostKeyPath)
	if err != nil {
		return err
	}

	if s.AuthorizedKeysPath == "" {
		return fmt.Errorf("--authorized-keys path is required for direct mode")
	}

	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		// Identifies this port as a Cenvero Fleet agent to anyone who scans it.
		// Standard SSH clients cannot open sessions anyway — they don't know the
		// fleet-rpc / fleet-shell channel types.
		ServerVersion: "SSH-2.0-cenvero-fleet-agent",
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			authorizedKeys, err := loadAuthorizedKeys(s.AuthorizedKeysPath)
			if err != nil {
				return nil, fmt.Errorf("authorized keys unavailable: %w", err)
			}
			if _, ok := authorizedKeys[string(key.Marshal())]; ok {
				return &ssh.Permissions{
					Extensions: map[string]string{
						"user":   conn.User(),
						"key_fp": ssh.FingerprintSHA256(key),
					},
				}, nil
			}
			return nil, fmt.Errorf("unauthorized public key for %s", conn.User())
		},
	}
	config.AddHostKey(signer)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		rawConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept transport connection: %w", err)
		}
		go func() {
			_ = s.serveConn(rawConn, config)
		}()
	}
}

func (s Server) ServeConn(rawConn net.Conn) error {
	signer, err := fleetcrypto.EnsureEd25519Signer(s.HostKeyPath)
	if err != nil {
		return err
	}
	if s.AuthorizedKeysPath == "" {
		return fmt.Errorf("--authorized-keys path is required for direct mode")
	}
	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		ServerVersion: "SSH-2.0-cenvero-fleet-agent",
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			authorizedKeys, err := loadAuthorizedKeys(s.AuthorizedKeysPath)
			if err != nil {
				return nil, fmt.Errorf("authorized keys unavailable: %w", err)
			}
			if _, ok := authorizedKeys[string(key.Marshal())]; ok {
				return &ssh.Permissions{
					Extensions: map[string]string{
						"user":   conn.User(),
						"key_fp": ssh.FingerprintSHA256(key),
					},
				}, nil
			}
			return nil, fmt.Errorf("unauthorized public key for %s", conn.User())
		},
	}
	config.AddHostKey(signer)
	return s.serveConn(rawConn, config)
}

func (s Server) serveConn(rawConn net.Conn, config *ssh.ServerConfig) error {
	defer rawConn.Close()

	conn, chans, reqs, err := ssh.NewServerConn(rawConn, config)
	if err != nil {
		return err
	}
	defer conn.Close()
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		switch newChannel.ChannelType() {
		case transport.RPCChannelType:
			channel, requests, err := newChannel.Accept()
			if err != nil {
				continue
			}
			go ssh.DiscardRequests(requests)
			go s.serveRPC(channel)
		case transport.ShellChannelType:
			channel, requests, err := newChannel.Accept()
			if err != nil {
				continue
			}
			sessionID := conn.Permissions.Extensions["key_fp"]
			go serveShell(channel, requests, sessionID)
		case directTCPIPChannelType:
			go serveDirectTCPIP(newChannel)
		default:
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
	return nil
}

// directTCPIPChannelType is the standard SSH channel type a client opens to
// request the server make an outbound TCP connection on its behalf — the
// primitive behind `fleet tunnel`. The Go SSH client's Client.Dial("tcp", addr)
// opens exactly this channel type (RFC 4254 §7.2).
const directTCPIPChannelType = "direct-tcpip"

// tunnelDialTimeout bounds how long the agent waits when dialing a tunnel
// target. Without it a controller could open many channels to unroutable or
// black-holed host:ports and pin a goroutine + fd per channel until the OS
// connect timeout (minutes), a cheap fd/goroutine DoS.
const tunnelDialTimeout = 10 * time.Second

// maxConcurrentTunnels caps how many direct-tcpip channels may be live at once
// across the whole agent. Each live tunnel holds an fd to the target plus two
// copy goroutines; bounding the count prevents an authenticated controller from
// exhausting file descriptors or goroutines by opening unbounded channels.
// Requests beyond the cap are rejected (the controller can retry).
const maxConcurrentTunnels = 64

// tunnelSlots is a counting semaphore: a token is acquired before a tunnel is
// served and released when it tears down. Buffered to maxConcurrentTunnels.
var tunnelSlots = make(chan struct{}, maxConcurrentTunnels)

// directTCPIPExtraData is the channel-open extra data for a "direct-tcpip"
// request, per RFC 4254 §7.2: the host/port the server should connect to,
// followed by the originator's host/port (informational only).
//
// The field order and wire types match what golang.org/x/crypto/ssh's
// Client.dial marshals, so ssh.Unmarshal decodes it directly.
type directTCPIPExtraData struct {
	HostToConnect  string
	PortToConnect  uint32
	OriginatorIP   string
	OriginatorPort uint32
}

// serveDirectTCPIP fulfills an SSH "direct-tcpip" channel: it parses the
// requested destination, dials it FROM the agent, accepts the channel, and
// pipes bytes both ways until either side closes.
//
// SECURITY: this lets an authenticated controller reach any host:port the agent
// can route to — that is the entire point of `fleet tunnel` (e.g. reaching a
// private DB that only the server can see). It is deliberately NOT restricted by
// the agent's --file-root: --file-root scopes filesystem access for file.*
// transfers and has no meaning for TCP destinations, and a network tunnel is a
// distinct capability from file access. The only gate is the existing SSH
// public-key authentication (PublicKeyCallback) — by the time a channel is
// opened the controller's key is already authorized, which is the same trust
// boundary that grants shell.exec and firewall control. No new authorization is
// added here; anyone able to open a fleet-shell/fleet-rpc channel can already
// run arbitrary commands on the agent, so tunneling grants no additional power.
func serveDirectTCPIP(newChannel ssh.NewChannel) {
	var req directTCPIPExtraData
	if err := ssh.Unmarshal(newChannel.ExtraData(), &req); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}

	// Bound concurrent tunnels: grab a semaphore token or reject. Done before the
	// dial so we cap in-flight dials too, not just established tunnels.
	select {
	case tunnelSlots <- struct{}{}:
		defer func() { <-tunnelSlots }()
	default:
		_ = newChannel.Reject(ssh.ResourceShortage, "too many concurrent tunnels")
		return
	}

	dest := net.JoinHostPort(req.HostToConnect, strconv.Itoa(int(req.PortToConnect)))
	// DialTimeout, not Dial: a black-holed target must not pin a goroutine + fd
	// for the OS-default connect timeout (minutes).
	remote, err := net.DialTimeout("tcp", dest, tunnelDialTimeout)
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, fmt.Sprintf("dial %s: %v", dest, err))
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		_ = remote.Close()
		return
	}
	go ssh.DiscardRequests(requests)

	// Pipe both directions. When either side hits EOF/error, close both ends so
	// the opposite copy unblocks, then wait for both to finish.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = channel.Close()
			_ = remote.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(remote, channel)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(channel, remote)
		closeBoth()
	}()
	wg.Wait()
}

func (s Server) serveRPC(channel ssh.Channel) {
	defer channel.Close()

	for {
		request, err := proto.Decode(channel)
		if err != nil {
			if err != io.EOF {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					Action:          "error",
					Error: &proto.Error{
						Code:    "decode_error",
						Message: err.Error(),
					},
				})
			}
			return
		}

		switch strings.ToLower(request.Action) {
		case "hello", "inventory":
			hello := Hello(s.Mode)
			hello.ControllerID = controllerIDFromPayload(request.Payload)
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Capabilities:    hello.Capabilities,
				Payload:         hello,
			})
		case "service.list":
			services, err := s.serviceManager().List(context.Background())
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         services,
			})
		case "service.control":
			action, err := proto.DecodePayload[proto.ServiceActionPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			info, err := s.serviceManager().Control(context.Background(), action.Service, action.Action)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         info,
			})
		case "auth.update_keys":
			payload, err := proto.DecodePayload[proto.AuthorizedKeysPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			result, err := s.authorizedKeysManager().Update(context.Background(), payload)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         result,
			})
		case "auth.update_controller_host_keys":
			payload, err := proto.DecodePayload[proto.ControllerKnownHostsPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			result, err := s.controllerKnownHostsManager().Update(context.Background(), payload)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         result,
			})
		case "metrics.collect":
			snapshot, err := s.metricsCollector().Collect(context.Background())
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         snapshot,
			})
		case "metrics.flush_queue":
			snapshots, err := s.metricsQueue().Flush()
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload: proto.MetricsReplayResult{
					Snapshots: snapshots,
				},
			})
		case "log.read":
			payload, err := proto.DecodePayload[proto.LogReadPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			result, err := s.logReader().Read(context.Background(), payload)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         result,
			})
		case "firewall.status":
			info, err := s.firewallManager().Status(context.Background())
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         info,
			})
		case "firewall.enable", "firewall.disable":
			info, err := s.firewallManager().Enable(context.Background(), request.Action == "firewall.enable")
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         info,
			})
		case "firewall.add_rule":
			rule, err := proto.DecodePayload[proto.FirewallRulePayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			info, err := s.firewallManager().AddRule(context.Background(), rule.Rule)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         info,
			})
		case "port.list":
			ports, err := s.firewallManager().ListOpenPorts(context.Background())
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         ports,
			})
		case "port.set":
			payload, err := proto.DecodePayload[proto.PortActionPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			info, err := s.firewallManager().SetPort(context.Background(), payload.Port, payload.Open)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         info,
			})
		case "update.apply":
			if !globalUpdateMu.TryLock() {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "update_in_progress",
						Message: "an update or sync is already in progress on this agent — try again in a moment",
					},
				})
				continue
			}
			payload, err := proto.DecodePayload[proto.UpdateApplyPayload](request.Payload)
			if err != nil {
				globalUpdateMu.Unlock()
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			op, err := s.updater().Apply(context.Background(), payload)
			globalUpdateMu.Unlock()
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			encErr := proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         op.Result,
			})
			// Always run Finalize regardless of whether the encode succeeded.
			// Finalize triggers the service restart; skipping it on a dropped
			// connection would leave the agent in a broken mid-update state.
			if op.Finalize != nil {
				_ = op.Finalize()
			}
			if encErr != nil {
				continue
			}
		case "shell.exec":
			payload, err := proto.DecodePayload[proto.ExecPayload](request.Payload)
			if err != nil {
				_ = proto.Encode(channel, proto.Envelope{
					Type:            proto.EnvelopeTypeResponse,
					ProtocolVersion: proto.CurrentProtocolVersion,
					RequestID:       request.RequestID,
					Action:          request.Action,
					Error: &proto.Error{
						Code:    "bad_payload",
						Message: err.Error(),
					},
				})
				continue
			}
			result, err := runShellExec(context.Background(), payload)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         result,
			})
		case proto.ActionFileList:
			handleFileRPC(channel, request, s.fileManager().List)
		case proto.ActionFileStat:
			handleFileRPC(channel, request, s.fileManager().Stat)
		case proto.ActionFileRead:
			handleFileRPC(channel, request, s.fileManager().Read)
		case proto.ActionFileOpenWrite:
			handleFileRPC(channel, request, s.fileManager().OpenWrite)
		case proto.ActionFileWrite:
			handleFileRPC(channel, request, s.fileManager().Write)
		case proto.ActionFileFinalize:
			handleFileRPC(channel, request, s.fileManager().Finalize)
		case proto.ActionFileProbe:
			handleFileRPC(channel, request, s.fileManager().Probe)
		case proto.ActionFileMkdir:
			handleFileRPC(channel, request, s.fileManager().Mkdir)
		case proto.ActionFileDelete:
			handleFileRPC(channel, request, s.fileManager().Delete)
		case proto.ActionFileRename:
			handleFileRPC(channel, request, s.fileManager().Rename)
		default:
			_ = proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Error: &proto.Error{
					Code:    "unsupported_action",
					Message: fmt.Sprintf("action %q is not supported by the agent yet", request.Action),
				},
			})
		}
	}
}

func (s Server) serviceManager() ServiceManager {
	if s.ServiceManager != nil {
		return s.ServiceManager
	}
	return defaultServiceManager()
}

func (s Server) logReader() LogReader {
	if s.LogReader != nil {
		return s.LogReader
	}
	return defaultLogReader()
}

func (s Server) firewallManager() FirewallManager {
	if s.FirewallManager != nil {
		return s.FirewallManager
	}
	return defaultFirewallManager()
}

func (s Server) metricsCollector() MetricsCollector {
	if s.MetricsCollector != nil {
		return s.MetricsCollector
	}
	return defaultMetricsCollector()
}

func (s Server) metricsQueue() MetricsQueue {
	if s.MetricsQueue != nil {
		return s.MetricsQueue
	}
	return noopMetricsQueue{}
}

func (s Server) updater() Updater {
	if s.Updater != nil {
		return s.Updater
	}
	return defaultUpdater()
}

func (s Server) fileManager() FileManager {
	if s.FileManager != nil {
		return s.FileManager
	}
	return defaultFileManager()
}

func errorEnvelope(request proto.Envelope, err error) proto.Envelope {
	code := "internal_error"
	message := err.Error()
	if rpcErr, ok := err.(*RPCError); ok {
		code = rpcErr.Code
		message = rpcErr.Message
	}
	return proto.Envelope{
		Type:            proto.EnvelopeTypeResponse,
		ProtocolVersion: proto.CurrentProtocolVersion,
		RequestID:       request.RequestID,
		Action:          request.Action,
		Error: &proto.Error{
			Code:    code,
			Message: message,
		},
	}
}

// handleFileRPC decodes the request payload as T, runs the manager method, and
// encodes the typed response (or an error envelope). It collapses the per-action
// decode/dispatch/encode boilerplate shared by every file.* handler.
func handleFileRPC[T any, R any](channel ssh.Channel, request proto.Envelope, fn func(context.Context, T) (R, error)) {
	payload, err := proto.DecodePayload[T](request.Payload)
	if err != nil {
		_ = proto.Encode(channel, proto.Envelope{
			Type:            proto.EnvelopeTypeResponse,
			ProtocolVersion: proto.CurrentProtocolVersion,
			RequestID:       request.RequestID,
			Action:          request.Action,
			Error:           &proto.Error{Code: "bad_payload", Message: err.Error()},
		})
		return
	}
	result, err := fn(context.Background(), payload)
	if err != nil {
		_ = proto.Encode(channel, errorEnvelope(request, err))
		return
	}
	_ = proto.Encode(channel, proto.Envelope{
		Type:            proto.EnvelopeTypeResponse,
		ProtocolVersion: proto.CurrentProtocolVersion,
		RequestID:       request.RequestID,
		Action:          request.Action,
		Payload:         result,
	})
}

func controllerIDFromPayload(payload any) string {
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	controllerID, _ := payloadMap["controller_id"].(string)
	return controllerID
}

func loadAuthorizedKeys(path string) (map[string]struct{}, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// File not yet written (e.g. install race). Return empty set — connections
		// will be denied but the service keeps running and will succeed once the
		// file is in place.
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read authorized keys %s: %w", path, err)
	}
	keys := make(map[string]struct{})
	remaining := data
	for len(remaining) > 0 {
		before := len(remaining)
		pub, _, _, rest, err := ssh.ParseAuthorizedKey(remaining)
		if err != nil {
			// Skip malformed lines rather than stopping — one bad entry must not
			// silently block all keys that follow it.
			if len(rest) < before {
				remaining = rest
			} else {
				// ParseAuthorizedKey didn't advance; skip one byte to avoid infinite loop.
				remaining = remaining[1:]
			}
			continue
		}
		keys[string(pub.Marshal())] = struct{}{}
		remaining = rest
	}
	return keys, nil
}

func DefaultHostKeyPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return "fleet-agent-hostkey"
	}
	return filepath.Join(home, ".cenvero-fleet-agent", "ssh_host_ed25519_key")
}
