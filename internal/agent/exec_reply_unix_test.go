// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

//go:build !windows

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/cenvero/fleet/pkg/proto"
)

// TestShellExecReplyFitsEnvelopeLimit: stdout and stderr are each captured up
// to 16 MiB, which is also the limit on the whole encoded reply. Output that
// would push the encoded reply over the limit must be trimmed, with the
// truncation marker, so a decodable reply always comes back. Each case used to
// produce a reply the codec refused, so no reply was sent at all.
func TestShellExecReplyFitsEnvelopeLimit(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name    string
		command string
		// minimum bytes of each stream that must survive, to catch trimming far
		// more than needed
		minStdout, minStderr   int
		stdoutByte, stderrByte byte
		wantStderrMarker       bool
	}{
		{
			name:       "stdout over the capture cap",
			command:    "head -c 17825792 /dev/zero | tr '\\000' a",
			minStdout:  proto.MaxEnvelopeSize - 64*1024,
			stdoutByte: 'a',
		},
		{
			name:             "stdout plus stderr over the limit",
			command:          "head -c 9437184 /dev/zero | tr '\\000' a; head -c 9437184 /dev/zero | tr '\\000' b >&2",
			minStdout:        8*mib - 64*1024,
			minStderr:        8*mib - 64*1024,
			stdoutByte:       'a',
			stderrByte:       'b',
			wantStderrMarker: true,
		},
		{
			name: "control bytes that expand under escaping",
			// 3 MiB of \x01 encodes as \u0001: about 18 MiB of JSON.
			command:    "head -c 3145728 /dev/zero | tr '\\000' '\\001'",
			minStdout:  (proto.MaxEnvelopeSize - 64*1024) / 6,
			stdoutByte: 0x01,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, counted := startRPCAgent(t, Server{})
			resp := callAgent(t, session, proto.Envelope{
				RequestID: "exec-1",
				Action:    "shell.exec",
				Payload:   proto.ExecPayload{Command: tc.command},
			}, 20*time.Second)
			if resp.Error != nil {
				t.Fatalf("shell.exec error = %+v, want the trimmed output", resp.Error)
			}
			frame := counted.read.Load()
			if frame > 4+proto.MaxEnvelopeSize {
				t.Fatalf("reply frame = %d bytes, over the %d-byte limit", frame, proto.MaxEnvelopeSize)
			}
			result, err := proto.DecodePayload[proto.ExecResult](resp.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if result.ExitCode != 0 {
				t.Fatalf("exit code = %d", result.ExitCode)
			}
			checkStream(t, "stdout", result.Stdout, tc.stdoutByte, tc.minStdout, true)
			if tc.stderrByte != 0 {
				checkStream(t, "stderr", result.Stderr, tc.stderrByte, tc.minStderr, tc.wantStderrMarker)
			}
			t.Logf("reply frame %d bytes; stdout %d bytes, stderr %d bytes", frame, len(result.Stdout), len(result.Stderr))
		})
	}
}

// checkStream asserts s is at least min bytes of want, ending with exactly one
// truncation marker when marked is set.
func checkStream(t *testing.T, name, s string, want byte, minimum int, marked bool) {
	t.Helper()
	const marker = "\n[output truncated: exceeded 16 MiB limit]"
	body := s
	if marked {
		if !strings.HasSuffix(s, marker) {
			t.Fatalf("%s does not end with the truncation marker (tail %q)", name, s[max(0, len(s)-64):])
		}
		body = strings.TrimSuffix(s, marker)
		if strings.Contains(body, "[output truncated") {
			t.Fatalf("%s carries more than one truncation marker", name)
		}
	}
	if len(body) < minimum {
		t.Fatalf("%s kept %d bytes, want at least %d", name, len(body), minimum)
	}
	if strings.Trim(body, string(want)) != "" {
		t.Fatalf("%s holds bytes other than %q", name, want)
	}
}
