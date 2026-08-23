//go:build !windows

package main

// enableVT is a no-op away from Windows: terminals here interpret escape
// sequences without being asked.
func enableVT() bool { return true }
