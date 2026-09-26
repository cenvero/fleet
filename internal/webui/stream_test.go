// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenvero/fleet/internal/core"
	"github.com/cenvero/fleet/internal/transport"
	"github.com/cenvero/fleet/pkg/proto"
)

// fakeRemote is a scriptable remote file: it can stream slowly, fail midway,
// grow, change between stats, or have its parallel copy disagree with its
// sequential stream.
type fakeRemote struct {
	mu        sync.Mutex
	data      []byte
	modTime   time.Time
	statCalls int
	// changeAfterStats, when > 0, makes every stat after that many calls
	// report a newer mtime (the file was modified mid-download).
	changeAfterStats int
	statErr          error
	chunk            int
	catDelay         time.Duration
	catFailAfter     int64 // fail once this many bytes were written (0 = never)
	catExtra         []byte
	catWriteErr      error // first error returned by the response writer
	catWritten       int64 // bytes the sequential reader got accepted
	catDone          chan struct{}
	dlData           []byte
	dlDelay          time.Duration
	dlErr            error
	dlCalls          int
}

func newFakeRemote(data []byte) *fakeRemote {
	return &fakeRemote{data: data, modTime: time.Unix(1_700_000_000, 0).UTC(), chunk: 64 << 10, catDone: make(chan struct{}, 8)}
}

func (f *fakeRemote) entry(changed bool) proto.FileEntry {
	mt := f.modTime
	if changed {
		mt = mt.Add(time.Minute)
	}
	return proto.FileEntry{Name: "payload.bin", Size: int64(len(f.data)), Type: proto.FileEntryTypeRegular, ModTime: mt}
}

func (f *fakeRemote) StatRemoteFile(_, _ string) (proto.FileStatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statCalls++
	if f.statErr != nil {
		return proto.FileStatResult{}, f.statErr
	}
	changed := f.changeAfterStats > 0 && f.statCalls > f.changeAfterStats
	return proto.FileStatResult{Entry: f.entry(changed)}, nil
}

func (f *fakeRemote) CatRemoteFile(_, _ string, w io.Writer) (int64, error) {
	defer func() { f.catDone <- struct{}{} }()
	var n int64
	for off := 0; off < len(f.data); off += f.chunk {
		if f.catDelay > 0 {
			time.Sleep(f.catDelay)
		}
		if f.catFailAfter > 0 && n >= f.catFailAfter {
			return n, errors.New("checksum mismatch at offset " + strconv.FormatInt(n, 10))
		}
		end := min(off+f.chunk, len(f.data))
		if _, err := w.Write(f.data[off:end]); err != nil {
			f.mu.Lock()
			if f.catWriteErr == nil {
				f.catWriteErr = err
			}
			f.mu.Unlock()
			return n, err
		}
		n += int64(end - off)
		f.mu.Lock()
		f.catWritten = n
		f.mu.Unlock()
	}
	if len(f.catExtra) > 0 {
		if _, err := w.Write(f.catExtra); err != nil {
			return n, err
		}
		n += int64(len(f.catExtra))
	}
	return n, nil
}

func (f *fakeRemote) DownloadFile(_, _, localPath string, _ core.FileTransferOptions, _ core.ProgressFunc) (proto.FileStatResult, error) {
	f.mu.Lock()
	f.dlCalls++
	f.mu.Unlock()
	if f.dlDelay > 0 {
		time.Sleep(f.dlDelay)
	}
	if f.dlErr != nil {
		return proto.FileStatResult{}, f.dlErr
	}
	data := f.dlData
	if data == nil {
		data = f.data
	}
	if err := os.WriteFile(localPath, data, 0o600); err != nil {
		return proto.FileStatResult{}, err
	}
	return proto.FileStatResult{Entry: f.entry(false)}, nil
}

func (f *fakeRemote) writeErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catWriteErr
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newStreamTestServer returns a server whose remote reads go to fake, with a
// managed POSIX node named "node" so path-style lookups succeed.
func newStreamTestServer(t *testing.T, fake *fakeRemote) (*Server, string) {
	t.Helper()
	s, ts := newTestServer(t)
	if err := s.app.SaveServer(core.ServerRecord{Name: "node", Mode: transport.ModeDirect, Observed: core.ServerObservation{Reachable: true, OS: "linux"}}); err != nil {
		t.Fatal(err)
	}
	s.files = fake
	return s, ts.URL
}

