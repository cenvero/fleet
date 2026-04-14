// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

func Encode(w io.Writer, env Envelope) error {
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = CurrentProtocolVersion
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now().UTC()
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	if err := binary.Write(w, binary.BigEndian, uint32(len(body))); err != nil { // #nosec G115 -- envelope size fits uint32
		return fmt.Errorf("write envelope length: %w", err)
	}

	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("write envelope body: %w", err)
	}

	return nil
}

func Decode(r io.Reader) (Envelope, error) {
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return Envelope{}, fmt.Errorf("read envelope length: %w", err)
	}
	if size == 0 {
		return Envelope{}, fmt.Errorf("envelope length must be greater than zero")
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return Envelope{}, fmt.Errorf("read envelope body: %w", err)
	}

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.ProtocolVersion == 0 {
		env.ProtocolVersion = CurrentProtocolVersion
	}
	return env, nil
}
