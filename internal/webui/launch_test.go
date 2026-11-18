// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func getPage(t *testing.T, base, query string) string {
	t.Helper()
	res, err := http.Get(base + "/" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /%s: %d %s", query, res.StatusCode, body)
	}
	return string(body)
}

// A launch code opens the page with the session token exactly once, within
// its lifetime; a used, unknown or expired code is reported to the page, and
// the token never appears without a valid code.
func TestLaunchCodeWorksOnce(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	tokenMeta := `name="fleet-launch-token" content="` + s.Token() + `"`

	if page := getPage(t, ts.URL, ""); strings.Contains(page, s.Token()) || strings.Contains(page, "__FLEET_LAUNCH") {
		t.Fatal("the plain page carries the token or an unfilled placeholder")
	}
	code, err := s.NewLaunchCode()
	if err != nil {
		t.Fatal(err)
	}
	page := getPage(t, ts.URL, "?c="+code)
	if !strings.Contains(page, tokenMeta) || !strings.Contains(page, `name="fleet-launch-state" content="ok"`) {
		t.Fatal("a fresh launch code did not open the page with the token")
	}
	page = getPage(t, ts.URL, "?c="+code)
	if strings.Contains(page, s.Token()) || !strings.Contains(page, `name="fleet-launch-state" content="used"`) {
		t.Fatal("a used launch code opened the page again, or was not reported as used")
	}
	if page := getPage(t, ts.URL, "?c=not-a-code"); strings.Contains(page, s.Token()) {
		t.Fatal("an unknown code opened the page")
	}

	// Expired.
	code, err = s.NewLaunchCode()
	if err != nil {
		t.Fatal(err)
	}
	s.launch.mu.Lock()
	for k := range s.launch.codes {
		s.launch.codes[k] = time.Now().Add(-time.Second)
	}
	s.launch.mu.Unlock()
	if page := getPage(t, ts.URL, "?c="+code); strings.Contains(page, s.Token()) {
		t.Fatal("an expired code opened the page")
	}
}

// Outstanding codes are bounded, so nothing can pile them up.
func TestLaunchCodesAreBounded(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	for i := 0; i < maxLaunchCodes; i++ {
		if _, err := s.NewLaunchCode(); err != nil {
			t.Fatalf("code %d: %v", i, err)
		}
	}
	if _, err := s.NewLaunchCode(); err == nil {
		t.Fatal("more launch codes than the limit were handed out")
	}
}

// Another local process gets a launch code only with the launch key, by
// POST, from a loopback origin matching the server — and never the token.
func TestLaunchCodeEndpoint(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	u, _ := url.Parse(ts.URL)
	origin := "http://" + u.Host
	post := func(key, origin string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/launch-code", nil)
		if key != "" {
			req.Header.Set("X-Fleet-Launch-Key", key)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	if code, _ := post("anything", origin); code != http.StatusUnauthorized {
		t.Fatalf("no launch key configured: %d, want 401", code)
	}
	s.SetLaunchKey("k3y-for-tests")
	if code, _ := post("", origin); code != http.StatusUnauthorized {
		t.Fatalf("missing key: %d, want 401", code)
	}
	if code, _ := post("wrong", origin); code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d, want 401", code)
	}
	if code, _ := post(s.Token(), origin); code != http.StatusUnauthorized {
		t.Fatalf("the session token is not the launch key: %d, want 401", code)
	}
	if code, _ := post("k3y-for-tests", ""); code != http.StatusForbidden {
		t.Fatalf("no Origin: %d, want 403", code)
	}
	if code, _ := post("k3y-for-tests", "http://evil.example"); code != http.StatusForbidden {
		t.Fatalf("foreign Origin: %d, want 403", code)
	}
	res, err := http.Get(ts.URL + "/api/launch-code")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", res.StatusCode)
	}

	code, body := post("k3y-for-tests", origin)
	if code != http.StatusOK || strings.Contains(body, s.Token()) {
		t.Fatalf("valid request: %d %s", code, body)
	}
	var out struct{ Code string }
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Code == "" {
		t.Fatalf("response %q: %v", body, err)
	}
	if page := getPage(t, ts.URL, "?c="+out.Code); !strings.Contains(page, s.Token()) {
		t.Fatal("the code from the endpoint did not open the page")
	}
}

// Requests the page makes by itself (streams, the overview refresh) do not
// keep a background console alive; anything a person does does.
func TestIdleIgnoresThePagesOwnRequests(t *testing.T) {
	t.Parallel()
	s, ts := newTestServer(t)
	old := time.Now().Add(-time.Hour).UnixNano()
	s.activity.last.Store(old)
	for _, p := range []string{"/api/overview", "/api/transfers", "/app.js"} {
		res, err := http.Get(ts.URL + p + "?t=" + s.Token())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	if got := s.IdleFor(); got < 59*time.Minute {
		t.Fatalf("the page's own requests counted as use (idle %s)", got)
	}
	res, err := http.Get(ts.URL + "/api/servers?t=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := s.IdleFor(); got > time.Minute {
		t.Fatalf("a request a person made did not count as use (idle %s)", got)
	}
}

func TestSetTokenAcceptsOnlyGeneratedTokens(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	for _, bad := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("a", 63) + "\""} {
		if err := s.SetToken(bad); err == nil {
			t.Fatalf("SetToken(%q) accepted", bad)
		}
	}
	good := strings.Repeat("ab", 32)
	if err := s.SetToken(good); err != nil || s.Token() != good {
		t.Fatalf("SetToken(valid) = %v", err)
	}
}
