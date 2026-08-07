// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package update

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxUpdateRedirects = 5

type updateIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func newUpdateHTTPClient(timeout time.Duration) *http.Client {
	return newUpdateHTTPClientForHosts(timeout, nil)
}

func newUpdateHTTPClientForHosts(timeout time.Duration, approvedHosts map[string]bool) *http.Client {
	resolver := updateIPResolver(net.DefaultResolver)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy would resolve and contact the destination outside the pinned dialer.
	// Updates fail closed instead of delegating SSRF policy to proxy configuration.
	transport.Proxy = nil
	transport.DialContext = updateDialContext(resolver, &net.Dialer{Timeout: timeout})
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := checkUpdateRedirectWithResolver(req.Context(), resolver, req, via); err != nil {
				return err
			}
			if len(approvedHosts) > 0 && !approvedHosts[strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))] {
				return fmt.Errorf("refusing update redirect to unapproved host %q", req.URL.Hostname())
			}
			return nil
		},
		Transport: transport,
	}
}

func checkUpdateRedirect(req *http.Request, via []*http.Request) error {
	return checkUpdateRedirectWithResolver(req.Context(), net.DefaultResolver, req, via)
}

func checkUpdateRedirectWithResolver(ctx context.Context, resolver updateIPResolver, req *http.Request, via []*http.Request) error {
	if len(via) > maxUpdateRedirects {
		return fmt.Errorf("refusing update redirect: maximum of %d redirects exceeded", maxUpdateRedirects)
	}
	if err := validateUpdateDestination(ctx, resolver, req.URL); err != nil {
		return fmt.Errorf("refusing update redirect to %s: %w", req.URL.Redacted(), err)
	}
	if len(via) > 0 {
		previous := via[len(via)-1].URL
		if !strings.EqualFold(previous.Scheme, req.URL.Scheme) {
			return fmt.Errorf("refusing update redirect scheme change from %s to %s", previous.Redacted(), req.URL.Redacted())
		}
	}
	return nil
}

func validateUpdateDestination(ctx context.Context, resolver updateIPResolver, parsed *url.URL) error {
	if parsed == nil {
		return fmt.Errorf("download URL is missing")
	}
	if err := validateDownloadScheme(parsed.String()); err != nil {
		return err
	}
	_, err := resolvePublicUpdateIPs(ctx, resolver, parsed.Hostname())
	return err
}

func resolvePublicUpdateIPs(ctx context.Context, resolver updateIPResolver, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" {
		return nil, fmt.Errorf("download host is empty")
	}
	if literal := net.ParseIP(host); literal != nil {
		if isForbiddenUpdateIP(literal) {
			return nil, fmt.Errorf("refusing private/internal update destination %q", host)
		}
		return []net.IP{literal}, nil
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve update host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("update host %q resolved to no addresses", host)
	}
	ips := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		if address.IP == nil || isForbiddenUpdateIP(address.IP) {
			return nil, fmt.Errorf("refusing update host %q resolving to private/internal address %q", host, address.IP)
		}
		ips = append(ips, address.IP)
	}
	return ips, nil
}

func updateDialContext(resolver updateIPResolver, dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse update destination %q: %w", address, err)
		}
		ips, err := resolvePublicUpdateIPs(ctx, resolver, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, fmt.Errorf("dial update host %q: %w", host, lastErr)
	}
}

func isForbiddenUpdateIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	// Cloud metadata and shared/address-transition ranges are never legitimate
	// release endpoints, even on platforms where net.IP does not call them private.
	for _, prefix := range forbiddenUpdatePrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

var forbiddenUpdatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("100.100.100.200/32"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}