func downloadURL(base string, s *Server, extra url.Values) string {
	q := url.Values{"t": {s.Token()}, "server": {"node"}, "path": {"/srv/payload.bin"}}
	for k, v := range extra {
		q[k] = v
	}
	return base + "/api/download?" + q.Encode()
}

func setThreshold(t *testing.T, v int64) {
	t.Helper()
	old := hybridStreamThreshold
	hybridStreamThreshold = v
	t.Cleanup(func() { hybridStreamThreshold = old })
}

func TestStreamDownloadSequential(t *testing.T) {
	data := randomBytes(t, 300<<10)
	fake := newFakeRemote(data)
	s, base := newStreamTestServer(t, fake)

	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Fatalf("status %d, %d bytes (want %d), equal=%v", res.StatusCode, len(body), len(data), bytes.Equal(body, data))
	}
	if got := res.Header.Get("Content-Length"); got != strconv.Itoa(len(data)) {
		t.Fatalf("Content-Length = %q", got)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "payload.bin") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if fake.dlCalls != 0 {
		t.Fatalf("small file must not start the parallel engine (calls=%d)", fake.dlCalls)
	}
	entries, err := s.app.AuditEntries()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "file.download" && strings.Contains(e.Details, "web UI browser download") {
			found = true
		}
	}
	if !found {
		t.Fatalf("streamed download was not audited")
	}
}

// The first byte must arrive long before the whole (slow) file has been read.
func TestStreamDownloadFirstByteBeforeCompletion(t *testing.T) {
	data := randomBytes(t, 20*(16<<10))
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 25 * time.Millisecond // whole file ≈ 500ms
	s, base := newStreamTestServer(t, fake)

	start := time.Now()
	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	one := make([]byte, 1)
	if _, err := io.ReadFull(res.Body, one); err != nil {
		t.Fatal(err)
	}
	// Deterministic check (no wall-clock race): when the browser gets its
	// first byte, the remote reader must still be far from the end.
	fake.mu.Lock()
	readSoFar := fake.catWritten
	fake.mu.Unlock()
	first := time.Since(start)
	rest, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(one, rest...), data) {
		t.Fatalf("content mismatch")
	}
	if readSoFar >= int64(len(data))/2 {
		t.Fatalf("first byte arrived only after %d of %d bytes were read (%v): response was not streamed", readSoFar, len(data), first)
	}
}

func TestStreamDownloadSwitchesToParallelCopy(t *testing.T) {
	setThreshold(t, 1024)
	data := randomBytes(t, 64*(16<<10))
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 20 * time.Millisecond // sequential ≈ 1.3s
	fake.dlDelay = 150 * time.Millisecond // parallel finishes first
	s, base := newStreamTestServer(t, fake)

	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(body, data) {
		t.Fatalf("content mismatch after switch (%d bytes)", len(body))
	}
	fake.mu.Lock()
	fromSequential, calls := fake.catWritten, fake.dlCalls
	fake.mu.Unlock()
	if calls != 1 {
		t.Fatalf("parallel engine calls = %d", calls)
	}
	// The tail must have come from the parallel copy, not the slow reader.
	if fromSequential >= int64(len(data)) {
		t.Fatalf("sequential reader delivered all %d bytes; the stream never switched", fromSequential)
	}
}

func TestStreamDownloadFallsBackWhenSequentialFails(t *testing.T) {
	setThreshold(t, 1024)
	data := randomBytes(t, 32*(16<<10))
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 5 * time.Millisecond
	fake.catFailAfter = 4 * (16 << 10)
	fake.dlDelay = 200 * time.Millisecond
	s, base := newStreamTestServer(t, fake)

	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || !bytes.Equal(body, data) {
		t.Fatalf("fallback download: err=%v equal=%v", err, bytes.Equal(body, data))
	}
}

