// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newEndpoints lists every endpoint added for the redesigned UI with the one
// method it accepts. Each must require the token, reject other methods, and
// (for POST) enforce the Origin/CSRF check.
var newEndpoints = []struct {
	path   string
	method string
	query  url.Values
}{
	{"/api/overview", http.MethodGet, nil},
	{"/api/preview", http.MethodGet, url.Values{"path": {"/nonexistent"}}},
	{"/api/transfers", http.MethodGet, nil},
	{"/api/transfers/stream", http.MethodGet, nil},
	{"/api/download", http.MethodGet, url.Values{"path": {"/nonexistent"}}},
	{"/api/transfers/cancel", http.MethodPost, url.Values{"id": {strings.Repeat("ab", 16)}}},
}

func doReq(t *testing.T, method, rawURL string, headers map[string]string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func withQuery(base, path string, q url.Values, token string) string {
	v := url.Values{}
	for k, vals := range q {
		v[k] = append([]string(nil), vals...)
	}
	if token != "" {
		v.Set("t", token)
	}
	return base + path + "?" + v.Encode()
}

func TestNewEndpointsRequireToken(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	for _, ep := range newEndpoints {
		for name, tok := range map[string]string{"missing": "", "wrong": strings.Repeat("0", 64)} {
			res := doReq(t, ep.method, withQuery(ts.URL, ep.path, ep.query, tok), map[string]string{"Origin": ts.URL})
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s with %s token: status %d, want 401", ep.method, ep.path, name, res.StatusCode)
			}
		}
		// The header form of the token works like the query form.
		res := doReq(t, ep.method, withQuery(ts.URL, ep.path, ep.query, ""), map[string]string{"Origin": ts.URL, "X-Fleet-Token": s.Token()})
		if res.StatusCode == http.StatusUnauthorized {
			t.Fatalf("%s %s rejected a valid X-Fleet-Token header", ep.method, ep.path)
		}
	}
}

func TestNewEndpointsRejectWrongMethod(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	for _, ep := range newEndpoints {
		wrong := []string{http.MethodPost, http.MethodPut, http.MethodDelete}
		if ep.method == http.MethodPost {
			wrong = []string{http.MethodGet, http.MethodPut, http.MethodDelete}
		}
		for _, m := range wrong {
			res := doReq(t, m, withQuery(ts.URL, ep.path, ep.query, s.Token()), map[string]string{"Origin": ts.URL})
			if res.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: status %d, want 405", m, ep.path, res.StatusCode)
			}
		}
	}
}

func TestNewPostEndpointsEnforceCSRF(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	u, _ := url.Parse(ts.URL)
	otherPort := "http://127.0.0.1:1"
	if u.Port() == "1" {
		otherPort = "http://127.0.0.1:2"
	}
	for _, ep := range newEndpoints {
		if ep.method != http.MethodPost {
			continue
		}
		cases := []struct {
			name    string
			headers map[string]string
		}{
			{"no origin, no fetch metadata", map[string]string{}},
			{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
			{"remote origin", map[string]string{"Origin": "https://evil.example"}},
			{"other loopback port", map[string]string{"Origin": otherPort}},
			{"null origin", map[string]string{"Origin": "null"}},
		}
		for _, c := range cases {
			res := doReq(t, ep.method, withQuery(ts.URL, ep.path, ep.query, s.Token()), c.headers)
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s (%s): status %d, want 403", ep.method, ep.path, c.name, res.StatusCode)
			}
		}
		// Same-origin passes the CSRF gate (the unknown id then 404s).
		res := doReq(t, ep.method, withQuery(ts.URL, ep.path, ep.query, s.Token()), map[string]string{"Origin": ts.URL})
		if res.StatusCode == http.StatusForbidden {
			t.Fatalf("%s %s: same-origin request rejected", ep.method, ep.path)
		}
	}
}

// The same-origin tightening applies to every mutating endpoint, not only the
// new ones: a page on another local port cannot drive a mkdir.
func TestMutationsRequireSameOrigin(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	dir := t.TempDir()
	code, _ := postAPI(t, s, ts.URL, "/api/mkdir", url.Values{"dir": {dir}, "name": {"x"}}, "http://localhost:1")
	if code != http.StatusForbidden {
		t.Fatalf("cross-port loopback origin: status %d, want 403", code)
	}
	code, body := postAPI(t, s, ts.URL, "/api/mkdir", url.Values{"dir": {dir}, "name": {"y"}}, ts.URL)
	if code != http.StatusOK {
		t.Fatalf("same-origin mkdir: status %d %s", code, body)
	}
}

