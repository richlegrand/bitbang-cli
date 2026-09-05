package fileshare

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// The browser reaches a share through these handlers, and nothing tested
// them until now -- the coverage stopped at SafePath, one layer below.
// That is why a download could be broken on Windows while every test
// passed. These run on each OS in CI, so the platform differences that
// only show up in path handling have somewhere to fail.

func newTestShare(t *testing.T) (*FileShare, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hello.txt", "contents of hello")
	write("sub/nested.txt", "contents of nested")
	write("a file with spaces.txt", "spaced")

	share, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return share, dir
}

func get(t *testing.T, share *FileShare, path, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := path
	if query != "" {
		target += "?path=" + url.QueryEscape(query)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	share.HTTPHandler().ServeHTTP(rec, req)
	return rec
}

// The bug in #38: clicking a file in the browser produced "Failed to
// download file" when the listener ran on Windows.
func TestDownloadServesFileContents(t *testing.T) {
	share, _ := newTestShare(t)

	cases := []struct {
		name, path, want string
	}{
		{"at the share root", "hello.txt", "contents of hello"},
		{"in a subdirectory", "sub/nested.txt", "contents of nested"},
		{"with spaces in the name", "a file with spaces.txt", "spaced"},
		{"leading slash", "/hello.txt", "contents of hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(t, share, "/api/download", c.path)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != c.want {
				t.Errorf("body = %q, want %q", got, c.want)
			}
			if cd := rec.Header().Get("Content-Disposition"); cd == "" {
				t.Error("no Content-Disposition, so a browser will render rather than save")
			}
		})
	}
}

func TestListShowsWhatDownloadCanFetch(t *testing.T) {
	share, _ := newTestShare(t)

	// The page has no path field to use: it joins the directory it is
	// showing with the entry name, with a forward slash, because it is
	// building a URL. Whether that survives on a listener whose
	// filesystem separator is a backslash is the whole question.
	var list func(dir string) int
	list = func(dir string) int {
		rec := get(t, share, "/api/list", dir)
		if rec.Code != http.StatusOK {
			t.Fatalf("list %q = %d (%s)", dir, rec.Code, rec.Body.String())
		}
		var listing struct {
			Entries []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
			t.Fatalf("list is not the shape the page parses: %v (%s)", err, rec.Body.String())
		}

		checked := 0
		for _, e := range listing.Entries {
			child := e.Name
			if dir != "" {
				child = dir + "/" + e.Name
			}
			if e.Type == "directory" {
				checked += list(child)
				continue
			}
			if rec := get(t, share, "/api/download", child); rec.Code != http.StatusOK {
				t.Errorf("listing offers %q but download says %d", child, rec.Code)
			}
			checked++
		}
		return checked
	}

	if n := list(""); n < 3 {
		t.Fatalf("only reached %d files, so this asserted less than it looks like", n)
	}
}

func TestDownloadRefusesWhatItShould(t *testing.T) {
	share, _ := newTestShare(t)

	for _, c := range []struct{ name, path string }{
		{"traversal", "../outside.txt"},
		{"a directory", "sub"},
		{"absent", "nope.txt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rec := get(t, share, "/api/download", c.path); rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestInfoReportsUploadState(t *testing.T) {
	share, _ := newTestShare(t)

	for _, enabled := range []bool{false, true} {
		share.UploadEnabled = enabled
		rec := get(t, share, "/api/info", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("info status = %d", rec.Code)
		}
		var info struct {
			UploadEnabled bool `json:"upload_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info.UploadEnabled != enabled {
			t.Errorf("upload_enabled = %v, want %v -- the page gates its Upload button on this", info.UploadEnabled, enabled)
		}
	}
}
