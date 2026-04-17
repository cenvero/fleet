// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
	"golang.org/x/crypto/ssh"
)

type ReverseSessionInfo struct {
	Server             string             `json:"server"`
	Connected          bool               `json:"connected"`
	ConnectedAt        time.Time          `json:"connected_at,omitempty"`
	HostKeyFingerprint string             `json:"host_key_fingerprint,omitempty"`
	Hello              proto.HelloPayload `json:"hello,omitempty"`
	ReplayedMetrics    int                `json:"replayed_metrics,omitempty"`
}

type reverseSession struct {
	session *transport.Session
	info    ReverseSessionInfo
}

type ReverseHub struct {
	app      *App
	mu       sync.RWMutex
	sessions map[string]*reverseSession
}

type reverseControlRequest struct {
	Type     string         `json:"type"`
	Server   string         `json:"server"`
	Envelope proto.Envelope `json:"envelope,omitempty"`
}

type reverseControlResponse struct {
	Response *proto.Envelope     `json:"response,omitempty"`
	Status   *ReverseSessionInfo `json:"status,omitempty"`
	Error    *proto.Error        `json:"error,omitempty"`
}

func NewReverseHub(app *App) *ReverseHub {
	return &ReverseHub{
		app:      app,
		sessions: make(map[string]*reverseSession),
	}
}

func (h *ReverseHub) Serve(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
		h.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept reverse transport connection: %w", err)
		}
		go func() {
			_ = h.ServeConn(conn)
		}()
	}
}

func (h *ReverseHub) ServeConn(rawConn net.Conn) error {
	defer rawConn.Close()

	signer, err := fleetcrypto.LoadPrivateKeySigner(h.app.controllerPrivateKeyPath(), nil)
	if err != nil {
		return err
	}

	config := &ssh.ServerConfig{
		Config: ssh.Config{
			Ciphers: transport.SupportedCiphers(),
		},
		PublicKeyCallback: h.authorizeAgent,
	}
	config.AddHostKey(signer)

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

		serverName := conn.User()
		session := &transport.Session{
			Mode:               transport.ModeReverse,
			LocalAddr:          conn.LocalAddr(),
			RemoteAddr:         conn.RemoteAddr(),
			HostKeyFingerprint: conn.Permissions.Extensions["fingerprint"],
			Channel:            channel,
			Closer:             conn,
		}

		hello, err := session.Hello(context.Background(), h.app.Config.InstanceID)
		if err != nil {
			_ = session.Close()
			return err
		}

		info := ReverseSessionInfo{
			Server:             serverName,
			Connected:          true,
			ConnectedAt:        time.Now().UTC(),
			HostKeyFingerprint: conn.Permissions.Extensions["fingerprint"],
			Hello:              hello,
		}
		replayed, replayErr := h.replayQueuedMetrics(serverName, session)
		if replayErr != nil {
			_ = h.app.AuditLog.Append(logs.AuditEntry{
				Action:   "metrics.replay.failed",
				Target:   serverName,
				Operator: h.app.operator(),
				Details:  replayErr.Error(),
			})
		} else {
			info.ReplayedMetrics = replayed
		}
		h.setSession(serverName, session, info)

		go func(name string, sshConn *ssh.ServerConn) {
			_ = sshConn.Wait()
			h.clearSession(name, "")
		}(serverName, conn)
	}
	return nil
}

func (h *ReverseHub) replayQueuedMetrics(serverName string, session *transport.Session) (int, error) {
	response, err := session.Call(context.Background(), proto.Envelope{
		Action: "metrics.flush_queue",
		Payload: proto.MetricsPayload{
			Server: serverName,
		},
	})
	if err != nil {
		return 0, err
	}
	if response.Error != nil {
		if response.Error.Code == "unsupported_action" {
			return 0, nil
		}
		return 0, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	replay, err := proto.DecodePayload[proto.MetricsReplayResult](response.Payload)
	if err != nil {
		return 0, err
	}
	if len(replay.Snapshots) == 0 {
		return 0, nil
	}

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return 0, err
	}

	latest := server.Metrics
	for _, snapshot := range replay.Snapshots {
		if err := h.app.persistMetricsSnapshot(serverName, snapshot); err != nil {
			return 0, err
		}
		if latest.Timestamp.IsZero() || snapshot.Timestamp.After(latest.Timestamp) {
			latest = snapshot
		}
	}
	server.Metrics = latest
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = ""
	if err := h.app.SaveServer(server); err != nil {
		return 0, err
	}
	if err := h.app.evaluateMetricAlerts(serverName, latest); err != nil {
		return 0, err
	}
	if err := h.app.Alerts.Delete(collectionFailureAlertID(serverName)); err != nil {
		return 0, err
	}
	if err := h.app.AuditLog.Append(logs.AuditEntry{
		Action:   "metrics.replay",
		Target:   serverName,
		Operator: h.app.operator(),
		Details:  fmt.Sprintf("snapshots=%d", len(replay.Snapshots)),
	}); err != nil {
		return 0, err
	}
	return len(replay.Snapshots), nil
}

