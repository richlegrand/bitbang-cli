// Package fileshare implements BitBang's fileshare app — a port of
// ~/bitbang-python/bitbang/apps/fileshare/. Wire-compatible: the same
// browser UI works against a Python or Go listener, and `bitbang cp`
// works against either.
//
// The package exposes two things:
//   1. An http.Handler with the browser-facing API (browse/download/upload).
//   2. A Filesystem implementation that the file-type SWSP stream handler
//      uses for `bitbang cp` (Step 4).
//
// Both share the same underlying filesystem primitives so behavior is
// consistent across the two access paths.
package fileshare

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed static/*
var staticFS embed.FS

// Mode is the access mode the listener offers.
type Mode int

const (
	// ModeBrowse offers a directory tree (file browser UI).
	ModeBrowse Mode = iota
	// ModeSend offers a single file (one-click download UI).
	ModeSend
)

// FileShare is the per-listener state. One per `bitbang fileshare <path>`
// invocation. Reusable across many peer sessions.
type FileShare struct {
	// BasePath is the absolute path being shared.
	BasePath string
	// Mode is ModeBrowse for a directory, ModeSend for a single file.
	Mode Mode
	// FileName is the display name in send mode (basename of BasePath).
	FileName string
	// UploadEnabled allows POST /api/upload to write files. Off by default.
	UploadEnabled bool
}

// New creates a FileShare for the given filesystem path. Auto-detects send
// vs browse mode based on whether the path is a file or directory.
func New(p string) (*FileShare, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	fs := &FileShare{BasePath: abs}
	if info.IsDir() {
		fs.Mode = ModeBrowse
	} else {
		fs.Mode = ModeSend
		fs.FileName = filepath.Base(abs)
	}
	return fs, nil
}

// HTTPHandler returns an http.Handler with the browser-facing routes. Wire-
// compatible with the Python fileshare's app.py routes.
func (f *FileShare) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/list", f.listFiles)
	mux.HandleFunc("/api/download", f.downloadFile)
	mux.HandleFunc("/api/preview", f.previewFile)
	mux.HandleFunc("/api/upload", f.uploadFile)
	mux.HandleFunc("/api/info", f.serveInfo)
	mux.HandleFunc("/download", f.downloadSingle)
	mux.HandleFunc("/", f.index)
	return mux
}

func (f *FileShare) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if f.Mode == ModeSend {
		// Render send.html with the file's name/size/icon.
		tmpl, err := staticFS.ReadFile("static/send.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		info, err := os.Stat(f.BasePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := string(tmpl)
		out = strings.ReplaceAll(out, "{{ filename }}", f.FileName)
		out = strings.ReplaceAll(out, "{{ size }}", FormatSize(info.Size()))
		out = strings.ReplaceAll(out, "{{ icon }}", FileIcon(f.FileName))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(out))
		return
	}
	data, err := staticFS.ReadFile("static/browse.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// Entry is one item in a directory listing. JSON shape matches Python's
// /api/list response.
type Entry struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`           // "file" | "directory"
	Modified float64 `json:"modified"`       // mtime as Unix seconds
	Size     *int64  `json:"size,omitempty"` // for files only
	Mime     *string `json:"mime,omitempty"` // for files only
}

func (f *FileShare) listFiles(w http.ResponseWriter, r *http.Request) {
	if f.Mode == ModeSend {
		http.NotFound(w, r)
		return
	}
	relPath := r.URL.Query().Get("path")
	absPath := SafePath(f.BasePath, relPath)
	if absPath == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		http.Error(w, "not a directory", http.StatusBadRequest)
		return
	}
	dir, err := os.Open(absPath)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		if !ShouldShow(name, false) {
			continue
		}
		full := filepath.Join(absPath, name)
		st, err := os.Stat(full)
		if err != nil {
			continue
		}
		e := Entry{
			Name:     name,
			Modified: float64(st.ModTime().Unix()),
		}
		if st.IsDir() {
			e.Type = "directory"
		} else {
			e.Type = "file"
			size := st.Size()
			e.Size = &size
			if mt := mime.TypeByExtension(filepath.Ext(name)); mt != "" {
				e.Mime = &mt
			}
		}
		entries = append(entries, e)
	}

	// Directories first, then case-insensitive alphabetic by name.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			return entries[i].Type == "directory"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})

	resp := map[string]interface{}{
		"path":    nonEmptyOr(relPath, "/"),
		"entries": entries,
	}
	if relPath != "" {
		parent := path.Dir(relPath)
		if parent == "." {
			parent = ""
		}
		resp["parent"] = parent
	} else {
		resp["parent"] = nil
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (f *FileShare) downloadSingle(w http.ResponseWriter, r *http.Request) {
	if f.Mode != ModeSend {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.FileName))
	http.ServeFile(w, r, f.BasePath)
}

func (f *FileShare) downloadFile(w http.ResponseWriter, r *http.Request) {
	if f.Mode == ModeSend {
		http.NotFound(w, r)
		return
	}
	relPath := r.URL.Query().Get("path")
	absPath := SafePath(f.BasePath, relPath)
	if absPath == "" {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(absPath)))
	http.ServeFile(w, r, absPath)
}

func (f *FileShare) previewFile(w http.ResponseWriter, r *http.Request) {
	if f.Mode == ModeSend {
		http.NotFound(w, r)
		return
	}
	relPath := r.URL.Query().Get("path")
	absPath := SafePath(f.BasePath, relPath)
	if absPath == "" {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	mt := mime.TypeByExtension(filepath.Ext(absPath))
	if mt == "" {
		mt = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mt)
	// http.ServeFile sets Content-Type from the file content; override it.
	http.ServeFile(w, r, absPath)
}

func (f *FileShare) uploadFile(w http.ResponseWriter, r *http.Request) {
	if f.Mode == ModeSend || !f.UploadEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Stream the multipart body straight to disk via http.MaxBytesReader
	// (or just rely on multipart.Reader) — Go's parser already streams
	// without buffering the whole upload in memory.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file provided", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if header.Filename == "" {
		http.Error(w, "no file selected", http.StatusBadRequest)
		return
	}

	// Resolve target directory.
	relPath := r.FormValue("path")
	var targetDir string
	if relPath != "" {
		targetDir = SafePath(f.BasePath, relPath)
	} else {
		targetDir = f.BasePath
	}
	if targetDir == "" {
		http.Error(w, "invalid target directory", http.StatusForbidden)
		return
	}
	if st, err := os.Stat(targetDir); err != nil || !st.IsDir() {
		http.Error(w, "invalid target directory", http.StatusForbidden)
		return
	}

	// Sanitize filename — basename only, no dotfiles.
	filename := filepath.Base(header.Filename)
	if filename == "" || filename == "." || strings.HasPrefix(filename, ".") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	dest := filepath.Join(targetDir, filename)
	out, err := os.Create(dest)
	if err != nil {
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer out.Close()
	written, err := io.Copy(out, file)
	if err != nil {
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"filename": filename,
		"size":     written,
	})
}

func (f *FileShare) serveInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if f.Mode == ModeSend {
		info, err := os.Stat(f.BasePath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"mode":     "send",
			"filename": f.FileName,
			"size":     info.Size(),
		})
		return
	}

	var totalFiles int64
	var totalSize int64
	filepath.WalkDir(f.BasePath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !ShouldShow(d.Name(), false) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				totalFiles++
				totalSize += info.Size()
			}
		}
		return nil
	})

	root := filepath.Base(f.BasePath)
	if root == "" || root == "." || root == "/" {
		root = "Files"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"mode":           "browse",
		"root":           root,
		"upload_enabled": f.UploadEnabled,
		"total_files":    totalFiles,
		"total_size":     totalSize,
	})
}

func nonEmptyOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
