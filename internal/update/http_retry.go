// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPGetAttempts = 3
	defaultHTTPRetryDelay  = 500 * time.Millisecond
	maxHTTPRetryDelay      = 30 * time.Second
)

type httpGetRetryPolicy struct {
	attempts int
	baseWait time.Duration
	maxWait  time.Duration
	wait     func(context.Context, time.Duration) error
}

func defaultHTTPGetRetryPolicy() httpGetRetryPolicy {
	return httpGetRetryPolicy{
		attempts: defaultHTTPGetAttempts,
		baseWait: defaultHTTPRetryDelay,
		maxWait:  maxHTTPRetryDelay,
		wait:     waitForHTTPRetry,
	}
}

// getHTTPBodyWithRetry performs a bounded sequence of idempotent GETs. The
// client factory is invoked for each attempt so every retry gets a fresh request
// deadline and re-runs the DNS-pinned redirect policy. Only transient transport
// failures and 408/429/5xx responses are retried; URL/policy failures and other
// permanent 4xx responses fail immediately.
func getHTTPBodyWithRetry(
	ctx context.Context,
	rawURL string,
	newClient func() *http.Client,
	bodyLimit int64,
	label string,
	policy httpGetRetryPolicy,
) ([]byte, error) {
	if policy.attempts <= 0 {
		policy.attempts = 1
	}
	if policy.baseWait < 0 {
		policy.baseWait = 0
	}
	if policy.maxWait <= 0 {
		policy.maxWait = maxHTTPRetryDelay
	}
	if policy.wait == nil {
		policy.wait = waitForHTTPRetry
	}

	var lastErr error
	for attempt := 1; attempt <= policy.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create %s request: %w", label, err)
		}

		resp, err := newClient().Do(req)
		retry := false
		retryAfter := ""
		if err != nil {
			lastErr = redactHTTPErrorURL(err)
			retry = ctx.Err() == nil && isRetryableHTTPTransportError(err)
		} else {
			retryAfter = resp.Header.Get("Retry-After")
			if resp.StatusCode == http.StatusOK {
				data, readErr := readBoundedHTTPBody(resp.Body, bodyLimit, label)
				closeErr := resp.Body.Close()
				if readErr == nil && closeErr == nil {
					return data, nil
				}
				if readErr != nil {
					lastErr = readErr
				} else {
					lastErr = fmt.Errorf("close %s body: %w", label, closeErr)
				}
				// A short/truncated response can be retried safely. A body that
				// crossed the explicit size bound is permanent and remains fail-closed.
				retry = !strings.Contains(lastErr.Error(), "exceeds maximum size")
			} else {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
				lastErr = fmt.Errorf("unexpected %s status %s", label, resp.Status)
				retry = isRetryableHTTPStatus(resp.StatusCode)
			}
		}

		if !retry || attempt == policy.attempts {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if attempt > 1 {
				return nil, fmt.Errorf("%s failed after %d attempts: %w", label, attempt, lastErr)
			}
			return nil, fmt.Errorf("%s: %w", label, lastErr)
		}

		delay := retryDelay(attempt, policy.baseWait, policy.maxWait, retryAfter, time.Now())
		if err := policy.wait(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s: %w", label, lastErr)
}

func isRetryableHTTPTransportError(err error) bool {
	for {
		urlErr, ok := err.(*url.Error)
		if !ok {
			break
		}
		err = urlErr.Err
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func redactHTTPErrorURL(err error) error {
	urlErr, ok := err.(*url.Error)
	if !ok {
		return err
	}
	redacted := *urlErr
	redacted.Err = redactHTTPErrorURL(urlErr.Err)
	if parsed, parseErr := url.Parse(redacted.URL); parseErr == nil {
		parsed.RawQuery = ""
		parsed.Fragment = ""
		redacted.URL = parsed.Redacted()
	}
	return &redacted
}

func isRetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(attempt int, baseWait, maxWait time.Duration, retryAfter string, now time.Time) time.Duration {
	if delay, ok := parseRetryAfter(retryAfter, now); ok {
		if delay > maxWait {
			return maxWait
		}
		return delay
	}
	if baseWait <= 0 {
		return 0
	}
	delay := baseWait
	for i := 1; i < attempt && delay < maxWait; i++ {
		if delay > maxWait/2 {
			return maxWait
		}
		delay *= 2
	}
	if delay > maxWait {
		return maxWait
	}
	return delay
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > int64(maxHTTPRetryDelay/time.Second) {
			return maxHTTPRetryDelay, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	if !when.After(now) {
		return 0, true
	}
	return when.Sub(now), true
}

func waitForHTTPRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