func (h *ReverseHub) Call(server string, env proto.Envelope) (proto.Envelope, error) {
	h.mu.RLock()
	current := h.sessions[server]
	h.mu.RUnlock()
	if current == nil || current.session == nil {
		return proto.Envelope{}, fmt.Errorf("reverse session for %q is not connected", server)
	}
	response, err := current.session.Call(context.Background(), env)
	if err != nil {
		h.clearSession(server, err.Error())
		return proto.Envelope{}, err
	}
	return response, nil
}

func (h *ReverseHub) Status(server string) (ReverseSessionInfo, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	current := h.sessions[server]
	if current == nil {
		return ReverseSessionInfo{}, fmt.Errorf("reverse session for %q is not connected", server)
	}
	return current.info, nil
}

func (h *ReverseHub) Disconnect(server string) error {
	h.mu.RLock()
	current := h.sessions[server]
	h.mu.RUnlock()
	if current == nil || current.session == nil {
		return fmt.Errorf("reverse session for %q is not connected", server)
	}
	return current.session.Close()
}

func (h *ReverseHub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, session := range h.sessions {
		if session.session != nil {
			_ = session.session.Close()
		}
		delete(h.sessions, name)
	}
}

func (h *ReverseHub) ServeControl(ctx context.Context, listener net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept control connection: %w", err)
		}
		go h.handleControlConn(conn)
	}
}

func (h *ReverseHub) handleControlConn(conn net.Conn) {
	defer conn.Close()

	var req reverseControlRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(reverseControlResponse{
			Error: &proto.Error{Code: "decode_error", Message: err.Error()},
		})
		return
	}

	var resp reverseControlResponse
	switch req.Type {
	case "call":
		out, err := h.Call(req.Server, req.Envelope)
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_call_failed", Message: err.Error()}
			break
		}
		resp.Response = &out
	case "status":
		info, err := h.Status(req.Server)
		if err != nil {
			resp.Error = &proto.Error{Code: "reverse_status_failed", Message: err.Error()}
			break
		}
		resp.Status = &info
	case "disconnect":
		if err := h.Disconnect(req.Server); err != nil {
			resp.Error = &proto.Error{Code: "reverse_disconnect_failed", Message: err.Error()}
			break
		}
	default:
		resp.Error = &proto.Error{Code: "unsupported_action", Message: fmt.Sprintf("control request %q is not supported", req.Type)}
	}

	_ = json.NewEncoder(conn).Encode(resp)
}

func (h *ReverseHub) authorizeAgent(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	serverName := conn.User()
	if serverName == "" {
		return nil, fmt.Errorf("reverse agent must provide a server name as the SSH username")
	}
	server, err := h.app.GetServer(serverName)
	if err != nil {
		return nil, fmt.Errorf("reverse server %q is not registered: %w", serverName, err)
	}
	if server.Mode != transport.ModeReverse && server.Mode != transport.ModePerNode {
		return nil, fmt.Errorf("server %q is not configured for reverse mode", serverName)
	}

	path := filepath.Join(h.app.ConfigDir, "keys", "agents", serverName+".pub")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create reverse agent key directory: %w", err)
	}

	if data, err := os.ReadFile(path); err == nil {
		existing, _, _, _, parseErr := ssh.ParseAuthorizedKey(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse pinned reverse key for %s: %w", serverName, parseErr)
		}
		if string(existing.Marshal()) != string(key.Marshal()) {
			return nil, fmt.Errorf("reverse agent key mismatch for %s", serverName)
		}
	} else if os.IsNotExist(err) {
		if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(key), 0o644); err != nil { // #nosec G306 -- public key is intentionally world-readable
			return nil, fmt.Errorf("pin reverse agent key for %s: %w", serverName, err)
		}
	} else {
		return nil, fmt.Errorf("read pinned reverse key for %s: %w", serverName, err)
	}

	return &ssh.Permissions{
		Extensions: map[string]string{
			"server":      serverName,
			"fingerprint": ssh.FingerprintSHA256(key),
		},
	}, nil
}

