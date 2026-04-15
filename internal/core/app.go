// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/notify"
	"github.com/cenvero/fleet/internal/store"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
	"github.com/cenvero/fleet/internal/version"
	"github.com/cenvero/fleet/pkg/proto"
)

var ErrNotInitialized = errors.New("cenvero fleet is not initialized")

type App struct {
	ConfigDir           string
	Config              Config
	ExecutablePath      string
	AuditLog            *logs.AuditLog
	Alerts              *alerts.Store
	Notifier            notify.Notifier
	BootstrapExecutor   BootstrapExecutor
	ControllerUpdater   func(context.Context, update.ApplyOptions) (update.ApplyResult, error)
	StateDB             *store.Store
	MetricsDB           *store.Store
	NetworkDialContext  func(context.Context, string, string) (net.Conn, error)
	ReverseRPC          func(string, proto.Envelope) (proto.Envelope, error)
	ReverseStatusLookup func(string) (ReverseSessionInfo, error)
	ReverseDisconnect   func(string) error

	serverMu sync.RWMutex
}

func Open(configDir string) (*App, error) {
	if configDir == "" {
		configDir = DefaultConfigDir("")
	}
	cfg, err := LoadConfig(ConfigPath(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotInitialized
		}
		return nil, err
	}
	stateDB, err := store.Open(cfg.Database, store.WorkloadState)
	if err != nil {
		return nil, err
	}
	metricsDB, err := store.Open(cfg.Database, store.WorkloadMetrics)
	if err != nil {
		_ = stateDB.Close()
		return nil, err
	}
	return &App{
		ConfigDir: configDir,
		Config:    cfg,
		AuditLog:  logs.NewAuditLog(filepath.Join(configDir, "logs", "_audit.log")),
		Alerts:    alerts.NewStore(filepath.Join(configDir, "alerts")),
		Notifier:  notify.NewDesktopNotifier(cfg.Runtime.DesktopNotifications),
		StateDB:   stateDB,
		MetricsDB: metricsDB,
	}, nil
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	var firstErr error
	if a.StateDB != nil {
		if err := a.StateDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if a.MetricsDB != nil {
		if err := a.MetricsDB.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (a *App) Status() (Status, error) {
	servers, err := a.ListServers()
	if err != nil {
		return Status{}, err
	}
	fingerprints, err := crypto.Fingerprints(filepath.Join(a.ConfigDir, "keys"))
	if err != nil {
		return Status{}, err
	}
	return Status{
		Initialized:     true,
		ConfigDir:       a.ConfigDir,
		ProductName:     a.Config.ProductName,
		Version:         version.Version,
		DefaultMode:     a.Config.DefaultMode,
		Alias:           a.Config.Alias,
		ServerCount:     len(servers),
		Channel:         a.Config.Updates.Channel,
		Policy:          a.Config.Updates.Policy,
		DatabaseBackend: a.Config.Database.Backend,
		Fingerprints:    fingerprints,
	}, nil
}

func (a *App) UpdateChannel(channel string) error {
	a.Config.Updates.Channel = channel
	if err := SaveConfig(ConfigPath(a.ConfigDir), a.Config); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "update.channel",
		Target:   channel,
		Operator: a.operator(),
	})
}

func (a *App) AddServer(record ServerRecord) error {
	if record.Name == "" {
		return fmt.Errorf("server name is required")
	}
	if record.Port == 0 {
		record.Port = 22
	}
	if record.User == "" {
		record.User = "root"
	}
	if record.Mode == "" {
		record.Mode = a.Config.DefaultMode
		if record.Mode == transport.ModePerNode {
			record.Mode = transport.ModeDirect
		}
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	record.UpdatedAt = time.Now().UTC()
	if err := a.writeServerFile(record); err != nil {
		return fmt.Errorf("create server record: %w", err)
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "server.add",
		Target:   record.Name,
		Operator: a.operator(),
		Details:  fmt.Sprintf("mode=%s address=%s port=%d user=%s", record.Mode, record.Address, record.Port, record.User),
	})
}

func (a *App) ListServers() ([]ServerRecord, error) {
	a.serverMu.RLock()
	entries, err := os.ReadDir(filepath.Join(a.ConfigDir, "servers"))
	a.serverMu.RUnlock()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read servers directory: %w", err)
	}
	var servers []ServerRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		path := filepath.Join(a.ConfigDir, "servers", entry.Name())
		a.serverMu.RLock()
		var server ServerRecord
		_, err := toml.DecodeFile(path, &server)
		a.serverMu.RUnlock()
		if err != nil {
			return nil, fmt.Errorf("decode server %s: %w", entry.Name(), err)
		}
		servers = append(servers, server)
	}
	slices.SortFunc(servers, func(a, b ServerRecord) int {
		return strings.Compare(a.Name, b.Name)
	})
	return servers, nil
}

