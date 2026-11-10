package main

import (
	"errors"
	"flag"

	"github.com/richlegrand/bitbang-cli/internal/bbhome"
)

// homeFlag registers -home on a command's flags.
//
// Every command gets it, not just `serve`. The directory is not serve's: it
// holds the identity, the link table, the share state, and the device table
// that `connect` and `cp` read. With the flag on one command, every other
// command on that box would need the environment variable anyway, and
// forgetting it once on `bitbang link` would quietly edit a different table.
//
// Registered with Func rather than String so it applies during Parse, before
// any command body can ask bbhome where to look. There is nothing to thread
// through and no order to get wrong.
func homeFlag(fs *flag.FlagSet) {
	// The backquoted word is what flag prints as the operand name, so this
	// renders as `-home directory` rather than Func's default `-home value`.
	fs.Func("home",
		"`directory` for BitBang's own files -- identity, links, devices, "+
			"shares (default ~/.bitbang; also BITBANG_HOME)",
		func(v string) error {
			if v == "" {
				return errors.New("needs a directory")
			}
			bbhome.Set(v)
			return nil
		})
}
