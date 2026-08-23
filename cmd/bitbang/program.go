package main

import "errors"

// errIdentityBusy is returned by acquireIdentityLock when another local process
// already holds the per-identity lock (see lock_unix.go).
var errIdentityBusy = errors.New("identity already in use by another process")

// deriveProgram picks the identity "program" name -- the directory under
// ~/.bitbang/<program>/ whose keypair fixes the UID, and so the shareable
// URL.
//
// One device, one identity. Authorization is carried by codes now, not by
// addresses: a link's scope says what it reaches and its terms say for how
// long, and several codes live under one UID. Deriving a separate identity
// per files path or proxy target predates that, from when a code carried
// nothing and a distinct URL was the only way to say "this one reaches only
// that task". It worked, and it cost a namespace of machine-generated names
// nobody could see or type -- which is also why `bitbang link` had no usable
// way to name a listener.
//
// An explicit --program still wins, and is now the whole story for anyone who
// genuinely wants two independent devices out of one machine. Embedders use it
// that way already (the OctoPrint plugin pins "octoprint").
func deriveProgram(cfg serveConfig) string {
	if cfg.program != "" {
		return cfg.program
	}
	return defaultProgram
}

// defaultProgram is the identity every listener uses unless told otherwise.
const defaultProgram = "bitbang"
