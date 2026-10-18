package fileshare

import (
	"encoding/json"
	"mime"
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

// The pages hand downloads to the browser's download manager now, so the
// headers are what decide how the file is saved. See asAttachment.
func TestDownloadIsSavedAsTheFileItIs(t *testing.T) {
	dir := t.TempDir()
	// A semicolon ends a parameter unless the value is quoted. A double
	// quote would test that harder, but Windows does not allow one in a
	// filename and this runs there too.
	files := map[string]string{
		"page.html":       "<p>saved, not rendered</p>",
		"caf\xc3\xa9.txt": "accented",
		"notes; v2.txt":   "semicolon",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	share, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	for name, body := range files {
		t.Run(name, func(t *testing.T) {
			rec := get(t, share, "/api/download", name)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != body {
				t.Errorf("body = %q, want %q", got, body)
			}
			// text/html would get the service worker's shims spliced into
			// the saved file.
			if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream", ct)
			}
			disp, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
			if err != nil {
				t.Fatalf("Content-Disposition %q does not parse: %v",
					rec.Header().Get("Content-Disposition"), err)
			}
			if disp != "attachment" || params["filename"] != name {
				t.Errorf("Content-Disposition = %s %v, want attachment with filename %q", disp, params, name)
			}
		})
	}
}

// A browser resumes an interrupted download with a Range request. For a
// file the size of the one that prompted this, that is the difference
// between picking up where it stopped and starting over.
func TestDownloadAnswersRange(t *testing.T) {
	share, _ := newTestShare(t)
	req := httptest.NewRequest(http.MethodGet, "/api/download?path=hello.txt", nil)
	req.Header.Set("Range", "bytes=9-")
	rec := httptest.NewRecorder()
	share.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "of hello" {
		t.Errorf("body = %q, want the tail %q", got, "of hello")
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Error("no Accept-Ranges: bytes, so a browser will not try to resume")
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
