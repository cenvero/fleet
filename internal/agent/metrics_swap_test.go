// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestCollectReportsSwap: the snapshot carries swap so `fleet top` no longer
// has to exec `free` on every server every frame.
func TestCollectReportsSwap(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("swap source checked on linux only")
	}
	snap, err := defaultMetricsCollector().Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.SwapReported {
		t.Fatal("swap not reported")
	}
	if snap.SwapUsedBytes > snap.SwapTotalBytes {
		t.Fatalf("swap used %d > total %d", snap.SwapUsedBytes, snap.SwapTotalBytes)
	}
}

// TestSwapFieldsAreOptionalOnTheWire: old controllers must be able to ignore
// the new fields, and an old agent's snapshot must decode with them unset.
func TestSwapFieldsAreOptionalOnTheWire(t *testing.T) {
	t.Parallel()
	old, err := json.Marshal(proto.MetricsSnapshot{CPUPercent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(old), "swap") {
		t.Fatalf("zero swap fields are serialized: %s", old)
	}
	var decoded proto.MetricsSnapshot
	if err := json.Unmarshal([]byte(`{"timestamp":"2026-01-01T00:00:00Z","cpu_percent":1,"memory_percent":2,"disk_percent":3}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SwapReported || decoded.SwapTotalBytes != 0 {
		t.Fatalf("old-agent snapshot decoded with swap set: %+v", decoded)
	}
}
