// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Launch codes keep the session token out of the browser's command line.
//
// Opening the UI used to run `open`/`xdg-open http://…/?t=<token>`, and a
// process's arguments are visible to every user on the machine (ps,
// /proc/<pid>/cmdline): another local account could read the token and,
// with the Control or Terminal view on, act on the fleet. Now the browser is
// opened on `/?c=<code>`: a random code that works once, within
// launchCodeTTL. Loading the page with it puts the token into that page
// only (see handleIndex), and the page drops the code from the address bar.
//
// A code that was already used fails visibly ("this link was already used"),
// so if another account ever raced the browser to one, the operator sees it.
const (
	launchCodeTTL  = 30 * time.Second
	maxLaunchCodes = 32
)

type launchCodes struct {
	mu    sync.Mutex
	codes map[[32]byte]time.Time // sha256(code) -> expiry
	// spent remembers redeemed codes for their lifetime: one presented again
	// was used by two parties, so the session token is replaced.
	spent map[[32]byte]time.Time
}

// What redeeming a launch code found.
type launchOutcome int

const (
	launchInvalid  launchOutcome = iota // unknown or expired
	launchRedeemed                      // valid; it is now spent
	launchReused                        // already spent: the session token was replaced
)

// NewLaunchCode returns a one-time code that opens the UI with this
// server's session token, valid for launchCodeTTL.
func (s *Server) NewLaunchCode() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate launch code: %w", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw[:])
	s.launch.mu.Lock()
	defer s.launch.mu.Unlock()
	if s.launch.codes == nil {
		s.launch.codes = map[[32]byte]time.Time{}
	}
	now := time.Now()
	for k, exp := range s.launch.codes {
		if now.After(exp) {
			delete(s.launch.codes, k)
		}
	}
	if len(s.launch.codes) >= maxLaunchCodes {
		return "", fmt.Errorf("too many unused launch codes; wait %s", launchCodeTTL)
	}
	s.launch.codes[sha256.Sum256([]byte(code))] = now.Add(launchCodeTTL)
	return code, nil
}

// redeemLaunchCode consumes code. Codes are looked up by their hash, so the
// lookup reveals nothing about a valid code's bytes.
//
// A code presented a second time within its lifetime was used by two
// parties: the browser it was opened for and something else on this machine
// that read it from the browser's command line (or the other way round).
// Whoever came first holds the session token, so it is replaced: the copy
// handed out stops working, and the operator, who sees "this link was
// already used", opens the console again with a fresh code.
func (s *Server) redeemLaunchCode(code string) launchOutcome {
	if code == "" || len(code) > 128 {
		return launchInvalid
	}
	key := sha256.Sum256([]byte(code))
	now := time.Now()
	s.launch.mu.Lock()
	defer s.launch.mu.Unlock()
	if s.launch.spent == nil {
		s.launch.spent = map[[32]byte]time.Time{}
	}
	for k, exp := range s.launch.spent {
		if now.After(exp) {
			delete(s.launch.spent, k)
		}
	}
	if _, reused := s.launch.spent[key]; reused {
		s.rotateToken()
		return launchReused
	}
	exp, ok := s.launch.codes[key]
	if !ok {
		return launchInvalid
	}
	delete(s.launch.codes, key)
	if now.After(exp) {
		return launchInvalid
	}
	s.launch.spent[key] = exp
	return launchRedeemed
}

// rotateToken replaces the session token with a fresh one: every page and
// link that carried the old one stops working.
func (s *Server) rotateToken() {
	token, err := randomToken()
	if err != nil {
		// Without randomness, do not keep serving a token that leaked.
		token = ""
	}
	s.tokenMu.Lock()
	s.token = token
	s.tokenMu.Unlock()
	if s.onTokenReset != nil {
		s.onTokenReset()
	}
}

// SetOnTokenReset runs fn after a reused launch code replaced the session
// token (the console tells its operator, or its log).
func (s *Server) SetOnTokenReset(fn func()) { s.onTokenReset = fn }

// LaunchURL is the address that opens the UI in a browser without the
// session token: base is the server's root URL ("http://127.0.0.1:9445/")
// and view an optional view to land on ("files").
func LaunchURL(base, code, view string) string {
	u := strings.TrimSuffix(base, "/") + "/?c=" + code
	if view != "" {
		u += "#" + view
	}
	return u
}