func (a *App) GetServer(name string) (ServerRecord, error) {
	path := filepath.Join(a.ConfigDir, "servers", name+".toml")
	a.serverMu.RLock()
	var server ServerRecord
	_, err := toml.DecodeFile(path, &server)
	a.serverMu.RUnlock()
	if err != nil {
		return ServerRecord{}, fmt.Errorf("load server %s: %w", name, err)
	}
	return server, nil
}

func (a *App) SaveServer(server ServerRecord) error {
	server.UpdatedAt = time.Now().UTC()
	return a.writeServerFile(server)
}

func (a *App) writeServerFile(server ServerRecord) error {
	path := filepath.Join(a.ConfigDir, "servers", server.Name+".toml")
	a.serverMu.Lock()
	defer a.serverMu.Unlock()
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("write server %s: %w", server.Name, err)
	}
	defer f.Close()
	return toml.NewEncoder(f).Encode(server)
}

func (a *App) RemoveServer(name string) error {
	server, err := a.GetServer(name)
	if err != nil {
		return err
	}
	if server.Agent.Managed {
		if teardownErr := a.TeardownAgent(server); teardownErr != nil {
			return fmt.Errorf("teardown agent on %s: %w", name, teardownErr)
		}
	}
	path := filepath.Join(a.ConfigDir, "servers", name+".toml")
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove server %s: %w", name, err)
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "server.remove",
		Target:   name,
		Operator: a.operator(),
	})
}

func (a *App) ReconnectServer(name string, acceptNewHostKey bool) error {
	server, err := a.GetServer(name)
	if err != nil {
		return err
	}
	if server.Mode == transport.ModeReverse {
		info, err := a.reverseStatus(name)
		if err != nil {
			return err
		}
		event := proto.Envelope{
			Type:            proto.EnvelopeTypeEvent,
			ProtocolVersion: proto.CurrentProtocolVersion,
			Action:          "server.reconnect",
			Capabilities:    info.Hello.Capabilities,
			Payload:         info.Hello,
		}
		payload, _ := json.Marshal(event)
		return a.AuditLog.Append(logs.AuditEntry{
			Action:   "server.reconnect",
			Target:   name,
			Operator: a.operator(),
			Details:  fmt.Sprintf("reachable=true host_key=%s payload=%s", info.HostKeyFingerprint, string(payload)),
		})
	}
	session, hello, err := a.openDirectSession(server, acceptNewHostKey)
	if err != nil {
		return err
	}
	defer session.Close()

	event := proto.Envelope{
		Type:            proto.EnvelopeTypeEvent,
		ProtocolVersion: proto.CurrentProtocolVersion,
		Action:          "server.reconnect",
		Capabilities:    server.Capabilities,
		Payload:         hello,
	}
	payload, _ := json.Marshal(event)
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "server.reconnect",
		Target:   name,
		Operator: a.operator(),
		Details:  fmt.Sprintf("reachable=true host_key=%s payload=%s", session.HostKeyFingerprint, string(payload)),
	})
}

func (a *App) SetServerMode(name string, mode transport.Mode) error {
	server, err := a.GetServer(name)
	if err != nil {
		return err
	}
	server.Mode = mode
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "server.mode",
		Target:   name,
		Operator: a.operator(),
		Details:  string(mode),
	})
}

