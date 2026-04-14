// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/process"
)

type MetricsCollector interface {
	Collect(context.Context) (proto.MetricsSnapshot, error)
}

type systemMetricsCollector struct{}

func defaultMetricsCollector() MetricsCollector {
	return systemMetricsCollector{}
}

func (systemMetricsCollector) Collect(ctx context.Context) (proto.MetricsSnapshot, error) {
	hostname, _ := os.Hostname()
	snapshot := proto.MetricsSnapshot{
		Timestamp: time.Now().UTC(),
		Hostname:  hostname,
	}

	if percents, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(percents) > 0 {
		snapshot.CPUPercent = percents[0]
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		snapshot.MemoryPercent = vm.UsedPercent
		snapshot.MemoryUsedBytes = vm.Used
		snapshot.MemoryTotalBytes = vm.Total
	}
	if usage, path, err := diskUsageWithPath(ctx); err == nil {
		snapshot.DiskPath = path
		snapshot.DiskPercent = usage.UsedPercent
		snapshot.DiskUsedBytes = usage.Used
		snapshot.DiskTotalBytes = usage.Total
	}
	if avg, err := load.AvgWithContext(ctx); err == nil {
		snapshot.Load1 = avg.Load1
		snapshot.Load5 = avg.Load5
		snapshot.Load15 = avg.Load15
	}
	if uptime, err := host.UptimeWithContext(ctx); err == nil {
		snapshot.UptimeSeconds = uptime
	}
	if processCount, err := process.PidsWithContext(ctx); err == nil {
		snapshot.ProcessCount = uint64(len(processCount))
	}
	return snapshot, nil
}

func diskUsageWithPath(ctx context.Context) (*disk.UsageStat, string, error) {
	path := "/"
	if runtime.GOOS == "windows" {
		path = os.Getenv("SystemDrive")
		if path == "" {
			path = "C:"
		}
		if len(path) == 2 {
			path += "\\"
		}
	}
	usage, err := disk.UsageWithContext(ctx, path)
	return usage, path, err
}
