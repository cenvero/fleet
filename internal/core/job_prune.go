// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/cenvero/fleet/internal/logs"
)

// jobLogPruneInterval is how often the daemon reaps expired job logs.
const jobLogPruneInterval = 1 * time.Hour

// PruneJobLogs deletes job logs older than the configured retention, on BOTH
// sides:
//   - the controller's tracked job records (jobs.json) and each one's remote
//     /var/tmp/fleet-job-*.log (via rm -f), and
//   - any ORPHANED fleet-job logs still sitting in /var/tmp on managed servers
//     (an mtime sweep, so logs from a re-installed controller are cleaned too).
//
// It is best-effort and idempotent: unreachable servers are skipped, and when
// retention is 0/"off"/"never" it does nothing. Returns how many controller
// records were removed.
func (a *App) PruneJobLogs(ctx context.Context) (int, error) {
	retention := a.Config.JobLogRetentionDuration()
	if retention <= 0 {
		return 0, nil // pruning disabled
	}
	cutoff := time.Now().Add(-retention).UTC()
	exec := func(server, command string) (string, int, error) {
		res, err := a.ExecCommandContext(ctx, server, command)
		if err != nil {
			return "", 0, err
		}
		return res.Stdout, res.ExitCode, nil
	}

	removed, pruneErr := NewJobStore(a.ConfigDir).Prune(cutoff, exec)

	// Orphan sweep: delete fleet-job logs older than the window that no record
	// references. `find -mtime +N` counts whole 24h periods; round the window UP
	// to days so we never sweep a file that is still inside the retention window.
	days := int((retention + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	sweep := fmt.Sprintf(
		"find /var/tmp -maxdepth 1 -name 'fleet-job-*.log' -type f -mtime +%d -delete 2>/dev/null || true", days)
	servers, _ := a.ListServers()
	if err := a.sweepOrphanJobLogs(ctx, servers, sweep); err != nil {
		return removed, err
	}

	if removed > 0 {
		_ = a.AuditLog.Append(logs.AuditEntry{
			Action:   "job.logs.pruned",
			Target:   "controller",
			Operator: a.operator(),
			Details:  fmt.Sprintf("removed %d job record(s) older than %s", removed, retention),
		})
	}
	return removed, pruneErr
}

// jobSweepCursor is where the next orphan sweep starts in the (sorted) server
// list. The sweep runs under a time budget, and it used to start at the first
// server every run and check the budget only between servers, so on a large or
// slow fleet the late servers were never swept. Each run now resumes where the
// previous one stopped; a new process starts at a random offset.
var jobSweepCursor = func() *atomic.Int64 {
	var c atomic.Int64
	c.Store(rand.Int63()) // #nosec G404 -- load spreading, not security
	return &c
}()

// sweepOrphanJobLogs runs the sweep command on every server, starting at the
// rotating cursor and stopping as soon as ctx (the pruner's budget) expires —
// including in the middle of a slow server's call. It returns ctx.Err() when it
// had to stop early; the next run continues from the first server not swept.
func (a *App) sweepOrphanJobLogs(ctx context.Context, servers []ServerRecord, sweep string) error {
	n := len(servers)
	if n == 0 {
		return nil
	}
	start := int(jobSweepCursor.Load() % int64(n))
	if start < 0 {
		start += n
	}
	done := 0
	defer func() { jobSweepCursor.Store(int64((start + done) % n)) }()
	for ; done < n; done++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		srv := servers[(start+done)%n]
		_, _ = a.ExecCommandContext(ctx, srv.Name, sweep) // best-effort; unreachable servers skipped
		if err := ctx.Err(); err != nil {
			return err // this server's sweep was cut short: retry it first next time
		}
	}
	return nil
}

// runJobLogPruner runs the job-log pruner once at startup and then on a ticker
// while the daemon is alive, so expired job logs are reaped on both the
// controller and the managed servers without any operator action. It exits
// immediately when retention is disabled.
func (a *App) runJobLogPruner(ctx context.Context) {
	if a.Config.JobLogRetentionDuration() <= 0 {
		return
	}
	prune := func() {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		_, _ = a.PruneJobLogs(cctx)
	}
	prune()
	ticker := time.NewTicker(jobLogPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
