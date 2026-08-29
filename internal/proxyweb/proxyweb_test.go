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
		LandingHandler(nil, nil).ServeHTTP(rec, req)
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
	LandingHandler(nil, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// With targets named, the form is disabled rather than removed or replaced.
// The caret is the selection element everywhere else in the product and the
// targets are already entries in it; a chooser here would be a second
// mechanism for one job. The control stays visible so the page still says
// what is normally there.
func TestLandingPageDisablesTheFormWhenTargetsAreNamed(t *testing.T) {
	rec := httptest.NewRecorder()
	LandingHandler(nil, []string{"a.lan:80", "b.lan:8080"}).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `id="target"`) {
		t.Error("the form was removed; it should be disabled and still visible")
	}
	for _, want := range []string{
		`placeholder="set by the listener" disabled`,
		`<button onclick="go()" disabled>`,
		"pick one from the menu above",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// The targets belong in the caret, not on the page.
	for _, unwanted := range []string{"a.lan:80", "b.lan:8080"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page lists %q itself; that is the caret's job", unwanted)
		}
	}
}

// Without targets the page is unchanged -- the browser still names its own.
func TestLandingPageKeepsTheFormWhenUnrestricted(t *testing.T) {
	rec := httptest.NewRecorder()
	LandingHandler(nil, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `id="target"`) {
		t.Error("the target form is missing on an unrestricted proxy")
	}
	// The stylesheet mentions :disabled either way, so check the attribute.
	if strings.Contains(body, `disabled`) && strings.Contains(body, "set by the listener") {
		t.Error("the form is disabled with no targets named")
	}
	if !strings.Contains(body, `placeholder="localhost:3000"`) {
		t.Error("the usual placeholder is gone on an unrestricted proxy")
	}
}

// A markup change must not quietly leave the form live, inviting someone to
// type a target that is refused downstream.
func TestLandingPageFailsIfTheTemplateMoved(t *testing.T) {
	if _, ok := disableForm("<html><body>nothing familiar</body></html>"); ok {
		t.Error("disableForm reported success on a template it could not match")
	}
}
