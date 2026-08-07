//go:build !unix

package main

import (
	"strings"
	"testing"
)

func TestShareHostingReportsUnsupportedPlatform(t *testing.T) {
	if shareHostSupported {
		t.Fatal("non-Unix build unexpectedly enables share hosting")
	}
	_, err := resolveShareTarget(shareConfig{})
	if err == nil || !strings.Contains(err.Error(), "Unix or WSL") {
		t.Fatalf("resolveShareTarget error = %v, want unsupported-platform guidance", err)
	}
}
