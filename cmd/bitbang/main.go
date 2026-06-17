// Command bitbang is the BitBang CLI: a single binary with a listener
// entrypoint (`serve`) and two client subcommands (`cp`, `connect`).
//
// Usage:
//
//	bitbang serve                                 # all caps: shell + files + proxy
//	bitbang serve shell [flags]                   # shell only
//	bitbang serve files [PATH] [flags]            # files only, PATH defaults to cwd
//	bitbang serve proxy [flags]                   # proxy only (HTTP reverse proxy)
//	bitbang cp <src> <dst>                        (one side is <URL>:/path, or `-`)
//	bitbang connect <URL> [-- argv]               (client: interactive or one-shot)
//
// `bitbang serve` is the umbrella mode — its default cap set (today:
// shell + files + proxy) is what most users want, and the hamburger
// menu on the launcher tab is how they pick which cap to open.
// Single-cap modes are for when you specifically want to expose just
// one capability and skip the hamburger UI entirely.
//
// Bare `bitbang` (no args) prints help. The earlier no-args-runs-proxy
// behavior (inherited from the old `bitbangproxy` binary) is gone —
// accidental double-clicks shouldn't silently start a listener.
package main

import (
	"fmt"
	"os"
)

const version = "0.4.0"

const banner = `   ___  _ __  ___
  / _ )(_) /_/ _ )___ ____  ___ _
 / _  / / __/ _  / _ ` + "`" + `/ _ \/ _ ` + "`" + `/
/____/_/\__/____/\_,_/_//_/\_, /
                          /___/  `

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		dispatchServe(os.Args[2:])
	case "cp":
		runCp(os.Args[2:])
	case "connect":
		runConnect(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("bitbang v%s\n", version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "bitbang: unknown subcommand %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

// dispatchServe routes `bitbang serve [mode] [flags]`. With no mode
// (bare `serve` or `serve --flag`), runs the all-caps umbrella mode.
// With `shell`, `files`, or `proxy` as the next arg, runs that single
// cap. Anything else after `serve` that starts with `-` is treated as
// a flag to the all-mode; anything else is an unknown mode.
func dispatchServe(args []string) {
	if len(args) == 0 || args[0] == "" || args[0][0] == '-' {
		runServe(args)
		return
	}
	switch args[0] {
	case "shell":
		runServeShell(args[1:])
	case "files":
		runServeFiles(args[1:])
	case "proxy":
		runServeProxy(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "bitbang serve: unknown mode %q (expected shell, files, or proxy)\n\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Printf("%s v%s\n\n", banner, version)
	fmt.Println("Usage:")
	fmt.Println("  bitbang serve [flags]                  All caps (shell + files + proxy)")
	fmt.Println("  bitbang serve shell [flags]            Shell only")
	fmt.Println("  bitbang serve files [PATH] [flags]     Files only (PATH defaults to cwd)")
	fmt.Println("  bitbang serve proxy [flags]            Proxy only (HTTP reverse proxy)")
	fmt.Println("  bitbang cp <src> <dst>                 Copy files (one side is <URL>:/path, or '-')")
	fmt.Println("  bitbang connect <URL-or-code> [-- ...]  Open shell, or pair via 6-digit code")
	fmt.Println()
	fmt.Println("Run `bitbang serve --help` (or with a mode) for the available flags.")
}
