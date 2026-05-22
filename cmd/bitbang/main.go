// Command bitbang is the BitBang CLI: a single binary with subcommands
// for proxying local web apps, sharing files, and (in v2) running a shell.
//
// Usage:
//
//	bitbang proxy [--target HOST:PORT] [--pin PIN] [--ephemeral]
//	bitbang fileshare <path>          (v1)
//	bitbang shell                      (v2)
//	bitbang cp <src> <dst>             (v1, client-side)
//	bitbang connect <uid>              (v2, client-side)
//
// Running `bitbang` with no arguments is currently equivalent to `bitbang
// proxy` for backwards-compatibility with the old `bitbangproxy` binary.
package main

import (
	"fmt"
	"os"
)

const version = "0.2.0"

const banner = `   ___  _ __  ___
  / _ )(_) /_/ _ )___ ____  ___ _
 / _  / / __/ _  / _ ` + "`" + `/ _ \/ _ ` + "`" + `/
/____/_/\__/____/\_,_/_//_/\_, /
                          /___/  `

func main() {
	if len(os.Args) < 2 {
		// No subcommand → default to proxy (matches old bitbangproxy
		// behavior so existing invocations still work).
		runProxy(os.Args[1:])
		return
	}

	switch os.Args[1] {
	case "proxy":
		runProxy(os.Args[2:])
	case "fileshare":
		runFileshare(os.Args[2:])
	case "cp":
		fmt.Fprintln(os.Stderr, "cp: not yet implemented (coming in step 6)")
		os.Exit(2)
	case "shell", "connect":
		fmt.Fprintln(os.Stderr, os.Args[1]+": deferred to v2")
		os.Exit(2)
	case "version", "--version", "-v":
		fmt.Printf("bitbang v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		// Backwards compatibility: if the first arg looks like a flag,
		// fall through to proxy mode (matches old bitbangproxy CLI).
		if len(os.Args[1]) > 0 && os.Args[1][0] == '-' {
			runProxy(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "bitbang: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Printf("%s v%s\n\n", banner, version)
	fmt.Println("Usage:")
	fmt.Println("  bitbang proxy [--target HOST:PORT] [--pin PIN] [--ephemeral]")
	fmt.Println("  bitbang fileshare <path> [--pin PIN] [--upload] [--ephemeral]")
	fmt.Println("  bitbang cp <src> <dst>       (coming soon)")
	fmt.Println()
	fmt.Println("Without a subcommand, runs `bitbang proxy` for compatibility")
	fmt.Println("with the previous bitbangproxy binary.")
}