func (h *ReverseHub) setSession(serverName string, session *transport.Session, info ReverseSessionInfo) {
	h.mu.Lock()
	if existing := h.sessions[serverName]; existing != nil && existing.session != nil {
		_ = existing.session.Close()
	}
	h.sessions[serverName] = &reverseSession{session: session, info: info}
	h.mu.Unlock()

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return
	}
	server.Capabilities = info.Hello.Capabilities
	server.Observed = ServerObservation{
		Reachable:          true,
		LastSeen:           time.Now().UTC(),
		LastError:          "",
		NodeName:           info.Hello.NodeName,
		AgentVersion:       info.Hello.AgentVersion,
		OS:                 info.Hello.OS,
		Arch:               info.Hello.Arch,
		Transport:          info.Hello.Transport,
		HostKeyFingerprint: info.HostKeyFingerprint,
	}
	_ = h.app.SaveServer(server)
	_ = h.app.AuditLog.Append(logs.AuditEntry{
		Action:   "server.reverse.connect",
		Target:   serverName,
		Operator: h.app.operator(),
		Details:  fmt.Sprintf("fingerprint=%s capabilities=%d", info.HostKeyFingerprint, len(info.Hello.Capabilities)),
	})

	// Auto-update the agent only when the policy permits it.
	if info.Hello.AgentVersion != "" && info.Hello.AgentVersion != version.Version &&
		h.app.Config.Updates.Policy == update.PolicyAutoUpdate {
		go func() {
			_ = h.app.AuditLog.Append(logs.AuditEntry{
				Action:   "agent.auto-update",
				Target:   serverName,
				Operator: "system",
				Details:  fmt.Sprintf("agent=%s controller=%s", info.Hello.AgentVersion, version.Version),
			})
			h.app.applyAgentUpdate(context.Background(), server)
		}()
	}
}

func (h *ReverseHub) clearSession(serverName, lastError string) {
	h.mu.Lock()
	current := h.sessions[serverName]
	if current != nil {
		delete(h.sessions, serverName)
	}
	h.mu.Unlock()

	server, err := h.app.GetServer(serverName)
	if err != nil {
		return
	}
	server.Observed.Reachable = false
	server.Observed.LastSeen = time.Now().UTC()
	server.Observed.LastError = lastError
	_ = h.app.SaveServer(server)
}

func (a *App) reverseStatus(serverName string) (ReverseSessionInfo, error) {
	if a.ReverseStatusLookup != nil {
		return a.ReverseStatusLookup(serverName)
	}
	return a.callReverseStatus(serverName)
}

func (a *App) reverseDisconnect(serverName string) error {
	if a.ReverseDisconnect != nil {
		return a.ReverseDisconnect(serverName)
	}
	return a.callReverseDisconnect(serverName)
}

func (a *App) callReverseControl(serverName string, env proto.Envelope) (proto.Envelope, error) {
	conn, err := net.DialTimeout("tcp", a.Config.Runtime.ControlAddress, 2*time.Second)
	if err != nil {
		return proto.Envelope{}, fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(reverseControlRequest{
		Type:     "call",
		Server:   serverName,
		Envelope: env,
	}); err != nil {
		return proto.Envelope{}, err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return proto.Envelope{}, err
	}
	if resp.Error != nil {
		return proto.Envelope{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Response == nil {
		return proto.Envelope{}, fmt.Errorf("reverse control did not return a response envelope")
	}
	return *resp.Response, nil
}

func (a *App) callReverseStatus(serverName string) (ReverseSessionInfo, error) {
	conn, err := net.DialTimeout("tcp", a.Config.Runtime.ControlAddress, 2*time.Second)
	if err != nil {
		return ReverseSessionInfo{}, fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(reverseControlRequest{
		Type:   "status",
		Server: serverName,
	}); err != nil {
		return ReverseSessionInfo{}, err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return ReverseSessionInfo{}, err
	}
	if resp.Error != nil {
		return ReverseSessionInfo{}, fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.Status == nil {
		return ReverseSessionInfo{}, fmt.Errorf("reverse control did not return session status")
	}
	return *resp.Status, nil
}

func (a *App) callReverseDisconnect(serverName string) error {
	conn, err := net.DialTimeout("tcp", a.Config.Runtime.ControlAddress, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to local reverse control at %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(reverseControlRequest{
		Type:   "disconnect",
		Server: serverName,
	}); err != nil {
		return err
	}

	var resp reverseControlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

func (a *App) RunDaemon(ctx context.Context) error {
	reverseListener, err := net.Listen("tcp", a.Config.Runtime.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for reverse agents on %s: %w", a.Config.Runtime.ListenAddress, err)
	}
	defer reverseListener.Close()

	controlListener, err := net.Listen("tcp", a.Config.Runtime.ControlAddress)
	if err != nil {
		return fmt.Errorf("listen for local control on %s: %w", a.Config.Runtime.ControlAddress, err)
	}
	defer controlListener.Close()

	hub := NewReverseHub(a)
	errCh := make(chan error, 2)
	go func() { errCh <- hub.Serve(ctx, reverseListener) }()
	go func() { errCh <- hub.ServeControl(ctx, controlListener) }()
	go a.runMetricsPoller(ctx)

	select {
	case <-ctx.Done():
		hub.Close()
		return nil
	case err := <-errCh:
		hub.Close()
		return err
	}
}

func (a *App) controllerPrivateKeyPath() string {
	return filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
}
