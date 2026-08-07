// Command bitbang is the BitBang CLI. It can serve a shell, files, and
// HTTP endpoints; connect to another listener; copy files; or publish a
// running tmux session.
//
// Usage:
//
//	bitbang serve                                 # all caps: shell + files + proxy + TCP
//	bitbang serve shell [flags]                   # shell + TCP forwarding
//	bitbang serve files [PATH] [flags]            # files only, PATH defaults to cwd
//	bitbang serve proxy [flags]                   # proxy only (HTTP reverse proxy)
//	bitbang share [flags]                         # publish the current tmux session
//	bitbang share status|stop|rotate              # manage a running share
//	bitbang cp <src> <dst>                        (one side is <URL>:/path, or `-`)
//	bitbang connect <URL> [-- argv]               # shell or command
//	bitbang connect <URL> -L port:host:port       # forwarding only
//
// `bitbang serve` is the umbrella mode — its default cap set (today:
// shell + files + proxy + TCP) is what most users want, and the hamburger
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

const version = "0.5.0-dev"

const banner = `   ___         ___
  / __\_ _    / __\
 /__\/(_) |_ /__\// ___ ____  ___ _
/ \/  \ |  _/ \/  \/ _ ` + "`" + `/ _ \/ _ ` + "`" + `/
\_____/_|\__\_____/\_,_/_//_/\_, /
                            /___/  `

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		dispatchServe(os.Args[2:])
	case "share":
		dispatchShare(os.Args[2:])
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
	fmt.Println("  bitbang serve [flags]                  All caps (shell + files + proxy + TCP)")
	fmt.Println("  bitbang serve shell [flags]            Shell + CLI TCP forwarding")
	fmt.Println("  bitbang serve files [PATH] [flags]     Files only (PATH defaults to cwd)")
	fmt.Println("  bitbang serve proxy [flags]            Proxy only (HTTP reverse proxy)")
	fmt.Println("  bitbang share [flags]                  Publish the current tmux session as a URL")
	fmt.Println("  bitbang share status|stop|rotate       Manage a running share")
	fmt.Println("  bitbang cp <src> <dst>                 Copy files (one side is <URL>:/path, or '-')")
	fmt.Println("  bitbang connect <URL-or-code> [-- ...]  Open a shell or run a command")
	fmt.Println("  bitbang connect <URL-or-code> -L port:host:port [-L ...] [-g]")
	fmt.Println("                                             Hold local TCP forwards without a shell")
	fmt.Println()
	fmt.Println("Run a command with `--help` for its available flags.")
}
