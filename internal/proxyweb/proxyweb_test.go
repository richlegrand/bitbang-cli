package proxyweb

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLandingHandler_ServesForm(t *testing.T) {
	for _, path := range []string{"/", "/proxy/"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		LandingHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("path %s: status = %d, want 200", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Target URL") {
			t.Errorf("path %s: body missing 'Target URL' label", path)
		}
		if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("path %s: Content-Type = %q, want text/html", path, rec.Header().Get("Content-Type"))
		}
	}
}

func TestLandingHandler_404OnOtherPaths(t *testing.T) {
	req := httptest.NewRequest("GET", "/some/other/path", nil)
	rec := httptest.NewRecorder()
	LandingHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
