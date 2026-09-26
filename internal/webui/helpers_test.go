// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Cenvero / Shubhdeep Singh

package webui

import (
	"io"
	"net/http"
	"net/url"
	"testing"
)

// postAPI sends an authenticated POST with the given Origin (empty = none).
func postAPI(t *testing.T, s *Server, base, endpoint string, params url.Values, origin string) (int, string) {
	t.Helper()
	params.Set("t", s.Token())
	req, err := http.NewRequest(http.MethodPost, base+endpoint+"?"+params.Encode(), nil)
	if err != nil {
		t.Fatal(err)
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
