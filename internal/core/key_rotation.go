// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	fleetcrypto "github.com/cenvero/fleet/internal/crypto"
	"github.com/cenvero/fleet/internal/logs"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

type KeyRotationResult struct {
	RotationDir     string   `json:"rotation_dir"`
	Algorithm       string   `json:"algorithm"`
	PrimaryKey      string   `json:"primary_key"`
	RotatedServers  []string `json:"rotated_servers,omitempty"`
	VerifiedServers []string `json:"verified_servers,omitempty"`
	SkippedServers  []string `json:"skipped_servers,omitempty"`
	ArchivedFiles   []string `json:"archived_files,omitempty"`
	VerificationKey string   `json:"verification_key"`
}

func (a *App) RotateKeys() (KeyRotationResult, error) {
	servers, err := a.ListServers()
	if err != nil {
		return KeyRotationResult{}, err
	}
	if unsupported := unsupportedRotationServers(servers); len(unsupported) > 0 {
		return KeyRotationResult{}, fmt.Errorf("key rotation supports only direct or reverse fleets right now; unsupported servers: %s", strings.Join(unsupported, ", "))
	}

	rotationDir := filepath.Join(a.ConfigDir, "keys", "rotations", time.Now().UTC().Format("2006-01-02-150405"))
	if err := os.MkdirAll(rotationDir, 0o700); err != nil {
		return KeyRotationResult{}, err
	}

	archivedFiles, err := archiveKeyFiles(filepath.Join(a.ConfigDir, "keys"), rotationDir)
	if err != nil {
		return KeyRotationResult{}, err
	}

	// os.MkdirTemp appends a random suffix, preventing an attacker who has
	// access to the tmp directory from pre-staging a path before the rotation.
	tempDir, err := os.MkdirTemp(filepath.Join(a.ConfigDir, "tmp"), "key-rotation-*")
	if err != nil {
		return KeyRotationResult{}, fmt.Errorf("create key rotation temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	if err := fleetcrypto.GenerateKeySet(tempDir, fleetcrypto.Algorithm(a.Config.Crypto.Algorithm), nil); err != nil {
		return KeyRotationResult{}, err
	}

	oldPrimaryPubPath := filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey+".pub")
	newPrimaryPrivPath := filepath.Join(tempDir, a.Config.Crypto.PrimaryKey)
	newPrimaryPubPath := newPrimaryPrivPath + ".pub"

	oldKey, err := readPublicKeyLine(oldPrimaryPubPath)
	if err != nil {
		return KeyRotationResult{}, err
	}
	newKey, err := readPublicKeyLine(newPrimaryPubPath)
	if err != nil {
		return KeyRotationResult{}, err
	}

	directTargets, reverseTargets, skipped := rotationTargets(a, servers)
	stagedDirect := make([]ServerRecord, 0, len(directTargets))
	stagedReverse := make([]ServerRecord, 0, len(reverseTargets))
	cleanedReverse := make([]ServerRecord, 0, len(reverseTargets))
	cleanedDirect := make([]ServerRecord, 0, len(directTargets))
	verifiedNames := make([]string, 0, len(directTargets)+len(reverseTargets))

	rollbackBeforePromote := func(stepErr error) error {
		for _, server := range stagedReverse {
			_ = a.updateControllerHostKeys(server, nil, []string{newKey})
		}
		rollbackKeyPath := a.serverPrivateKeyPath(ServerRecord{})
		for _, server := range stagedDirect {
			_ = a.updateAuthorizedKeys(server, rollbackKeyPath, nil, []string{newKey})
		}
		return stepErr
	}

	rollbackAfterPromote := func(stepErr error) error {
		currentKeyPath := filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
		for _, server := range cleanedDirect {
			_ = a.updateAuthorizedKeys(server, currentKeyPath, []string{oldKey}, nil)
		}
		for _, server := range cleanedReverse {
			_ = a.updateControllerHostKeys(server, []string{oldKey}, nil)
		}
		_ = restoreActiveKeyFiles(rotationDir, filepath.Join(a.ConfigDir, "keys"))
		rollbackKeyPath := a.serverPrivateKeyPath(ServerRecord{})
		for _, server := range stagedDirect {
			_ = a.updateAuthorizedKeys(server, rollbackKeyPath, nil, []string{newKey})
		}
		for _, server := range stagedReverse {
			_ = a.updateControllerHostKeys(server, nil, []string{newKey})
		}
		return stepErr
	}

	for _, server := range directTargets {
		if err := a.updateAuthorizedKeys(server, a.serverPrivateKeyPath(server), []string{newKey}, nil); err != nil {
			return KeyRotationResult{}, rollbackBeforePromote(err)
		}
		stagedDirect = append(stagedDirect, server)
	}

	for _, server := range reverseTargets {
		if err := a.updateControllerHostKeys(server, []string{newKey}, nil); err != nil {
			return KeyRotationResult{}, rollbackBeforePromote(err)
		}
		stagedReverse = append(stagedReverse, server)
	}

	for _, server := range directTargets {
		if err := a.verifyAuthorizedKey(server, newPrimaryPrivPath); err != nil {
			return KeyRotationResult{}, rollbackBeforePromote(err)
		}
		verifiedNames = append(verifiedNames, server.Name)
	}

	if err := promoteGeneratedKeys(tempDir, filepath.Join(a.ConfigDir, "keys")); err != nil {
		return KeyRotationResult{}, rollbackBeforePromote(err)
	}

	for _, server := range reverseTargets {
		if err := a.verifyReverseReconnect(server); err != nil {
			return KeyRotationResult{}, rollbackAfterPromote(err)
		}
		verifiedNames = append(verifiedNames, server.Name)
	}

	for _, server := range reverseTargets {
		if err := a.updateControllerHostKeys(server, nil, []string{oldKey}); err != nil {
			return KeyRotationResult{}, rollbackAfterPromote(err)
		}
		cleanedReverse = append(cleanedReverse, server)
	}

	for _, server := range directTargets {
		if err := a.updateAuthorizedKeys(server, newPrimaryPrivPath, nil, []string{oldKey}); err != nil {
			return KeyRotationResult{}, rollbackAfterPromote(err)
		}
		cleanedDirect = append(cleanedDirect, server)
	}

	rotatedNames := make([]string, 0, len(directTargets)+len(reverseTargets))
	for _, server := range directTargets {
		rotatedNames = append(rotatedNames, server.Name)
	}
	for _, server := range reverseTargets {
		rotatedNames = append(rotatedNames, server.Name)
	}
	slices.Sort(rotatedNames)
	slices.Sort(verifiedNames)
	slices.Sort(skipped)

	if err := a.AuditLog.Append(logs.AuditEntry{
		Action:   "key.rotate",
		Target:   a.Config.Crypto.PrimaryKey,
		Operator: a.operator(),
		Details:  fmt.Sprintf("rotation_dir=%s direct=%d reverse=%d skipped=%d", rotationDir, len(directTargets), len(reverseTargets), len(skipped)),
	}); err != nil {
		return KeyRotationResult{}, err
	}

	return KeyRotationResult{
		RotationDir:     rotationDir,
		Algorithm:       a.Config.Crypto.Algorithm,
		PrimaryKey:      a.Config.Crypto.PrimaryKey,
		RotatedServers:  rotatedNames,
		VerifiedServers: verifiedNames,
		SkippedServers:  skipped,
		ArchivedFiles:   archivedFiles,
		VerificationKey: filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey+".pub"),
	}, nil
}

