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
	"strings"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

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
}

func (s Server) Serve(ctx context.Context, listener net.Listener) error {
	signer, err := fleetcrypto.EnsureEd25519Signer(s.HostKeyPath)
	if err != nil {
		return err
	}

	authorizedKeys, err := loadAuthorizedKeys(s.AuthorizedKeysPath)
	if err != nil {
		return err
	}

	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if _, ok := authorizedKeys[string(key.Marshal())]; ok {
				return &ssh.Permissions{
					Extensions: map[string]string{
						"user": conn.User(),
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
	authorizedKeys, err := loadAuthorizedKeys(s.AuthorizedKeysPath)
	if err != nil {
		return err
	}
	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if _, ok := authorizedKeys[string(key.Marshal())]; ok {
				return &ssh.Permissions{
					Extensions: map[string]string{
						"user": conn.User(),
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
		if newChannel.ChannelType() != transport.RPCChannelType {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(requests)
		go s.serveRPC(channel)
	}
	return nil
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
			payload, err := proto.DecodePayload[proto.UpdateApplyPayload](request.Payload)
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
			op, err := s.updater().Apply(context.Background(), payload)
			if err != nil {
				_ = proto.Encode(channel, errorEnvelope(request, err))
				continue
			}
			if err := proto.Encode(channel, proto.Envelope{
				Type:            proto.EnvelopeTypeResponse,
				ProtocolVersion: proto.CurrentProtocolVersion,
				RequestID:       request.RequestID,
				Action:          request.Action,
				Payload:         op.Result,
			}); err != nil {
				continue
			}
			if op.Finalize != nil {
				_ = op.Finalize()
			}
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

func controllerIDFromPayload(payload any) string {
	payloadMap, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	controllerID, _ := payloadMap["controller_id"].(string)
	return controllerID
}

func loadAuthorizedKeys(path string) (map[string]struct{}, error) {
	if path == "" {
		return nil, fmt.Errorf("authorized keys path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read authorized keys %s: %w", path, err)
	}
	keys := make(map[string]struct{})
	remaining := data
	for len(remaining) > 0 {
		pub, _, _, rest, err := ssh.ParseAuthorizedKey(remaining)
		if err != nil {
			break
		}
		keys[string(pub.Marshal())] = struct{}{}
		remaining = rest
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid authorized keys found in %s", path)
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
