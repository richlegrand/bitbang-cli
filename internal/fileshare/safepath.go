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
	// Must be base itself or strictly inside base.
	if requested != base && !strings.HasPrefix(requested, base+string(os.PathSeparator)) {
		return ""
	}
	if _, err := os.Stat(requested); err != nil {
		return ""
	}
	return requested
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
