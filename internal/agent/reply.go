// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cenvero/fleet/pkg/proto"
)

// truncationMarker is appended to shell.exec output that was cut short, so the
// controller can tell the output was cut rather than ending naturally. It marks
// both limits the output can hit: the per-stream capture cap and the 16 MiB cap
// on the whole encoded reply.
const truncationMarker = "\n[output truncated: exceeded 16 MiB limit]"

// Error codes for a reply the agent produced but could not send.
const (
	codeResponseTooLarge     = "response_too_large"
	codeResponseEncodeFailed = "response_encode_failed"
)

// replyEncoder writes envelopes to one RPC channel, one at a time, and makes
// sure a reply that cannot be sent is answered with an error instead.
//
// proto.Encode refuses an envelope over proto.MaxEnvelopeSize, or one
// encoding/json cannot marshal, before it writes anything. Every reply used to
// be sent as `_ = encode(...)`, so such a reply was dropped and the controller
// waited out its own timeout with no explanation.
type replyEncoder struct {
	mu      sync.Mutex
	channel io.WriteCloser
	written int // bytes of the current envelope that reached the channel
}

// Write counts what reaches the channel, so encode can tell whether a failed
// envelope left the stream at a frame boundary.
func (e *replyEncoder) Write(p []byte) (int, error) {
	n, err := e.channel.Write(p)
	e.written += n
	return n, err
}

func (e *replyEncoder) encode(env proto.Envelope) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.send(env)
	if err == nil {
		return nil
	}
	if e.written > 0 {
		// Part of the frame is on the wire, so the stream is no longer at a frame
		// boundary and nothing more can be sent on it. Close it so the
		// controller sees the channel end instead of waiting for the rest.
		_ = e.channel.Close()
		return err
	}

	code, failure := codeResponseTooLarge, "is too large to send"
	if isMarshalError(err) {
		code, failure = codeResponseEncodeFailed, "could not be encoded"
	}
	action := env.Action
	if len(action) > 128 {
		action = "the request"
	}
	message := fmt.Sprintf("the agent's reply to %s %s: %v", action, failure, err)
	if len(message) > 1024 {
		message = message[:1024]
	}
	fallback := proto.Envelope{
		Type:            proto.EnvelopeTypeResponse,
		ProtocolVersion: proto.CurrentProtocolVersion,
		RequestID:       env.RequestID,
		Action:          env.Action,
		Error:           &proto.Error{Code: code, Message: message},
	}
	if e.send(fallback) == nil {
		return err
	}
	if e.written == 0 {
		// The echoed request ID or action is itself too large to send. A reply
		// without them still answers the controller's pending call, which
		// accepts a response that carries no request ID.
		fallback.Action = ""
		if len(fallback.RequestID) > 256 {
			fallback.RequestID = ""
		}
		fallback.Error.Message = "the agent's reply could not be sent"
		if e.send(fallback) == nil {
			return err
		}
	}
	// Nothing can be sent on this channel. Closing it ends the controller's
	// wait instead of leaving it to time out.
	_ = e.channel.Close()
	return err
}

func (e *replyEncoder) send(env proto.Envelope) error {
	e.written = 0
	return proto.Encode(e, env)
}

// isMarshalError reports whether err is encoding/json refusing a value, as
// opposed to the codec's size limits.
func isMarshalError(err error) bool {
	var unsupportedValue *json.UnsupportedValueError
	var unsupportedType *json.UnsupportedTypeError
	var marshaler *json.MarshalerError
	return errors.As(err, &unsupportedValue) || errors.As(err, &unsupportedType) || errors.As(err, &marshaler)
}

// execReplySlack is headroom kept under proto.MaxEnvelopeSize when output is
// trimmed to fit a shell.exec reply.
const execReplySlack = 1024

// execResponse builds the reply to a shell.exec request. stdout and stderr are
// each captured up to 16 MiB, which is also the limit on the whole encoded
// envelope, and JSON escaping can expand output up to six times (a control
// byte becomes \u0001). Output that would push the reply over the limit is cut
// to fit, ending with truncationMarker, so the reply can always be sent.
func execResponse(request proto.Envelope, result proto.ExecResult) proto.Envelope {
	env := proto.Envelope{
		Type:            proto.EnvelopeTypeResponse,
		ProtocolVersion: proto.CurrentProtocolVersion,
		RequestID:       request.RequestID,
		Action:          request.Action,
		// Set here rather than left to the codec, so the measurement below is
		// of exactly the bytes the codec will send.
		Timestamp: time.Now().UTC(),
	}
	stdoutCost, stderrCost := jsonStringCost(result.Stdout), jsonStringCost(result.Stderr)

	// Measure everything except the two output strings.
	bare := result
	bare.Stdout, bare.Stderr = "", ""
	env.Payload = bare
	encoded, err := json.Marshal(env)
	if err != nil {
		// Left to the reply encoder, which reports it.
		env.Payload = result
		return env
	}
	budget := proto.MaxEnvelopeSize - len(encoded) - execReplySlack
	if stdoutCost+stderrCost > budget {
		stdoutShare, stderrShare := splitReplyBudget(stdoutCost, stderrCost, budget)
		result.Stdout = fitJSONString(result.Stdout, stdoutCost, stdoutShare)
		result.Stderr = fitJSONString(result.Stderr, stderrCost, stderrShare)
	}
	env.Payload = result
	return env
}

// splitReplyBudget divides budget between two streams costing a and b bytes
// when they do not both fit: a stream that fits in half keeps all of it and
// the other gets the rest; otherwise each gets half.
func splitReplyBudget(a, b, budget int) (int, int) {
	half := budget / 2
	switch {
	case a <= half:
		return a, budget - a
	case b <= half:
		return budget - b, b
	default:
		return budget - half, half
	}
}

// fitJSONString returns s unchanged if its encoded cost fits budget; otherwise
// the longest prefix of s that fits together with truncationMarker, followed by
// the marker. A marker already at the end of s (from the capture cap) is cut
// with the rest, so the result carries at most one.
func fitJSONString(s string, cost, budget int) string {
	if cost <= budget {
		return s
	}
	keep := budget - jsonStringCost(truncationMarker)
	if keep < 0 {
		return ""
	}
	used, cut := 0, 0
	for cut < len(s) {
		c, size := jsonUnitCost(s[cut:])
		if used+c > keep {
			break
		}
		used += c
		cut += size
	}
	return s[:cut] + truncationMarker
}

// jsonStringCost is the number of bytes encoding/json writes for s inside the
// quotes of a JSON string.
func jsonStringCost(s string) int {
	total := 0
	for i := 0; i < len(s); {
		c, size := jsonUnitCost(s[i:])
		total += c
		i += size
	}
	return total
}

// jsonUnitCost returns the encoded size of the first byte or rune of s and how
// many bytes of s it covers. It mirrors encoding/json's string encoding with
// HTML escaping, which proto.Encode uses: <, > and & become < and so on;
// control bytes other than \b, \f, \n, \r and \t become \u00XX; each invalid
// UTF-8 byte becomes \ufffd; U+2028 and U+2029 are escaped. A test pins this
// table to the real encoder.
func jsonUnitCost(s string) (cost, size int) {
	if b := s[0]; b < utf8.RuneSelf {
		switch {
		case b == '"' || b == '\\' || b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
			return 2, 1
		case b < 0x20 || b == '<' || b == '>' || b == '&':
			return 6, 1
		default:
			return 1, 1
		}
	}
	r, size := utf8.DecodeRuneInString(s)
	if (r == utf8.RuneError && size == 1) || r == '\u2028' || r == '\u2029' {
		return 6, size
	}
	return size, size
}
