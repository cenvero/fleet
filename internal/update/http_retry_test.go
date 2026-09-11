// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestHTTPGetRetriesTransientStatusAndHonorsRetryAfterCap(t *testing.T) {
	attempts := 0
	var waited []time.Duration
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts < 3 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Status:     "503 Service Unavailable",
				Header:     http.Header{"Retry-After": []string{"60"}},
				Body:       io.NopCloser(strings.NewReader("retry")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("ok")),
		}, nil
	})}
	policy := httpGetRetryPolicy{
		attempts: 3,
		baseWait: time.Millisecond,
		maxWait:  2 * time.Second,
		wait: func(_ context.Context, delay time.Duration) error {
			waited = append(waited, delay)
			return nil
		},
	}

	body, err := getHTTPBodyWithRetry(context.Background(), "https://example.com/artifact", func() *http.Client { return client }, 1024, "artifact", policy)
	if err != nil {
		t.Fatalf("getHTTPBodyWithRetry() error = %v", err)
	}
	if string(body) != "ok" || attempts != 3 {
		t.Fatalf("body=%q attempts=%d, want ok and 3 attempts", body, attempts)
	}
	if len(waited) != 2 || waited[0] != 2*time.Second || waited[1] != 2*time.Second {
		t.Fatalf("waited=%v, want Retry-After capped to 2s on each retry", waited)
	}
}

func TestHTTPGetDoesNotRetryPermanentStatus(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("missing")),
		}, nil
	})}
	policy := httpGetRetryPolicy{attempts: 3, wait: func(context.Context, time.Duration) error { return nil }}

	_, err := getHTTPBodyWithRetry(context.Background(), "https://example.com/missing", func() *http.Client { return client }, 1024, "artifact", policy)
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("error=%v, want permanent 404", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}
}

func TestHTTPGetRetriesTransientNetworkError(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.DNSError{Err: "temporary resolver failure", IsTemporary: true}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("recovered")),
		}, nil
	})}
	policy := httpGetRetryPolicy{attempts: 3, wait: func(context.Context, time.Duration) error { return nil }}

	body, err := getHTTPBodyWithRetry(context.Background(), "https://example.com/artifact", func() *http.Client { return client }, 1024, "artifact", policy)
	if err != nil {
		t.Fatalf("getHTTPBodyWithRetry() error = %v", err)
	}
	if string(body) != "recovered" || attempts != 2 {
		t.Fatalf("body=%q attempts=%d, want recovered and 2 attempts", body, attempts)
	}
}

func TestHTTPGetCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		cancel()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("retry")),
		}, nil
	})}
	policy := httpGetRetryPolicy{attempts: 3, baseWait: time.Hour, maxWait: time.Hour, wait: waitForHTTPRetry}

	_, err := getHTTPBodyWithRetry(ctx, "https://example.com/artifact", func() *http.Client { return client }, 1024, "artifact", policy)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}
}

func TestHTTPGetDoesNotRetryPolicyErrorWrappedByURLError(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("refusing update redirect to unapproved host")}
	})}
	policy := httpGetRetryPolicy{attempts: 3, wait: func(context.Context, time.Duration) error { return nil }}

	_, err := getHTTPBodyWithRetry(context.Background(), "https://example.com/artifact?sig=secret", func() *http.Client { return client }, 1024, "artifact", policy)
	if err == nil || !strings.Contains(err.Error(), "refusing update redirect") {
		t.Fatalf("error=%v, want policy rejection", err)
	}
	if strings.Contains(err.Error(), "sig=secret") {
		t.Fatalf("error leaked URL query parameters: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want policy rejection to fail immediately", attempts)
	}
}
