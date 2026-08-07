package shellweb

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestViewOnlyPageSetsFlagBeforeShellScript(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)

	view := httptest.NewRecorder()
	New(WithViewOnly()).HTTPHandler().ServeHTTP(view, request)
	body := view.Body.String()
	flagAt := strings.Index(body, "window.BB_VIEW_ONLY = true")
	scriptAt := strings.Index(body, `<script src="shell.js"></script>`)
	if flagAt < 0 || scriptAt < 0 || flagAt > scriptAt {
		t.Fatalf("view-only flag must precede shell.js: flag=%d script=%d", flagAt, scriptAt)
	}

	control := httptest.NewRecorder()
	New().HTTPHandler().ServeHTTP(control, request)
	if strings.Contains(control.Body.String(), "BB_VIEW_ONLY") {
		t.Fatal("control page included the view-only flag")
	}
}