func (a *App) AddService(serverName, serviceName, logPath string, critical bool) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}
	updated := false
	for i := range server.Services {
		if server.Services[i].Name == serviceName {
			server.Services[i].LogPath = logPath
			server.Services[i].Critical = critical
			server.Services[i].LastAction = "registered"
			updated = true
			break
		}
	}
	if !updated {
		server.Services = append(server.Services, ServiceRecord{
			Name:       serviceName,
			LogPath:    logPath,
			Critical:   critical,
			LastAction: "registered",
		})
	}
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "service.add",
		Target:   serverName + "/" + serviceName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("critical=%t", critical),
	})
}

func (a *App) ListTrackedServices(serverName string) ([]ServiceRecord, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return nil, err
	}
	return server.Services, nil
}

func (a *App) ListServices(serverName string) ([]ServiceRecord, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return nil, err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "service.list",
		Payload: map[string]any{
			"server": server.Name,
		},
	})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	live, err := proto.DecodePayload[[]proto.ServiceInfo](response.Payload)
	if err != nil {
		return nil, err
	}
	return mergeLiveServices(server.Services, live), nil
}

func (a *App) ControlService(serverName, serviceName, action string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "service.control",
		Payload: proto.ServiceActionPayload{
			Server:  serverName,
			Service: serviceName,
			Action:  action,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	info, err := proto.DecodePayload[proto.ServiceInfo](response.Payload)
	if err != nil {
		return err
	}
	server.Services = upsertTrackedService(server.Services, serviceName, action, info)
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "service." + action,
		Target:   serverName + "/" + serviceName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("state=%s/%s", info.ActiveState, info.SubState),
	})
}

func (a *App) ReadServiceLogs(serverName, serviceName, search string, tailLines int, follow bool) (proto.LogReadResult, error) {
	return a.readServiceLogs(serverName, serviceName, search, tailLines, follow, true)
}

func (a *App) ReadCachedServiceLogs(serverName, serviceName, search string, tailLines int) (proto.LogReadResult, error) {
	if _, _, err := a.trackedService(serverName, serviceName); err != nil {
		return proto.LogReadResult{}, err
	}
	result, err := a.aggregatedLogs().Read(serverName, serviceName, search, tailLines)
	if err != nil {
		return proto.LogReadResult{}, err
	}
	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "service.logs.cached",
		Target:   serverName + "/" + serviceName,
		Operator: a.operator(),
		Details:  fmt.Sprintf("path=%s lines=%d truncated=%t search=%q", result.Path, len(result.Lines), result.Truncated, search),
	}); err != nil {
		return proto.LogReadResult{}, err
	}
	return result, nil
}

func (a *App) readServiceLogs(serverName, serviceName, search string, tailLines int, follow bool, recordAudit bool) (proto.LogReadResult, error) {
	server, service, err := a.trackedService(serverName, serviceName)
	if err != nil {
		return proto.LogReadResult{}, err
	}
	if strings.TrimSpace(service.LogPath) == "" {
		return proto.LogReadResult{}, fmt.Errorf("service %q on %q does not have a tracked log path", serviceName, serverName)
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "log.read",
		Payload: proto.LogReadPayload{
			Server:    serverName,
			Service:   serviceName,
			Path:      service.LogPath,
			Search:    search,
			TailLines: tailLines,
			Follow:    false,
		},
	})
	if err != nil {
		return proto.LogReadResult{}, err
	}
	if response.Error != nil {
		return proto.LogReadResult{}, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	result, err := proto.DecodePayload[proto.LogReadResult](response.Payload)
	if err != nil {
		return proto.LogReadResult{}, err
	}
	if strings.TrimSpace(search) == "" {
		if err := a.aggregatedLogs().Append(serverName, serviceName, result.Lines); err != nil {
			return proto.LogReadResult{}, err
		}
	}
	if recordAudit {
		if err := a.AuditLog.Append(logs.AuditEntry{
			Action:   "service.logs",
			Target:   serverName + "/" + serviceName,
			Operator: a.operator(),
			Details:  fmt.Sprintf("path=%s lines=%d truncated=%t search=%q follow=%t", result.Path, len(result.Lines), result.Truncated, search, follow),
		}); err != nil {
			return proto.LogReadResult{}, err
		}
	}
	return result, nil
}

