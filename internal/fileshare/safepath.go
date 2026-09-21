package fileshare

import (
	"os"
	"path/filepath"
	"strings"
)

// systemFiles are always hidden from listings regardless of dotfile policy.
// Mirrors Python's SYSTEM_FILES (core.py:6).
var systemFiles = map[string]bool{
	".DS_Store": true, "Thumbs.db": true, "desktop.ini": true,
	".git": true, "__pycache__": true, ".env": true,
}

// SafePath resolves relPath against base and returns the absolute path if
// it stays within base. Returns "" if traversal was attempted or the path
// doesn't exist (the latter matches Python's behavior — callers that need
// to distinguish "outside base" from "not found" should reorder their
// checks).
//
// Symlinks are resolved before the containment check. A lexical check alone
// is not enough: a symlink inside the share whose target is outside it has a
// path under base, so the prefix test passes and the kernel then follows the
// link on open. Both sides must be resolved — a share root that is itself a
// symlink is entirely normal (macOS /tmp -> /private/tmp, and many home and
// NAS layouts), and comparing a resolved path against an unresolved base
// would reject every such share.
//
// The returned path is fully resolved, so it contains no symlink components
// for a later open to follow somewhere else.
//
// Mirrors Python's safe_path (core.py:36-59).
func SafePath(baseDir, relPath string) string {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return ""
	}
	requested, err := filepath.Abs(filepath.Join(base, relPath))
	if err != nil {
		return ""
	}
	// Cheap lexical rejection first: catches ../ traversal without touching
	// the filesystem.
	if !withinBase(requested, base) {
		return ""
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return ""
	}
	// Also rejects a path that doesn't exist, preserving the documented
	// "" -on-not-found behavior.
	realPath, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return ""
	}
	if !withinBase(realPath, realBase) {
		return ""
	}
	return realPath
}

// withinBase reports whether path is base itself or strictly inside it.
//
// The separator is appended so that /srv/photos does not match /srv/photos-old,
// which is the whole point of the check -- but a base that is already a
// filesystem root ends in one. "/" becomes "//" and "D:\" becomes "D:\\" --
// prefixes nothing matches -- so every file under a root share resolved to ""
// and read as not found. A share of D:\ that listed its files and then 404'd
// every one of them is issue #38.
func withinBase(path, base string) bool {
	if path == base {
		return true
	}
	if strings.HasSuffix(base, string(os.PathSeparator)) {
		return strings.HasPrefix(path, base)
	}
	return strings.HasPrefix(path, base+string(os.PathSeparator))
}

// ShouldShow returns false for entries that should be hidden from the
// fileshare listing. System files are always hidden; dotfiles are hidden
// unless showHidden is true.
//
// Mirrors Python's should_show (core.py:62-76).
func ShouldShow(name string, showHidden bool) bool {
	if systemFiles[name] {
		return false
	}
	if !showHidden && strings.HasPrefix(name, ".") {
		return false
	}
	return true
}
