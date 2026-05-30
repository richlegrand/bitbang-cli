package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/richlegrand/bitbang/internal/fileshare"
)

// TestBuildServeHTTPHandler_AllModeRouting exercises the all-mode mux:
// shell-with-cap-bar at /, plain shell at /shell/, files at /files/,
// proxy landing at /proxy/.
func TestBuildServeHTTPHandler_AllModeRouting(t *testing.T) {
	share, err := fileshare.New(t.TempDir())
	if err != nil {
		t.Fatalf("fileshare.New: %v", err)
	}
	h := buildServeHTTPHandler(share, true, true, 1, true)

	cases := []struct {
		path            string
		wantStatusOneOf []int
		wantBodyHas     string
	}{
		{path: "/", wantStatusOneOf: []int{200}, wantBodyHas: "bb-cap-bar"},
		{path: "/shell/", wantStatusOneOf: []int{200}},
		{path: "/proxy/", wantStatusOneOf: []int{200}, wantBodyHas: "Target URL"},
		{path: "/files/", wantStatusOneOf: []int{200, 302}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			ok := false
			for _, s := range tc.wantStatusOneOf {
				if rec.Code == s {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("status = %d, want one of %v; body=%q",
					rec.Code, tc.wantStatusOneOf, rec.Body.String())
			}
			if tc.wantBodyHas != "" && !strings.Contains(rec.Body.String(), tc.wantBodyHas) {
				t.Errorf("body missing %q", tc.wantBodyHas)
			}
		})
	}
}

// TestBuildServeHTTPHandler_ShellRootCapBarOnlyAtSlash verifies the
// strip is injected at "/" (launcher) but NOT at "/shell/" (cap tab).
func TestBuildServeHTTPHandler_ShellRootCapBarOnlyAtSlash(t *testing.T) {
	share, err := fileshare.New(t.TempDir())
	if err != nil {
		t.Fatalf("fileshare.New: %v", err)
	}
	h := buildServeHTTPHandler(share, true, true, 1, true)

	for _, tc := range []struct {
		path     string
		wantStrip bool
	}{
		{path: "/", wantStrip: true},
		{path: "/shell/", wantStrip: false},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		hasStrip := strings.Contains(rec.Body.String(), "bb-cap-bar")
		if hasStrip != tc.wantStrip {
			t.Errorf("%s: hasStrip=%v, want %v", tc.path, hasStrip, tc.wantStrip)
		}
	}
}

// TestBuildServeHTTPHandler_CapRootsNoRedirect verifies the trailing-slash
// normalizer: a request to /proxy (no slash) must serve the form
// directly, not 301 to /proxy/.
func TestBuildServeHTTPHandler_CapRootsNoRedirect(t *testing.T) {
	share, err := fileshare.New(t.TempDir())
	if err != nil {
		t.Fatalf("fileshare.New: %v", err)
	}
	h := buildServeHTTPHandler(share, true, true, 1, true)

	for _, p := range []string{"/proxy", "/shell", "/files"} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusMovedPermanently || rec.Code == http.StatusFound {
			t.Errorf("%s returned %d redirect to %q; expected in-place rewrite",
				p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

// TestBuildServeHTTPHandler_SingleCapFastPath verifies single-cap modes
// skip the mux entirely so relative URLs in the cap's HTML work.
func TestBuildServeHTTPHandler_SingleCapFastPath(t *testing.T) {
	h := buildServeHTTPHandler(nil, true, false, 1, false)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound {
		t.Errorf("single-cap path returned a redirect, want direct handler")
	}
	// Single-cap shell must NOT include the strip — that's only on
	// the all-mode launcher.
	if strings.Contains(rec.Body.String(), "bb-cap-bar") {
		t.Errorf("single-cap shell body unexpectedly contains cap bar strip")
	}
}

// TestLauncherCapBarItems exercises the cap-bar item rules.
func TestLauncherCapBarItems(t *testing.T) {
	share, err := fileshare.New(t.TempDir())
	if err != nil {
		t.Fatalf("fileshare.New: %v", err)
	}

	cases := []struct {
		name             string
		share            *fileshare.FileShare
		shellEnabled    bool
		proxyEnabled    bool
		shellMaxSessions int
		wantLabels       []string
	}{
		{
			name:             "max=1 hides shell",
			share:            share,
			shellEnabled:     true,
			proxyEnabled:     true,
			shellMaxSessions: 1,
			wantLabels:       []string{"Files", "Proxy"},
		},
		{
			name:             "max=0 includes shell",
			share:            share,
			shellEnabled:     true,
			proxyEnabled:     true,
			shellMaxSessions: 0,
			wantLabels:       []string{"Shell", "Files", "Proxy"},
		},
		{
			name:             "max=3 includes shell",
			share:            share,
			shellEnabled:     true,
			proxyEnabled:     true,
			shellMaxSessions: 3,
			wantLabels:       []string{"Shell", "Files", "Proxy"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := launcherCapBarItems(tc.share, tc.shellEnabled, tc.proxyEnabled, tc.shellMaxSessions)
			if len(got) != len(tc.wantLabels) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tc.wantLabels), got)
			}
			for i, want := range tc.wantLabels {
				if got[i].Label != want {
					t.Errorf("item %d: got %q, want %q", i, got[i].Label, want)
				}
			}
		})
	}
}

// TestLooksLikeProxyTarget exercises the SWSP-layer dispatch heuristic.
func TestLooksLikeProxyTarget(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/nas.local/", true},
		{"/nas.local", true},
		{"/localhost:3000/api", true},
		{"/127.0.0.1:8080", true},
		{"/example.com/path", true},
		{"/localhost", true},
		{"/shell/", false},
		{"/files/index.html", false},
		{"/proxy/", false},
		{"/", false},
		{"", false},
	}
	for _, tc := range cases {
		got := looksLikeProxyTarget(tc.path)
		if got != tc.want {
			t.Errorf("looksLikeProxyTarget(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