func unsupportedRotationServers(servers []ServerRecord) []string {
	var unsupported []string
	for _, server := range servers {
		switch server.Mode {
		case "", transport.ModeDirect, transport.ModeReverse, transport.ModePerNode:
			// per-server mode is resolved to direct or reverse in rotationTargets
		default:
			unsupported = append(unsupported, server.Name)
		}
	}
	slices.Sort(unsupported)
	return unsupported
}

func rotationTargets(a *App, servers []ServerRecord) ([]ServerRecord, []ServerRecord, []string) {
	defaultKey := filepath.Join(a.ConfigDir, "keys", a.Config.Crypto.PrimaryKey)
	directTargets := make([]ServerRecord, 0, len(servers))
	reverseTargets := make([]ServerRecord, 0, len(servers))
	var skipped []string
	for _, server := range servers {
		mode := server.Mode
		if mode == "" || mode == transport.ModePerNode {
			mode = a.Config.DefaultMode
			if mode == "" || mode == transport.ModePerNode {
				mode = transport.ModeDirect
			}
		}
		switch mode {
		case transport.ModeDirect:
			if server.KeyPath != "" && server.KeyPath != defaultKey {
				skipped = append(skipped, server.Name)
				continue
			}
			directTargets = append(directTargets, server)
		case transport.ModeReverse:
			reverseTargets = append(reverseTargets, server)
		}
	}
	return directTargets, reverseTargets, skipped
}

