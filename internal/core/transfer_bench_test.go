// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package core

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// BenchmarkUploadThroughput moves a real file to an in-process agent over the
// full stack: SSH channels, the RPC codec, chunk checksums and the temp-file
// assembly. There is no network latency here, so it measures the CPU and
// allocation cost of the transfer engine itself — which is exactly what the
// binary frame encoding, the reused chunk buffers and the pooled connections
// were meant to reduce.
func BenchmarkUploadThroughput(b *testing.B) {
	app, _, _ := pooledTestFleet(b)
	benchUpload(b, app, 16<<20)
}

// BenchmarkDownloadThroughput is the download counterpart.
func BenchmarkDownloadThroughput(b *testing.B) {
	app, _, _ := pooledTestFleet(b)
	benchDownload(b, app, 16<<20)
}

// BenchmarkSmallFileUpload/Download measure per-file overhead, which dominates
// for configuration files and source trees.
func BenchmarkSmallFileUpload(b *testing.B) {
	app, _, _ := pooledTestFleet(b)
	benchUpload(b, app, 4<<10)
}

func BenchmarkSmallFileDownload(b *testing.B) {
	app, _, _ := pooledTestFleet(b)
	benchDownload(b, app, 4<<10)
}

// BenchmarkUploadDirSmallFiles uploads a tree of small files. With 40 files the
// previous engine failed outright (one SSH connection per file exceeded the
// agent's per-key limit); 6 files is the largest tree it could move.
func BenchmarkUploadDirSmallFiles(b *testing.B) {
	for _, files := range []int{6, 40} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			app, _, _ := pooledTestFleet(b)
			benchUploadDir(b, app, files)
		})
	}
}

// The RTT variants run the same transfers over a link that delays every write
// by half the round-trip time in each direction, where round trips — not CPU —
// decide the cost.
func BenchmarkUploadRTT(b *testing.B) {
	for _, tc := range []struct {
		rtt  time.Duration
		size int
	}{{40 * time.Millisecond, 4 << 10}, {100 * time.Millisecond, 16 << 20}} {
		b.Run(fmt.Sprintf("rtt=%v/size=%d", tc.rtt, tc.size), func(b *testing.B) {
			benchUpload(b, latencyTestFleet(b, tc.rtt/2), tc.size)
		})
	}
}

func BenchmarkDownloadRTT(b *testing.B) {
	for _, tc := range []struct {
		rtt  time.Duration
		size int
	}{{40 * time.Millisecond, 4 << 10}, {100 * time.Millisecond, 16 << 20}} {
		b.Run(fmt.Sprintf("rtt=%v/size=%d", tc.rtt, tc.size), func(b *testing.B) {
			benchDownload(b, latencyTestFleet(b, tc.rtt/2), tc.size)
		})
	}
}

func BenchmarkUploadDirRTT(b *testing.B) {
	for _, files := range []int{6, 40} {
		b.Run(fmt.Sprintf("rtt=40ms/files=%d", files), func(b *testing.B) {
			benchUploadDir(b, latencyTestFleet(b, 20*time.Millisecond), files)
		})
	}
}

func benchPayload(b *testing.B, size int) string {
	b.Helper()
	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(b.TempDir(), "payload.bin")
	if err := os.WriteFile(src, payload, 0o600); err != nil {
		b.Fatal(err)
	}
	return src
}

func benchUpload(b *testing.B, app *App, size int) {
	src := benchPayload(b, size)
	dstDir := b.TempDir()
	// Warm the pooled connection so every variant measures steady state.
	if _, err := app.ListRemoteDir("loopback", dstDir); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst := filepath.Join(dstDir, "out.bin")
		if _, err := app.UploadFile("loopback", src, dst, FileTransferOptions{}, nil); err != nil {
			b.Fatalf("UploadFile() error = %v", err)
		}
		// Each iteration must actually move bytes, not resume a finished temp.
		if err := os.Remove(dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchDownload(b *testing.B, app *App, size int) {
	src := benchPayload(b, size)
	dstDir := b.TempDir()
	if _, err := app.ListRemoteDir("loopback", dstDir); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst := filepath.Join(dstDir, "out.bin")
		if _, err := app.DownloadFile("loopback", src, dst, FileTransferOptions{}, nil); err != nil {
			b.Fatalf("DownloadFile() error = %v", err)
		}
		if err := os.Remove(dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchUploadDir(b *testing.B, app *App, files int) {
	src := b.TempDir()
	for i := range files {
		p := filepath.Join(src, fmt.Sprintf("d%02d", i%10), fmt.Sprintf("f%02d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 2048), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	dstDir := b.TempDir()
	if _, err := app.ListRemoteDir("loopback", dstDir); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		i++
		if _, err := app.UploadDir("loopback", src, filepath.Join(dstDir, fmt.Sprintf("up%d", i)), FileTransferOptions{}, nil); err != nil {
			b.Fatalf("UploadDir() error = %v", err)
		}
	}
}

// latencyTestFleet is pooledTestFleet behind a link that delivers every write
// oneWay later.
func latencyTestFleet(b *testing.B, oneWay time.Duration) *App {
	app, _, _ := pooledTestFleet(b)
	dial := app.NetworkDialContext
	app.NetworkDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newDelayedConn(c, oneWay), nil
	}
	return app
}

type delayedPacket struct {
	data []byte
	at   time.Time
}

// delayedConn delays delivery of everything written in either direction by a
// fixed latency while preserving order and bandwidth.
type delayedConn struct {
	net.Conn
	delay  time.Duration
	out    chan delayedPacket
	in     chan delayedPacket
	rbuf   []byte
	done   chan struct{}
	closed sync.Once
}

func newDelayedConn(c net.Conn, delay time.Duration) *delayedConn {
	d := &delayedConn{Conn: c, delay: delay, out: make(chan delayedPacket, 1<<16), in: make(chan delayedPacket, 1<<16), done: make(chan struct{})}
	go d.pumpOut()
	go d.pumpIn()
	return d
}

func (d *delayedConn) pumpOut() {
	for {
		select {
		case p := <-d.out:
			time.Sleep(time.Until(p.at))
			if _, err := d.Conn.Write(p.data); err != nil {
				return
			}
		case <-d.done:
			return
		}
	}
}

func (d *delayedConn) pumpIn() {
	defer close(d.in)
	for {
		buf := make([]byte, 64<<10)
		n, err := d.Conn.Read(buf)
		if n > 0 {
			select {
			case d.in <- delayedPacket{data: buf[:n], at: time.Now().Add(d.delay)}:
			case <-d.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (d *delayedConn) Write(p []byte) (int, error) {
	select {
	case d.out <- delayedPacket{data: append([]byte(nil), p...), at: time.Now().Add(d.delay)}:
		return len(p), nil
	case <-d.done:
		return 0, net.ErrClosed
	}
}

func (d *delayedConn) Read(p []byte) (int, error) {
	for len(d.rbuf) == 0 {
		select {
		case pkt, ok := <-d.in:
			if !ok {
				return 0, net.ErrClosed
			}
			time.Sleep(time.Until(pkt.at))
			d.rbuf = pkt.data
		case <-d.done:
			return 0, net.ErrClosed
		}
	}
	n := copy(p, d.rbuf)
	d.rbuf = d.rbuf[n:]
	return n, nil
}

func (d *delayedConn) Close() error {
	d.closed.Do(func() { close(d.done) })
	return d.Conn.Close()
}