func (a *App) ListPorts(serverName string) ([]int, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return nil, err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "port.list",
		Payload: map[string]any{
			"server": serverName,
		},
	})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	ports, err := proto.DecodePayload[[]int](response.Payload)
	if err != nil {
		return nil, err
	}
	server.OpenPorts = append([]int(nil), ports...)
	slices.Sort(server.OpenPorts)
	if err := a.SaveServer(server); err != nil {
		return nil, err
	}
	return server.OpenPorts, nil
}

func (a *App) SetPort(serverName string, port int, open bool) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "port.set",
		Payload: proto.PortActionPayload{
			Server: serverName,
			Port:   port,
			Open:   open,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	info, err := proto.DecodePayload[proto.FirewallInfo](response.Payload)
	if err != nil {
		return err
	}
	applyFirewallInfo(&server, info)
	if err := a.SaveServer(server); err != nil {
		return err
	}
	state := "close"
	if open {
		state = "open"
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "port." + state,
		Target:   fmt.Sprintf("%s/%d", serverName, port),
		Operator: a.operator(),
	})
}

func (a *App) FirewallStatus(serverName string) (FirewallState, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return FirewallState{}, err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "firewall.status",
		Payload: map[string]any{
			"server": serverName,
		},
	})
	if err != nil {
		return FirewallState{}, err
	}
	if response.Error != nil {
		return FirewallState{}, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	info, err := proto.DecodePayload[proto.FirewallInfo](response.Payload)
	if err != nil {
		return FirewallState{}, err
	}
	applyFirewallInfo(&server, info)
	if err := a.SaveServer(server); err != nil {
		return FirewallState{}, err
	}
	return server.Firewall, nil
}

func (a *App) SetFirewall(serverName string, enabled bool) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	actionName := "firewall.disable"
	if enabled {
		actionName = "firewall.enable"
	}
	response, err := a.callRPC(server, proto.Envelope{
		Action: actionName,
		Payload: map[string]any{
			"server": serverName,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	info, err := proto.DecodePayload[proto.FirewallInfo](response.Payload)
	if err != nil {
		return err
	}
	applyFirewallInfo(&server, info)
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   actionName,
		Target:   serverName,
		Operator: a.operator(),
	})
}

func (a *App) AddFirewallRule(serverName, rule string) error {
	server, err := a.GetServer(serverName)
	if err != nil {
		return err
	}

	response, err := a.callRPC(server, proto.Envelope{
		Action: "firewall.add_rule",
		Payload: proto.FirewallRulePayload{
			Server: serverName,
			Rule:   rule,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}

	info, err := proto.DecodePayload[proto.FirewallInfo](response.Payload)
	if err != nil {
		return err
	}
	applyFirewallInfo(&server, info)
	if err := a.SaveServer(server); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "firewall.rule.add",
		Target:   serverName,
		Operator: a.operator(),
		Details:  rule,
	})
}

type ExecServerResult struct {
	Server string
	Result proto.ExecResult
	Error  error
}

func (a *App) ExecCommand(serverName, command string) (proto.ExecResult, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return proto.ExecResult{}, err
	}
	response, err := a.callRPC(server, proto.Envelope{
		Action:  "shell.exec",
		Payload: proto.ExecPayload{Command: command},
	})
	if err != nil {
		return proto.ExecResult{}, err
	}
	if response.Error != nil {
		return proto.ExecResult{}, fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	return proto.DecodePayload[proto.ExecResult](response.Payload)
}

func (a *App) ExecCommandAll(command string) []ExecServerResult {
	servers, err := a.ListServers()
	if err != nil {
		return []ExecServerResult{{Error: err}}
	}
	results := make([]ExecServerResult, len(servers))
	var wg sync.WaitGroup
	for i, server := range servers {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			result, err := a.ExecCommand(name, command)
			results[i] = ExecServerResult{Server: name, Result: result, Error: err}
		}(i, server.Name)
	}
	wg.Wait()
	return results
}

func (a *App) ListAlerts(server, severity string) ([]alerts.Alert, error) {
	return a.Alerts.ListFiltered(server, severity)
}

func (a *App) AckAlert(id string) error {
	if err := a.Alerts.Ack(id); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "alert.ack",
		Target:   id,
		Operator: a.operator(),
	})
}