// expectAborted reads a response that must fail: fewer bytes than the
// declared Content-Length and an unexpected EOF.
func expectAborted(t *testing.T, res *http.Response, want []byte) {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err == nil {
		t.Fatalf("expected a failed (aborted) body read, got %d bytes and no error", len(body))
	}
	if len(body) >= len(want) {
		t.Fatalf("aborted response still delivered %d/%d bytes", len(body), len(want))
	}
	if !bytes.Equal(body, want[:len(body)]) {
		t.Fatalf("delivered prefix is not the file's prefix")
	}
}

func TestStreamDownloadAbortsWhenFileChangesDuringRead(t *testing.T) {
	data := randomBytes(t, 200<<10)
	fake := newFakeRemote(data)
	fake.changeAfterStats = 1 // the verification re-stat sees a new mtime
	s, base := newStreamTestServer(t, fake)
	track := strings.Repeat("ab", 16)

	res, err := http.Get(downloadURL(base, s, url.Values{"track": {track}}))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("status %d CL %q", res.StatusCode, res.Header.Get("Content-Length"))
	}
	expectAborted(t, res, data)
	snap := waitDone(t, s, track)
	if !strings.Contains(snap.Error, "changed") {
		t.Fatalf("hub error = %q", snap.Error)
	}
}

func TestStreamDownloadAbortsWhenParallelCopyDisagrees(t *testing.T) {
	setThreshold(t, 1024)
	data := randomBytes(t, 64*(16<<10))
	corrupt := append([]byte(nil), data...)
	corrupt[100] ^= 0xff // differs inside the prefix already streamed
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 20 * time.Millisecond
	fake.dlDelay = 150 * time.Millisecond
	fake.dlData = corrupt
	s, base := newStreamTestServer(t, fake)

	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	expectAborted(t, res, data)
}

func TestStreamDownloadAbortsWhenFileGrows(t *testing.T) {
	data := randomBytes(t, 100<<10)
	fake := newFakeRemote(data)
	fake.catExtra = []byte("appended while reading")
	s, base := newStreamTestServer(t, fake)

	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	expectAborted(t, res, data)
}

func TestStreamDownloadAbortsOnMidStreamReadError(t *testing.T) {
	data := randomBytes(t, 256<<10)
	fake := newFakeRemote(data)
	fake.catFailAfter = 128 << 10
	s, base := newStreamTestServer(t, fake)
	track := strings.Repeat("cd", 16)

	res, err := http.Get(downloadURL(base, s, url.Values{"track": {track}}))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	expectAborted(t, res, data)
	if snap := waitDone(t, s, track); !strings.Contains(snap.Error, "checksum mismatch") {
		t.Fatalf("hub error = %q", snap.Error)
	}
}

func TestStreamDownloadStopsWhenClientDisconnects(t *testing.T) {
	data := randomBytes(t, 400*(16<<10))
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 10 * time.Millisecond // ≈ 4s if read to the end
	s, base := newStreamTestServer(t, fake)
	track := strings.Repeat("ef", 16)

	res, err := http.Get(downloadURL(base, s, url.Values{"track": {track}}))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32<<10)
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close() // the browser cancels the download
	select {
	case <-fake.catDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("sequential reader kept running after the client disconnected")
	}
	if fake.writeErr() == nil {
		t.Fatalf("reader was not stopped by a write error")
	}
	if snap := waitDone(t, s, track); !strings.Contains(snap.Error, "cancelled") {
		t.Fatalf("hub error = %q", snap.Error)
	}
}

func TestStreamDownloadCancelFromTransfersPanel(t *testing.T) {
	data := randomBytes(t, 400*(16<<10))
	fake := newFakeRemote(data)
	fake.chunk = 16 << 10
	fake.catDelay = 10 * time.Millisecond
	s, base := newStreamTestServer(t, fake)
	track := strings.Repeat("0f", 16)

	res, err := http.Get(downloadURL(base, s, url.Values{"track": {track}}))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	buf := make([]byte, 16<<10)
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatal(err)
	}
	snap, ok := s.hub.snapshot(track)
	if !ok || !snap.Cancellable {
		t.Fatalf("tracked download should be cancellable: %+v", snap)
	}
	code, body := postAPI(t, s, base, "/api/transfers/cancel", url.Values{"id": {track}}, base)
	if code != http.StatusOK {
		t.Fatalf("cancel status %d: %s", code, body)
	}
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Fatalf("cancelled download completed")
	}
	if snap := waitDone(t, s, track); !snap.Cancelled || !strings.Contains(snap.Error, "Transfers panel") {
		t.Fatalf("hub after cancel = %+v", snap)
	}
	// A finished transfer cannot be cancelled again.
	if code, _ := postAPI(t, s, base, "/api/transfers/cancel", url.Values{"id": {track}}, base); code != http.StatusConflict {
		t.Fatalf("second cancel status %d, want 409", code)
	}
}

