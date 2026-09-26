// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// notifyQueueCapacity bounds how many notifications may wait for delivery.
	// When full, the OLDEST is dropped (with a log line): the newest state of
	// the fleet is the most useful thing to deliver.
	notifyQueueCapacity = 256
	// notifyFlushTimeout bounds how long App.Close waits for queued
	// notifications, so a short-lived CLI command still delivers what it fired
	// and a stopping daemon drains its queue, without hanging on a dead endpoint.
	notifyFlushTimeout = 10 * time.Second
)

// notifyLog receives the dispatcher's operational messages (drops, undelivered
// notifications at exit). Replaceable in tests.
var notifyLog io.Writer = os.Stderr

type notifyJob struct {
	event   string
	message string
	at      time.Time
}

// notifyDispatcher delivers notifications on a single background worker, in
// the order they were fired. The worker exists only while there is something to
// deliver, so an idle process holds no goroutine. Deciding WHETHER to notify
// (transition detection, cooldowns) stays with the caller; only the slow
// network delivery moves off the caller's path.
type notifyDispatcher struct {
	deliver func(notifyJob)

	mu      sync.Mutex
	queue   []notifyJob
	running bool
	idle    chan struct{} // closed when the current worker has drained the queue
}

func newNotifyDispatcher(deliver func(notifyJob)) *notifyDispatcher {
	return &notifyDispatcher{deliver: deliver}
}

func (d *notifyDispatcher) enqueue(job notifyJob) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) >= notifyQueueCapacity {
		oldest := d.queue[0]
		d.queue = d.queue[1:]
		fmt.Fprintf(notifyLog, "fleet: notification queue full (%d pending); dropped the oldest %q notification (fired %s)\n",
			notifyQueueCapacity, oldest.event, oldest.at.Format(time.RFC3339))
	}
	d.queue = append(d.queue, job)
	if !d.running {
		d.running = true
		d.idle = make(chan struct{})
		go d.run(d.idle)
	}
}

func (d *notifyDispatcher) run(idle chan struct{}) {
	defer close(idle)
	for {
		d.mu.Lock()
		if len(d.queue) == 0 {
			d.running = false
			d.queue = nil
			d.mu.Unlock()
			return
		}
		job := d.queue[0]
		d.queue = d.queue[1:]
		d.mu.Unlock()
		d.deliverSafely(job)
	}
}

func (d *notifyDispatcher) deliverSafely(job notifyJob) {
	defer func() {
		// A panic in best-effort delivery must never take the process down.
		_ = recover()
	}()
	d.deliver(job)
}

// flush waits until every queued notification has been delivered, or timeout
// elapses (then it reports how many were abandoned). It returns true when the
// queue drained.
func (d *notifyDispatcher) flush(timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		d.mu.Lock()
		running, idle := d.running, d.idle
		d.mu.Unlock()
		if !running {
			return true
		}
		select {
		case <-idle:
			// Loop: something may have been enqueued after the worker exited.
		case <-deadline.C:
			d.mu.Lock()
			left := len(d.queue)
			d.mu.Unlock()
			fmt.Fprintf(notifyLog, "fleet: gave up waiting for notification delivery after %s (%d still queued, 1 in flight)\n", timeout, left)
			return false
		}
	}
}

// notificationQueue returns the App's dispatcher, creating it on first use.
func (a *App) notificationQueue() *notifyDispatcher {
	a.notificationsMu.Lock()
	defer a.notificationsMu.Unlock()
	if a.notifications == nil {
		configDir := a.ConfigDir
		a.notifications = newNotifyDispatcher(func(job notifyJob) {
			// The store is loaded at delivery time so it reflects the latest
			// configured targets; delivery errors are best-effort, as before.
			_ = NewNotifyStore(configDir).fireAt(job.event, job.message, job.at)
		})
	}
	return a.notifications
}

// FlushNotifications waits (up to timeout) for queued notifications to be
// delivered. App.Close calls it; long-running callers may call it earlier.
func (a *App) FlushNotifications(timeout time.Duration) bool {
	if a == nil {
		return true
	}
	a.notificationsMu.Lock()
	d := a.notifications
	a.notificationsMu.Unlock()
	if d == nil {
		return true
	}
	return d.flush(timeout)
}
