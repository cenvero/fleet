// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/alerts"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/internal/update"
)

func TestRaiseAlertTracksOccurrencesAndCooldown(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()
	app.Config.Runtime.AlertNotifyCooldown = "1h"
	notifier := &fakeNotifier{}
	app.Notifier = notifier

	alert := alerts.Alert{
		ID:       "cpu-hot",
		Server:   "loopback",
		Code:     "metrics.cpu.critical",
		Severity: alerts.SeverityCritical,
		Message:  "CPU is hot",
	}
	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("first raiseAlert() error = %v", err)
	}
	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("second raiseAlert() error = %v", err)
	}
	if notifier.Count() != 1 {
		t.Fatalf("expected cooldown to suppress duplicate notifications, got %d", notifier.Count())
	}

	saved, err := app.Alerts.Get("cpu-hot")
	if err != nil {
		t.Fatalf("Get(alert) error = %v", err)
	}
	if saved.Occurrences != 2 {
		t.Fatalf("expected occurrences=2, got %d", saved.Occurrences)
	}
	past := time.Now().UTC().Add(-2 * time.Hour)
	saved.LastNotifiedAt = &past
	if err := app.Alerts.Save(saved); err != nil {
		t.Fatalf("Save(alert) error = %v", err)
	}

	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("third raiseAlert() error = %v", err)
	}
	if notifier.Count() != 2 {
		t.Fatalf("expected reminder notification after cooldown, got %d", notifier.Count())
	}
}

func TestSuppressAlertPreventsCriticalNotification(t *testing.T) {
	t.Parallel()

	configDir := filepath.Join(t.TempDir(), "fleet")
	if _, err := Initialize(InitOptions{
		ConfigDir:       configDir,
		Alias:           "fleet",
		DefaultMode:     transport.ModeDirect,
		CryptoAlgorithm: "ed25519",
		UpdateChannel:   "stable",
		UpdatePolicy:    update.PolicyNotifyOnly,
	}); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	app, err := Open(configDir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer app.Close()
	app.Config.Runtime.AlertNotifyCooldown = "1m"
	notifier := &fakeNotifier{}
	app.Notifier = notifier

	alert := alerts.Alert{
		ID:       "disk-full",
		Server:   "loopback",
		Code:     "metrics.disk.critical",
		Severity: alerts.SeverityCritical,
		Message:  "Disk is full",
	}
	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("first raiseAlert() error = %v", err)
	}
	if err := app.SuppressAlert(alert.ID, time.Hour); err != nil {
		t.Fatalf("SuppressAlert() error = %v", err)
	}

	saved, err := app.Alerts.Get(alert.ID)
	if err != nil {
		t.Fatalf("Get(alert) error = %v", err)
	}
	past := time.Now().UTC().Add(-2 * time.Hour)
	saved.LastNotifiedAt = &past
	if err := app.Alerts.Save(saved); err != nil {
		t.Fatalf("Save(alert) error = %v", err)
	}

	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("second raiseAlert() error = %v", err)
	}
	if notifier.Count() != 1 {
		t.Fatalf("expected suppression to prevent a repeat notification, got %d", notifier.Count())
	}

	if err := app.UnsuppressAlert(alert.ID); err != nil {
		t.Fatalf("UnsuppressAlert() error = %v", err)
	}
	saved, err = app.Alerts.Get(alert.ID)
	if err != nil {
		t.Fatalf("Get(alert after unsuppress) error = %v", err)
	}
	saved.LastNotifiedAt = &past
	if err := app.Alerts.Save(saved); err != nil {
		t.Fatalf("Save(alert after unsuppress) error = %v", err)
	}
	if err := app.raiseAlert(alert); err != nil {
		t.Fatalf("third raiseAlert() error = %v", err)
	}
	if notifier.Count() != 2 {
		t.Fatalf("expected unsuppressed alert to notify again, got %d", notifier.Count())
	}
}