func archiveKeyFiles(sourceDir, rotationDir string) ([]string, error) {
	var archived []string
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "id_rsa4096", "id_rsa4096.pub"} {
		source := filepath.Join(sourceDir, name)
		info, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(rotationDir, name)
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return nil, err
		}
		archived = append(archived, target)
	}
	slices.Sort(archived)
	return archived, nil
}

func promoteGeneratedKeys(tempDir, targetDir string) error {
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "id_rsa4096", "id_rsa4096.pub"} {
		source := filepath.Join(tempDir, name)
		info, err := os.Stat(source)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(targetDir, name), data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func restoreActiveKeyFiles(rotationDir, targetDir string) error {
	return filepath.WalkDir(rotationDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(targetDir, name), data, info.Mode().Perm())
	})
}

func readPublicKeyLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (a *App) updateAuthorizedKeys(server ServerRecord, privateKeyPath string, addKeys, removeKeys []string) error {
	response, err := a.callDirectRPCWithKey(server, privateKeyPath, proto.Envelope{
		Action: "auth.update_keys",
		Payload: proto.AuthorizedKeysPayload{
			AddKeys:    addKeys,
			RemoveKeys: removeKeys,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	_, err = proto.DecodePayload[proto.AuthorizedKeysResult](response.Payload)
	return err
}

func (a *App) verifyAuthorizedKey(server ServerRecord, privateKeyPath string) error {
	session, _, err := a.openDirectSessionWithKey(server, privateKeyPath, false)
	if err != nil {
		return err
	}
	return session.Close()
}

func (a *App) callDirectRPCWithKey(server ServerRecord, privateKeyPath string, env proto.Envelope) (proto.Envelope, error) {
	session, _, err := a.openDirectSessionWithKey(server, privateKeyPath, false)
	if err != nil {
		return proto.Envelope{}, err
	}
	defer session.Close()
	return session.Call(context.Background(), env)
}

func (a *App) updateControllerHostKeys(server ServerRecord, addKeys, removeKeys []string) error {
	response, err := a.callRPC(server, proto.Envelope{
		Action: "auth.update_controller_host_keys",
		Payload: proto.ControllerKnownHostsPayload{
			AddKeys:    addKeys,
			RemoveKeys: removeKeys,
		},
	})
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("%s: %s", response.Error.Code, response.Error.Message)
	}
	_, err = proto.DecodePayload[proto.ControllerKnownHostsResult](response.Payload)
	return err
}

func (a *App) verifyReverseReconnect(server ServerRecord) error {
	status, err := a.reverseStatus(server.Name)
	if err != nil {
		return err
	}
	if err := a.reverseDisconnect(server.Name); err != nil {
		return err
	}

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		current, err := a.reverseStatus(server.Name)
		if err == nil && current.Connected && current.ConnectedAt.After(status.ConnectedAt) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("reverse session for %q did not reconnect after controller host key rotation", server.Name)
}