func TestOriginMatchesHost(t *testing.T) {
	t.Parallel()
	mk := func(host, origin string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://"+host+"/api/mkdir", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	cases := []struct {
		host, origin string
		want         bool
	}{
		{"127.0.0.1:9445", "", true},
		{"127.0.0.1:9445", "http://127.0.0.1:9445", true},
		{"localhost:9445", "http://LOCALHOST:9445", true},
		{"127.0.0.1:9445", "http://127.0.0.1:9446", false},
		{"127.0.0.1:9445", "http://localhost:9445", false},
		{"127.0.0.1:9445", "null", false},
		{"127.0.0.1:9445", "http://evil.example", false},
	}
	for _, c := range cases {
		if got := originMatchesHost(mk(c.host, c.origin)); got != c.want {
			t.Fatalf("originMatchesHost(host=%q origin=%q) = %v, want %v", c.host, c.origin, got, c.want)
		}
	}
}

func TestNewEndpointsRejectRebindingHost(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	for _, ep := range newEndpoints {
		res := doReq(t, ep.method, withQuery(ts.URL, ep.path, ep.query, s.Token()), map[string]string{"Host": "attacker.example:80", "Origin": ts.URL})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with rebinding Host: status %d, want 403", ep.method, ep.path, res.StatusCode)
		}
	}
	for _, p := range []string{"/", "/app.js"} {
		res := doReq(t, http.MethodGet, ts.URL+p, map[string]string{"Host": "attacker.example"})
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s with rebinding Host: status %d, want 403", p, res.StatusCode)
		}
	}
}

func TestStaticAssetsAndSecurityHeaders(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	want := map[string]string{
		"/":        "text/html; charset=utf-8",
		"/app.js":  "text/javascript; charset=utf-8",
		"/app.css": "text/css; charset=utf-8",
	}
	for p, ctype := range want {
		res := doReq(t, http.MethodGet, ts.URL+p, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", p, res.StatusCode)
		}
		if got := res.Header.Get("Content-Type"); got != ctype {
			t.Fatalf("GET %s Content-Type %q, want %q", p, got, ctype)
		}
		csp := res.Header.Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, directive) {
				t.Fatalf("GET %s CSP %q lacks %q", p, csp, directive)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "http:") || strings.Contains(csp, "https:") {
			t.Fatalf("GET %s CSP was weakened: %q", p, csp)
		}
		for h, v := range map[string]string{
			"X-Content-Type-Options":       "nosniff",
			"X-Frame-Options":              "DENY",
			"Cache-Control":                "no-store",
			"Cross-Origin-Opener-Policy":   "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := res.Header.Get(h); got != v {
				t.Fatalf("GET %s %s = %q, want %q", p, h, got, v)
			}
		}
	}
}

// The page must not carry inline scripts, inline styles, event-handler
// attributes or external resources: the CSP would block them, and their
// absence is what lets it stay strict.
func TestIndexHasNoInlineCodeOrExternalResources(t *testing.T) {
	t.Parallel()
	html := strings.ToLower(string(indexHTML))
	for _, bad := range []string{"<script>", "style=", " onclick", " onload", " onerror", " oninput", "javascript:", "https://", "//cdn", "<style"} {
		if strings.Contains(html, bad) {
			t.Fatalf("index.html contains %q", bad)
		}
	}
	if !onlyNamespaceURIs(html) {
		t.Fatalf("index.html references an http:// resource")
	}
	for _, name := range []string{"assets/app.css"} {
		data, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, bad := range []string{"https://", "eval(", "new Function(", "@import"} {
			if strings.Contains(text, bad) {
				t.Fatalf("%s references %q", name, bad)
			}
		}
		if !onlyNamespaceURIs(text) {
			t.Fatalf("%s references an http:// resource", name)
		}
	}
}

// onlyNamespaceURIs reports whether every "http://" occurrence is the SVG/XLink
// namespace identifier (which is a name, not a network reference).
func onlyNamespaceURIs(text string) bool {
	rest := text
	for {
		i := strings.Index(rest, "http://")
		if i < 0 {
			return true
		}
		tail := rest[i:]
		if !strings.HasPrefix(tail, "http://www.w3.org/2000/svg") && !strings.HasPrefix(tail, "http://www.w3.org/1999/xlink") {
			return false
		}
		rest = rest[i+len("http://"):]
	}
}

func TestTransferStreamSendsSnapshots(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	id, err := s.hub.startMeta(transferMeta{Kind: "copy", Label: "a.txt", SrcPath: "/a.txt", DstServer: "web-01", DstPath: "/srv/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	res := doReq(t, http.MethodGet, withQuery(ts.URL, "/api/transfers/stream", nil, s.Token()), nil)
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type %q", ct)
	}
	buf := make([]byte, 4096)
	n, err := res.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	first := string(buf[:n])
	if !strings.HasPrefix(first, "data: [") || !strings.Contains(first, id) || !strings.Contains(first, `"dst_server":"web-01"`) {
		t.Fatalf("first event = %q", first)
	}
}
