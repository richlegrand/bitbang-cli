package fileshare

import (
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"

	"github.com/richlegrand/bitbang/internal/streamtype"
)

// FileShare implements streamtype.Filesystem so the file-type SWSP stream
// handler can serve the same directory tree the HTTP routes do. Path-
// traversal protection (SafePath) and hidden-file filtering (ShouldShow)
// are reused, keeping behavior consistent across the two access paths.

// StatPath implements streamtype.Filesystem.
func (f *FileShare) StatPath(relPath string) (streamtype.FileStat, error) {
	abs := f.resolveForRead(relPath)
	if abs == "" {
		return streamtype.FileStat{}, streamtype.ErrNotFound
	}
	info, err := os.Stat(abs)
	if err != nil {
		return streamtype.FileStat{}, streamtype.ErrNotFound
	}
	return statForInfo(filepath.Base(abs), info), nil
}

// ListPath implements streamtype.Filesystem.
func (f *FileShare) ListPath(relPath string) ([]streamtype.FileStat, error) {
	if f.Mode == ModeSend {
		return nil, errors.New("send mode: no directory to list")
	}
	abs := f.resolveForRead(relPath)
	if abs == "" {
		return nil, streamtype.ErrNotFound
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, streamtype.ErrNotFound
	}
	dir, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	out := make([]streamtype.FileStat, 0, len(names))
	for _, name := range names {
		if !ShouldShow(name, false) {
			continue
		}
		st, err := os.Stat(filepath.Join(abs, name))
		if err != nil {
			continue
		}
		out = append(out, statForInfo(name, st))
	}
	return out, nil
}

// OpenRead implements streamtype.Filesystem.
func (f *FileShare) OpenRead(relPath string) (io.ReadCloser, streamtype.FileStat, error) {
	abs := f.resolveForRead(relPath)
	if abs == "" {
		return nil, streamtype.FileStat{}, streamtype.ErrNotFound
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return nil, streamtype.FileStat{}, streamtype.ErrNotFound
	}
	r, err := os.Open(abs)
	if err != nil {
		return nil, streamtype.FileStat{}, err
	}
	return r, statForInfo(filepath.Base(abs), info), nil
}

// OpenWrite implements streamtype.Filesystem.
//
// Upload-via-cp is only allowed when fileshare is in browse mode AND
// UploadEnabled — same gate as the /api/upload HTTP route.
func (f *FileShare) OpenWrite(relPath string, overwrite bool) (io.WriteCloser, error) {
	if f.Mode == ModeSend {
		return nil, errors.New("listener is in send-mode (single file); uploads not allowed")
	}
	if !f.UploadEnabled {
		return nil, errors.New("uploads not enabled (start the listener with --upload to allow file uploads)")
	}
	// The target path must resolve to inside BasePath. SafePath verifies
	// existing paths; for new files we have to check the parent.
	abs, err := f.resolveForWrite(relPath)
	if err != nil {
		return nil, err
	}
	if !overwrite {
		if _, err := os.Stat(abs); err == nil {
			return nil, streamtype.ErrExists
		}
	}
	// Disallow writing hidden / system files.
	if !ShouldShow(filepath.Base(abs), false) {
		return nil, errors.New("invalid filename")
	}
	return os.Create(abs)
}

// resolveForRead returns the absolute path inside BasePath, or "" on
// traversal / not-found.
func (f *FileShare) resolveForRead(relPath string) string {
	if f.Mode == ModeSend {
		// In send mode the only "path" is the shared file itself; any
		// other request is a 404 from cp's perspective.
		if relPath == "" || relPath == "/" || relPath == "." || relPath == f.FileName {
			return f.BasePath
		}
		return ""
	}
	return SafePath(f.BasePath, relPath)
}

// resolveForWrite is like resolveForRead but accepts paths whose parent
// is inside BasePath, even if the file itself doesn't yet exist.
func (f *FileShare) resolveForWrite(relPath string) (string, error) {
	base, err := filepath.Abs(f.BasePath)
	if err != nil {
		return "", err
	}
	requested, err := filepath.Abs(filepath.Join(base, relPath))
	if err != nil {
		return "", err
	}
	if requested != base && !hasPrefixSep(requested, base) {
		return "", errors.New("path traversal")
	}
	// Parent must exist and be a directory.
	parent := filepath.Dir(requested)
	if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return "", errors.New("parent directory does not exist")
	}
	return requested, nil
}

func hasPrefixSep(s, prefix string) bool {
	if len(s) <= len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix && s[len(prefix)] == os.PathSeparator
}

func statForInfo(name string, info os.FileInfo) streamtype.FileStat {
	st := streamtype.FileStat{
		Name:     name,
		Modified: info.ModTime().Unix(),
	}
	if info.IsDir() {
		st.Type = "directory"
	} else {
		st.Type = "file"
		st.Size = info.Size()
		if mt := mime.TypeByExtension(filepath.Ext(name)); mt != "" {
			st.Mime = mt
		}
	}
	return st
}

// Compile-time check that FileShare implements streamtype.Filesystem.
var _ streamtype.Filesystem = (*FileShare)(nil)