func (a *App) SuppressAlert(id string, duration time.Duration) error {
	if duration <= 0 {
		return fmt.Errorf("suppression duration must be positive")
	}
	until := time.Now().UTC().Add(duration)
	if err := a.Alerts.Suppress(id, until); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "alert.suppress",
		Target:   id,
		Operator: a.operator(),
		Details:  until.Format(time.RFC3339),
	})
}

func (a *App) UnsuppressAlert(id string) error {
	if err := a.Alerts.Unsuppress(id); err != nil {
		return err
	}
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "alert.unsuppress",
		Target:   id,
		Operator: a.operator(),
	})
}

func (a *App) ListTemplates() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(a.ConfigDir, "templates"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names, nil
}

func (a *App) Backup(outputPath string) (string, error) {
	backupPath, err := BackupDir(a.ConfigDir, outputPath)
	if err != nil {
		return "", err
	}
	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "config.backup",
		Target:   backupPath,
		Operator: a.operator(),
	}); err != nil {
		return "", err
	}
	return backupPath, nil
}

func (a *App) Export() (ConfigExport, error) {
	servers, err := a.ListServers()
	if err != nil {
		return ConfigExport{}, err
	}
	return ConfigExport{Config: a.Config, Servers: servers}, nil
}

func (a *App) Import(export ConfigExport) error {
	export.Config.ConfigDir = a.ConfigDir
	if err := SaveConfig(ConfigPath(a.ConfigDir), export.Config); err != nil {
		return err
	}
	for _, server := range export.Servers {
		if err := a.SaveServer(server); err != nil {
			return err
		}
	}
	a.Config = export.Config
	return a.AuditLog.Append(logs.AuditEntry{
		Action:   "config.import",
		Target:   a.ConfigDir,
		Operator: a.operator(),
	})
}

func (a *App) AuditEntries() ([]logs.AuditEntry, error) {
	return a.AuditLog.ReadAll()
}

func (a *App) aggregatedLogs() *logs.ServiceStore {
	maxAge := logs.DefaultAggregatedLogMaxAge
	if a.Config.Runtime.AggregatedLogMaxAge != "" {
		if parsed, err := time.ParseDuration(a.Config.Runtime.AggregatedLogMaxAge); err == nil && parsed > 0 {
			maxAge = parsed
		}
	}
	return logs.NewServiceStore(
		a.Config.Runtime.AggregatedLogDir,
		a.Config.Runtime.AggregatedLogMaxSize,
		a.Config.Runtime.AggregatedLogMaxFiles,
		maxAge,
	)
}

func (a *App) trackedService(serverName, serviceName string) (ServerRecord, ServiceRecord, error) {
	server, err := a.GetServer(serverName)
	if err != nil {
		return ServerRecord{}, ServiceRecord{}, err
	}
	for _, service := range server.Services {
		if service.Name == serviceName {
			return server, service, nil
		}
	}
	return ServerRecord{}, ServiceRecord{}, fmt.Errorf("service %q not found on %q", serviceName, serverName)
}

func applyFirewallInfo(server *ServerRecord, info proto.FirewallInfo) {
	server.Firewall.Enabled = info.Enabled
	server.Firewall.Rules = append([]string(nil), info.Rules...)
	server.OpenPorts = append([]int(nil), info.OpenPorts...)
	slices.Sort(server.OpenPorts)
}

func (a *App) callRPC(server ServerRecord, env proto.Envelope) (proto.Envelope, error) {
	switch server.Mode {
	case transport.ModeDirect:
		session, _, err := a.openDirectSession(server, false)
		if err != nil {
			return proto.Envelope{}, err
		}
		defer session.Close()
		return session.Call(context.Background(), env)
	case transport.ModeReverse:
		if a.ReverseRPC != nil {
			return a.ReverseRPC(server.Name, env)
		}
		return a.callReverseControl(server.Name, env)
	default:
		return proto.Envelope{}, fmt.Errorf("transport mode %q is not implemented for live actions", server.Mode)
	}
}

func (a *App) operator() string {
	if a.Config.Operator != "" {
		return a.Config.Operator
	}
	currentUser, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return currentUser.Username
}

func runtimeCapability(capability string) bool {
	switch capability {
	case "service.manage", "firewall.manage":
		return runtime.GOOS == "linux"
	default:
		return false
	}
}

