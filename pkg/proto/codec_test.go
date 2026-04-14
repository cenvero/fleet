// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	original := Envelope{
		Type:            EnvelopeTypeRequest,
		ProtocolVersion: CurrentProtocolVersion,
		RequestID:       "req-1",
		Action:          "service.restart",
		Capabilities:    []string{"service.manage"},
		Payload: map[string]any{
			"server":  "web-01",
			"service": "nginx",
		},
	}

	var buf bytes.Buffer
	if err := Encode(&buf, original); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := Decode(&buf)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if decoded.Action != original.Action {
		t.Fatalf("decoded action = %s, want %s", decoded.Action, original.Action)
	}
	if decoded.RequestID != original.RequestID {
		t.Fatalf("decoded request id = %s, want %s", decoded.RequestID, original.RequestID)
	}
}