func TestStreamDownloadErrorsBeforeFirstByteAreJSON(t *testing.T) {
	fake := newFakeRemote([]byte("x"))
	fake.statErr = errors.New("invalid_path: path is outside the agent's allowed file roots")
	s, base := newStreamTestServer(t, fake)
	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadGateway || !strings.Contains(out.Error, "allowed file roots") {
		t.Fatalf("status %d error %q", res.StatusCode, out.Error)
	}
}

func TestStreamDownloadRejectsDirectoryAndBadTrackID(t *testing.T) {
	fake := newFakeRemote(nil)
	s, base := newStreamTestServer(t, fake)
	s.files = dirRemote{fake}
	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("directory download status %d", res.StatusCode)
	}
	for _, bad := range []string{"NOTHEX", "abc", strings.Repeat("a", 65), "../../etc"} {
		res, err := http.Get(downloadURL(base, s, url.Values{"track": {bad}}))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("track %q accepted (status %d)", bad, res.StatusCode)
		}
	}
	// Reusing a live/known id is refused so one request cannot hijack
	// another's progress record.
	id := strings.Repeat("12", 16)
	if err := s.hub.startWithID(id, transferMeta{Kind: "download"}); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(downloadURL(base, s, url.Values{"track": {id}}))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate track id accepted (status %d)", res.StatusCode)
	}
}

type dirRemote struct{ *fakeRemote }

func (d dirRemote) StatRemoteFile(_, _ string) (proto.FileStatResult, error) {
	return proto.FileStatResult{Entry: proto.FileEntry{Name: "d", IsDir: true, Type: proto.FileEntryTypeDirectory}}, nil
}

func TestStreamDownloadEmptyFile(t *testing.T) {
	fake := newFakeRemote([]byte{})
	s, base := newStreamTestServer(t, fake)
	res, err := http.Get(downloadURL(base, s, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || len(body) != 0 || res.StatusCode != http.StatusOK || res.Header.Get("Content-Length") != "0" {
		t.Fatalf("empty download: status %d CL %q len %d err %v", res.StatusCode, res.Header.Get("Content-Length"), len(body), err)
	}
}

func TestLocalDownloadStreamsWithRFC6266Name(t *testing.T) {
	s, ts := newTestServer(t)
	dir := t.TempDir()
	name := "résumé \"final\".txt"
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	track := strings.Repeat("9a", 16)
	res, err := http.Get(ts.URL + "/api/download?" + url.Values{"t": {s.Token()}, "path": {file}, "track": {track}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("status %d body %q", res.StatusCode, body)
	}
	cd := res.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "filename*=utf-8''r%C3%A9sum%C3%A9") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if snap := waitDone(t, s, track); snap.Error != "" || snap.BytesDone != 5 || snap.Kind != "download" {
		t.Fatalf("hub = %+v", snap)
	}
}

func TestDownloadRejectsNonGET(t *testing.T) {
	s, ts := newTestServer(t)
	code, _ := postAPI(t, s, ts.URL, "/api/download", url.Values{"path": {"/etc/hostname"}}, ts.URL)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/download status %d, want 405", code)
	}
}

// waitDone polls the hub until the transfer is finished.
func waitDone(t *testing.T, s *Server, id string) progressSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := s.hub.snapshot(id); ok && snap.Done {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := s.hub.snapshot(id)
	t.Fatalf("transfer %s did not finish: %+v", id, snap)
	return progressSnapshot{}
}
