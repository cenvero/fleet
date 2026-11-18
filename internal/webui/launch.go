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
}

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

// redeemLaunchCode consumes code, reporting whether it was valid and unused.
// Codes are looked up by their hash, so the lookup reveals nothing about a
// valid code's bytes.
func (s *Server) redeemLaunchCode(code string) bool {
	if code == "" || len(code) > 128 {
		return false
	}
	key := sha256.Sum256([]byte(code))
	s.launch.mu.Lock()
	defer s.launch.mu.Unlock()
	exp, ok := s.launch.codes[key]
	if !ok {
		return false
	}
	delete(s.launch.codes, key)
	return time.Now().Before(exp)
}

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
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden: loopback only", http.StatusForbidden)
		return
	}
	if !hostHeaderOK(r) {
		http.Error(w, "forbidden: bad host header", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !originLoopback(r) || !originMatchesHost(r) {
		http.Error(w, "forbidden: bad origin", http.StatusForbidden)
		return
	}
	provided := r.Header.Get("X-Fleet-Launch-Key")
	if s.launchKey == "" || provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(s.launchKey)) != 1 {
		http.Error(w, "unauthorized: missing or bad launch key", http.StatusUnauthorized)
		return
	}
	code, err := s.NewLaunchCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	writeJSON(w, map[string]string{"code": code})
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
	case r.URL.Path == "/api/launch-code":
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
