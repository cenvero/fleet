// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/cenvero/fleet/pkg/proto"
)

// encodedStringLen is what encoding/json (as proto.Encode uses it) writes for s
// between the quotes.
func encodedStringLen(t *testing.T, s string) int {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(s); err != nil {
		t.Fatal(err)
	}
	return buf.Len() - 3 // two quotes and the trailing newline
}

// TestJSONStringCostMatchesEncoder pins the hand-written cost table to the
// real encoder, for every byte value and the runes it escapes.
func TestJSONStringCostMatchesEncoder(t *testing.T) {
	var cases []string
	for b := 0; b < 256; b++ {
		cases = append(cases, string([]byte{byte(b)}))
	}
	cases = append(cases,
		"\u2028", "\u2029", "\u00e9", "\u20ac", "\U0001F600", "\ufffd",
		"\xe2\x82", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xc0\xaf",
		"<a href=\"x\">&amp;</a>\\\r\n\t\b\f",
	)
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		b := make([]byte, 64)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		cases = append(cases, string(b))
	}
	for _, s := range cases {
		if got, want := jsonStringCost(s), encodedStringLen(t, s); got != want {
			t.Errorf("jsonStringCost(%q) = %d, encoder writes %d", s, got, want)
		}
	}
}

func TestFitJSONStringCutsToBudget(t *testing.T) {
	markerCost := jsonStringCost(truncationMarker)

	if got := fitJSONString("short", 5, 100); got != "short" {
		t.Fatalf("a string that fits was changed: %q", got)
	}
	if got := fitJSONString(strings.Repeat("a", 100), 100, markerCost-1); got != "" {
		t.Fatalf("budget below the marker = %q, want empty", got)
	}

	s := strings.Repeat("\x01", 100) + strings.Repeat("\u00e9", 100) + "<&>" + truncationMarker
	cost := jsonStringCost(s)
	for budget := markerCost; budget < cost; budget += 7 {
		got := fitJSONString(s, cost, budget)
		if c := encodedStringLen(t, got); c > budget {
			t.Fatalf("budget %d: fitted string encodes to %d bytes", budget, c)
		}
		if !strings.HasSuffix(got, truncationMarker) || strings.Count(got, "[output truncated") != 1 {
			t.Fatalf("budget %d: want exactly one trailing marker, got %q", budget, got)
		}
		if !strings.HasPrefix(s, strings.TrimSuffix(got, truncationMarker)) {
			t.Fatalf("budget %d: result is not a prefix of the input", budget)
		}
		if body := strings.TrimSuffix(got, truncationMarker); !utf8ValidPrefix(body) {
			t.Fatalf("budget %d: cut split a rune: %q", budget, body)
		}
	}
}

func utf8ValidPrefix(s string) bool { return strings.ToValidUTF8(s, "?") == s }

// TestExecResponseFitsHostileShapes: the fitted reply encodes within the limit
// whatever the output is made of, and output that fits is left alone.
func TestExecResponseFitsHostileShapes(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	request := proto.Envelope{RequestID: "r1", Action: "shell.exec"}
	cases := map[string]proto.ExecResult{
		"fits":                  {Stdout: "hello\n", Stderr: "warn\n"},
		"html escapes":          {Stdout: strings.Repeat("<", 3*mib)},
		"invalid utf8":          {Stdout: strings.Repeat("\xff", 2*mib), Stderr: strings.Repeat("\xfe", 2*mib)},
		"line separators":       {Stderr: strings.Repeat("\u2028", 3*mib)},
		"small stdout big err":  {Stdout: "ok", Stderr: strings.Repeat("\x02", 3*mib)},
		"exit code carried too": {Stdout: strings.Repeat("\x00", 3*mib), ExitCode: 3, TimedOut: true},
	}
	for name, result := range cases {
		env := execResponse(request, result)
		var buf bytes.Buffer
		if err := proto.Encode(&buf, env); err != nil {
			t.Fatalf("%s: fitted reply does not encode: %v", name, err)
		}
		decoded, err := proto.Decode(&buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := proto.DecodePayload[proto.ExecResult](decoded.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if got.ExitCode != result.ExitCode || got.TimedOut != result.TimedOut {
			t.Fatalf("%s: exit/timeout changed: %+v", name, got)
		}
		if name == "fits" && (got.Stdout != result.Stdout || got.Stderr != result.Stderr) {
			t.Fatalf("output that fits was changed: %+v", got)
		}
		if name == "small stdout big err" && got.Stdout != "ok" {
			t.Fatalf("a small stream lost bytes to its neighbour: %q", got.Stdout)
		}
	}
}

// failingWriter writes up to limit bytes, then fails.
type failingWriter struct {
	buf    bytes.Buffer
	limit  int
	closed bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	room := w.limit - w.buf.Len()
	if room <= 0 {
		return 0, errors.New("channel closed")
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		return room, errors.New("channel closed")
	}
	return w.buf.Write(p)
}

func (w *failingWriter) Close() error { w.closed = true; return nil }

// TestReplyEncoderDoesNotWriteAfterAPartialFrame: once part of a frame is on
// the wire the stream is out of step, so no fallback may follow it; the channel
// is closed instead.
func TestReplyEncoderDoesNotWriteAfterAPartialFrame(t *testing.T) {
	w := &failingWriter{limit: 10}
	e := &replyEncoder{channel: w}
	err := e.encode(proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: "r", Action: "hello", Payload: strings.Repeat("x", 100)})
	if err == nil {
		t.Fatal("encode reported success on a failed write")
	}
	if w.buf.Len() != 10 || !w.closed {
		t.Fatalf("wrote %d bytes, closed=%v; want only the partial frame, then close", w.buf.Len(), w.closed)
	}
}

// TestReplyEncoderFallsBackWithoutHugeEcho: when even the error reply is too
// large because of the request fields it echoes, a reply without them is sent.
func TestReplyEncoderFallsBackWithoutHugeEcho(t *testing.T) {
	w := &failingWriter{limit: 1 << 30}
	e := &replyEncoder{channel: w}
	huge := strings.Repeat("<", proto.MaxEnvelopeSize/6+1)
	if err := e.encode(proto.Envelope{Type: proto.EnvelopeTypeResponse, RequestID: huge, Action: huge}); err == nil {
		t.Fatal("encode of an oversized envelope reported success")
	}
	resp, err := proto.Decode(&w.buf)
	if err != nil {
		t.Fatalf("no decodable fallback: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != codeResponseTooLarge || resp.RequestID != "" || resp.Action != "" {
		t.Fatalf("fallback = %+v", resp)
	}
	if w.buf.Len() != 0 || w.closed {
		t.Fatalf("unexpected trailing bytes (%d) or close (%v)", w.buf.Len(), w.closed)
	}
}