// SetLaunchKey lets another process on this machine ask for launch codes:
// a POST to /api/launch-code carrying this key in X-Fleet-Launch-Key returns
// a fresh code. `fleet console open` uses it to open a console that runs in
// the background; the key lives in a 0600 file in the controller's data
// directory, never the session token, which stays in memory only.
func (s *Server) SetLaunchKey(key string) { s.launchKey = key }

// handleLaunchCode answers POST /api/launch-code for a caller holding the
// launch key. It has the same loopback, Host and Origin checks as every
// other mutation.
func (s *Server) handleLaunchCode(w http.ResponseWriter, r *http.Request) {
	if !s.launchKeyRequestOK(w, r) {
		return
	}
	code, err := s.NewLaunchCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	writeJSON(w, map[string]string{"code": code})
}

// SetShutdown lets a process holding the launch key ask this server to stop
// (POST /api/shutdown): `fleet console stop` stops a background console that
// way, so the runs and terminals it started are ended cleanly, which a
// terminated process (on Windows, the only other way) cannot do.
func (s *Server) SetShutdown(fn func()) { s.shutdownFn = fn }

// handleShutdown answers POST /api/shutdown for a caller holding the launch
// key, with the same checks as /api/launch-code.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if !s.launchKeyRequestOK(w, r) {
		return
	}
	if s.shutdownFn == nil {
		http.Error(w, "this console cannot be stopped this way", http.StatusNotImplemented)
		return
	}
	writeJSON(w, map[string]bool{"stopping": true})
	go s.shutdownFn()
}

// launchKeyRequestOK applies the checks every launch-key endpoint shares and
// answers the request itself when one fails.
func (s *Server) launchKeyRequestOK(w http.ResponseWriter, r *http.Request) bool {
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden: loopback only", http.StatusForbidden)
		return false
	}
	if !hostHeaderOK(r) {
		http.Error(w, "forbidden: bad host header", http.StatusForbidden)
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !originLoopback(r) || !originMatchesHost(r) {
		http.Error(w, "forbidden: bad origin", http.StatusForbidden)
		return false
	}
	provided := r.Header.Get("X-Fleet-Launch-Key")
	if s.launchKey == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.launchKey)) != 1 {
		http.Error(w, "unauthorized: missing or bad launch key", http.StatusUnauthorized)
		return false
	}
	return true
}

// activity tracks requests a person made, for a console that stops itself
// when nobody has used it for a while. Streams the page holds open and the
// overview's automatic refresh are not use.
type activity struct {
	last     atomic.Int64 // unix nanoseconds of the last request that counted
	inFlight atomic.Int64
}

// passivePaths are requests the page makes by itself.
var passivePaths = map[string]bool{
	"/api/progress":         true,
	"/api/overview":         true,
	"/api/transfers":        true,
	"/api/transfers/stream": true,
	"/api/runs/stream":      true,
	"/api/term/stream":      true,
	"/api/term":             true,
}

func (s *Server) trackActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.countsAsUse(r) {
			next.ServeHTTP(w, r)
			return
		}
		s.activity.inFlight.Add(1)
		defer func() {
			s.markUsed()
			s.activity.inFlight.Add(-1)
		}()
		s.markUsed()
		next.ServeHTTP(w, r)
	})
}

// countsAsUse reports whether r is something a person did with this
// session's credentials. Only those keep a background console alive: a
// request without the token (any local process, or a web page aiming at
// 127.0.0.1) must not be able to hold it open. Loading the page itself
// counts in handleIndex, once a launch code or the token is checked.
func (s *Server) countsAsUse(r *http.Request) bool {
	switch {
	case passivePaths[r.URL.Path]:
		return false
	case r.URL.Path == "/api/launch-code" || r.URL.Path == "/api/shutdown":
		provided := r.Header.Get("X-Fleet-Launch-Key")
		return s.launchKey != "" && provided != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(s.launchKey)) == 1
	case strings.HasPrefix(r.URL.Path, "/api/"):
		return s.tokenOK(r)
	}
	return false
}

func (s *Server) markUsed() { s.activity.last.Store(time.Now().UnixNano()) }

// IdleFor reports how long it has been since someone last used the UI
// (zero while a request of theirs, such as an upload, is still running).
// Before the first use it counts from when the server started.
func (s *Server) IdleFor() time.Duration {
	if s.activity.inFlight.Load() > 0 {
		return 0
	}
	last := s.activity.last.Load()
	if last == 0 {
		return 0 // not serving yet
	}
	return time.Since(time.Unix(0, last))
}
