package fileshare

import (
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/richlegrand/bitbang-cli/internal/streamtype"
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
// traversal / not-found / hidden.
func (f *FileShare) resolveForRead(relPath string) string {
	if f.Mode == ModeSend {
		// In send mode the only "path" is the shared file itself; any
		// other request is a 404 from cp's perspective.
		if relPath == "" || relPath == "/" || relPath == "." || relPath == f.FileName {
			return f.BasePath
		}
		return ""
	}
	abs := SafePath(f.BasePath, relPath)
	if abs == "" || !visibleUnder(f.BasePath, abs) {
		return ""
	}
	return abs
}

// visibleUnder reports whether every path component between the share root
// and abs passes ShouldShow.
//
// Hiding an entry from the listing is not a read control on its own: without
// this, ".env" is absent from a directory listing but still served to anyone
// who asks for it by name, and so is anything inside a hidden directory.
// Checking the whole relative path rather than just the basename is what
// stops ".git/config" from being reachable while ".git" is hidden.
func visibleUnder(baseDir, abs string) bool {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(base); err == nil {
		base = real
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		if !ShouldShow(part, false) {
			return false
		}
	}
	return true
}

// resolveForWrite is like resolveForRead but accepts paths whose parent
// is inside BasePath, even if the file itself doesn't yet exist.
//
// Symlinks are resolved for the same reason as in SafePath, but there are
// two cases rather than one. If the target already exists it may itself be a
// symlink pointing outside the share, and os.Create would follow it — so the
// target is resolved and re-checked. If it doesn't exist there is nothing to
// resolve, and the parent directory carries the check instead, since a
// symlinked parent redirects the write just as effectively.
func (f *FileShare) resolveForWrite(relPath string) (string, error) {
	base, err := filepath.Abs(f.BasePath)
	if err != nil {
		return "", err
	}
	requested, err := filepath.Abs(filepath.Join(base, relPath))
	if err != nil {
		return "", err
	}
	if !withinBase(requested, base) {
		return "", errors.New("path traversal")
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	// Existing target: resolve it and re-check.
	if realPath, err := filepath.EvalSymlinks(requested); err == nil {
		if !withinBase(realPath, realBase) {
			return "", errors.New("path traversal")
		}
		return realPath, nil
	}
	// New file: the parent must exist, be a directory, and resolve inside
	// the base.
	realParent, err := filepath.EvalSymlinks(filepath.Dir(requested))
	if err != nil {
		return "", errors.New("parent directory does not exist")
	}
	if st, err := os.Stat(realParent); err != nil || !st.IsDir() {
		return "", errors.New("parent directory does not exist")
	}
	if !withinBase(realParent, realBase) {
		return "", errors.New("path traversal")
	}
	return filepath.Join(realParent, filepath.Base(requested)), nil
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