func (a *App) serverPrivateKeyPath(server ServerRecord) string {
	if server.KeyPath != "" {
		return server.KeyPath
	}
	return filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
}

func (a *App) openDirectSession(server ServerRecord, acceptNewHostKey bool) (*transport.Session, proto.HelloPayload, error) {
	return a.openDirectSessionWithKey(server, a.serverPrivateKeyPath(server), acceptNewHostKey)
}

func (a *App) openDirectSessionWithKey(server ServerRecord, privateKeyPath string, acceptNewHostKey bool) (*transport.Session, proto.HelloPayload, error) {
	if server.Mode != transport.ModeDirect {
		server.Observed.Reachable = false
		server.Observed.LastSeen = time.Now().UTC()
		server.Observed.LastError = fmt.Sprintf("mode %s is not implemented yet for live direct actions", server.Mode)
		_ = a.SaveServer(server)
		return nil, proto.HelloPayload{}, fmt.Errorf("server %q is configured for %s mode; only direct mode is implemented right now", server.Name, server.Mode)
	}

	target := transport.ServerTarget{
		Name:    server.Name,
		Address: server.Address,
		Port:    server.Port,
		Mode:    server.Mode,
		User:    server.User,
	}
	connector := transport.Connector{
		Mode:               server.Mode,
		Username:           server.User,
		PrivateKeyPath:     privateKeyPath,
		KnownHostsPath:     a.Config.Crypto.KnownHostsPath,
		AcceptNewHostKey:   acceptNewHostKey,
		NetworkDialContext: a.NetworkDialContext,
	}

	session, err := connector.DialContext(context.Background(), target)
	if err != nil {
		server.Observed.Reachable = false
		server.Observed.LastSeen = time.Now().UTC()
		server.Observed.LastError = err.Error()
		_ = a.SaveServer(server)
		return nil, proto.HelloPayload{}, err
	}

	hello, err := session.Hello(context.Background(), a.Config.InstanceID)
	if err != nil {
		_ = session.Close()
		server.Observed.Reachable = false
		server.Observed.LastSeen = time.Now().UTC()
		server.Observed.LastError = err.Error()
		_ = a.SaveServer(server)
		return nil, proto.HelloPayload{}, err
	}

	server.Capabilities = hello.Capabilities
	server.Observed = ServerObservation{
		Reachable:          true,
		LastSeen:           time.Now().UTC(),
		LastError:          "",
		NodeName:           hello.NodeName,
		AgentVersion:       hello.AgentVersion,
		OS:                 hello.OS,
		Arch:               hello.Arch,
		Transport:          hello.Transport,
		HostKeyFingerprint: session.HostKeyFingerprint,
	}
	_ = a.SaveServer(server)
	return session, hello, nil
}

func mergeLiveServices(tracked []ServiceRecord, live []proto.ServiceInfo) []ServiceRecord {
	lookup := make(map[string]ServiceRecord, len(tracked))
	for _, record := range tracked {
		lookup[record.Name] = record
	}

	merged := make([]ServiceRecord, 0, len(live))
	for _, service := range live {
		record := ServiceRecord{
			Name:        service.Name,
			LoadState:   service.LoadState,
			ActiveState: service.ActiveState,
			SubState:    service.SubState,
			Description: service.Description,
		}
		if trackedRecord, ok := lookup[service.Name]; ok {
			record.LogPath = trackedRecord.LogPath
			record.Critical = trackedRecord.Critical
			record.LastAction = trackedRecord.LastAction
		}
		merged = append(merged, record)
	}
	return merged
}

func upsertTrackedService(services []ServiceRecord, serviceName, action string, info proto.ServiceInfo) []ServiceRecord {
	for i := range services {
		if services[i].Name == serviceName {
			services[i].LastAction = action
			services[i].LoadState = info.LoadState
			services[i].ActiveState = info.ActiveState
			services[i].SubState = info.SubState
			services[i].Description = info.Description
			return services
		}
	}
	return append(services, ServiceRecord{
		Name:        serviceName,
		LastAction:  action,
		LoadState:   info.LoadState,
		ActiveState: info.ActiveState,
		SubState:    info.SubState,
		Description: info.Description,
	})
}
