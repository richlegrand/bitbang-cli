package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/richlegrand/bitbang/internal/identity"
	"github.com/richlegrand/bitbang/internal/links"
)

// dispatchLink routes `bitbang link <cmd>`, the editor-side of the link
// table. The listener is the only writer in the steady state, so these
// commands read, and `rm` and `edit` hand the file to you rather than
// growing a flag per term. Scope plus expiry plus whatever comes next is
// a record, not a set of orthogonal booleans, and $EDITOR gives
// completion and history for free.
func dispatchLink(args []string) {
	if len(args) == 0 {
		printLinkUsage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ls":
		runLinkLs(rest)
	case "edit":
		runLinkEdit(rest)
	case "rm":
		runLinkRm(rest)
	case "qr":
		runLinkQR(rest)
	case "help", "--help", "-h":
		printLinkUsage()
	default:
		fmt.Fprintf(os.Stderr, "bitbang link: unknown command %q\n\n", cmd)
		printLinkUsage()
		os.Exit(2)
	}
}

func printLinkUsage() {
	fmt.Println("Usage:")
	fmt.Println("  bitbang link ls                        List this program's links")
	fmt.Println("  bitbang link edit                      Open links.json in $EDITOR")
	fmt.Println("  bitbang link rm <label>                Delete a link")
	fmt.Println("  bitbang link qr <label>                Print a link's QR code")
	fmt.Println()
	fmt.Println("  --program NAME   Which listener's table (default \"bitbang\", the")
	fmt.Println("                   identity `serve` and `serve shell` use). A files-")
	fmt.Println("                   or proxy-only listener has its own; `link ls` with")
	fmt.Println("                   no table lists the names it can see.")
	fmt.Println()
	fmt.Println("A link is created by adding an entry with no code and reloading the")
	fmt.Println("listener, which mints one.")
}

func linkFlags(name string, args []string) (string, []string) {
	program, _, rest := linkFlagsFull(name, args)
	return program, rest
}

func linkFlagsFull(name string, args []string) (string, string, []string) {
	fs := flag.NewFlagSet("link "+name, flag.ExitOnError)
	program := fs.String("program", "bitbang", "which listener's link table")
	server := fs.String("server", defaultServer, "Signaling server hostname")
	fs.Parse(reorderArgs(fs, args))
	return *program, *server, fs.Args()
}

func linkPath(program string) string {
	return filepath.Join(identity.Dir(program), links.Filename)
}

// loadForEdit reads the table for a command that will write it back. The
// modtime comes with it: writing checks the file has not moved since,
// so a listener minting a code concurrently is noticed rather than
// clobbered.
func loadForEdit(program string) ([]links.Terms, time.Time) {
	entries, mod, err := links.Load(linkPath(program))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	return entries, mod
}

func runLinkLs(args []string) {
	program, server, rest := linkFlagsFull("ls", args)
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "bitbang link ls: unexpected argument %q\n", rest[0])
		os.Exit(2)
	}
	path := linkPath(program)
	entries, _, err := links.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 {
		fmt.Printf("No links for program %q (%s).\n", program, path)
		if others := programsWithTables(); len(others) > 0 {
			fmt.Printf("Tables exist for: %s\n", strings.Join(others, ", "))
		}
		fmt.Println("Add an entry with `bitbang link edit`, then reload the listener to mint a code.")
		return
	}

	// The URL is the thing you actually send someone, so print it rather
	// than the bare code -- ls is the command you reach for when handing a
	// link over, and it was the one view of the table that could not give
	// you one. Falls back to the code when there is no identity yet to
	// build a URL from.
	uid := ""
	if id, err := identity.Load(program, false); err == nil {
		uid = id.UID
	}

	now := time.Now()
	labelW, scopeW := 0, 0
	for _, e := range entries {
		if n := len(e.Label); n > labelW {
			labelW = n
		}
		if n := len(scopeOf(e)); n > scopeW {
			scopeW = n
		}
	}
	for _, e := range entries {
		fmt.Printf("  %-*s  %-*s  %-14s  %s\n",
			labelW, e.Label, scopeW, scopeOf(e), expiryNote(e, now), linkURL(server, uid, e))
	}
}

// scopeOf renders the scope as written in the file. Unlike the
// listener's own listing this cannot narrow it to what is actually
// served, because nothing here knows which mode the listener is running.
func scopeOf(e links.Terms) string {
	if e.Scope == nil {
		return "(everything served)"
	}
	return strings.Join(e.Scope, " ")
}

func linkURL(server, uid string, e links.Terms) string {
	switch {
	case e.Code == "":
		return "(no code until renewed)"
	case uid == "":
		return e.Code
	default:
		return "https://" + server + "/" + uid + "#" + e.Code
	}
}

func runLinkEdit(args []string) {
	program, rest := linkFlags("edit", args)
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "bitbang link edit: unexpected argument %q\n", rest[0])
		os.Exit(2)
	}
	path := linkPath(program)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot create %s: %v\n", filepath.Dir(path), err)
		os.Exit(1)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		seed := "[\n" +
			"  {\n" +
			"    \"label\": \"example\",\n" +
			"    \"scope\": [\"files\"],\n" +
			"    \"expires\": \"" + time.Now().AddDate(0, 0, 7).UTC().Format(time.RFC3339) + "\"\n" +
			"  }\n" +
			"]\n"
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "Cannot create %s: %v\n", path, err)
			os.Exit(1)
		}
	}

	editor := os.Getenv("EDITOR")
	if editor == "" {
		if runtime.GOOS == "windows" {
			editor = "notepad"
		} else {
			editor = "vi"
		}
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", editor, err)
		os.Exit(1)
	}

	// Validate before leaving, so a mistake surfaces here rather than as
	// a listener that refuses to start. Build as well as Load: a
	// duplicate label, or an entry that collides with the implicit `owner`
	// row, is only visible once the table is assembled.
	if err := validateTable(path); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		fmt.Fprintln(os.Stderr, "The listener will refuse to start until this is fixed.")
		os.Exit(1)
	}
	fmt.Println("Saved. A running listener picks it up at its console `reload`, or on restart.")
}

func runLinkRm(args []string) {
	program, rest := linkFlags("rm", args)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang link rm <label>")
		os.Exit(2)
	}
	label := rest[0]
	if label == links.OwnerLabel {
		fmt.Fprintf(os.Stderr,
			"%q is the identity's own code, not a table entry: removing it would revoke your\n"+
				"own access to a listener you are running, and it would come back on the next\n"+
				"reload anyway.\n", links.OwnerLabel)
		os.Exit(2)
	}

	entries, mod := loadForEdit(program)
	kept := make([]links.Terms, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.Label == label {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		fmt.Fprintf(os.Stderr, "No link labeled %q in %s\n", label, linkPath(program))
		os.Exit(1)
	}
	if err := links.Save(linkPath(program), kept, mod); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %q. Reload the listener to close any session still using it.\n", label)
}

func runLinkQR(args []string) {
	program, server, rest := linkFlagsFull("qr", args)
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: bitbang link qr <label>")
		os.Exit(2)
	}
	label := rest[0]

	idPath := filepath.Join(identity.Dir(program), "identity.pem")
	if _, err := os.Stat(idPath); err != nil {
		fmt.Fprintf(os.Stderr, "No identity for program %q yet; run the listener once first.\n", program)
		os.Exit(1)
	}
	id, err := identity.Load(program, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Identity error: %v\n", err)
		os.Exit(1)
	}

	code := ""
	if label == links.OwnerLabel {
		code = id.Code
	} else {
		entries, _, err := links.Load(linkPath(program))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		for _, e := range entries {
			if e.Label == label {
				code = e.Code
			}
		}
		if code == "" {
			fmt.Fprintf(os.Stderr,
				"No minted link labeled %q. Reload the listener to mint one.\n", label)
			os.Exit(1)
		}
	}

	url := "https://" + server + "/" + id.UID + "#" + code
	fmt.Print(smallQR(url))
	fmt.Println(url)
}

// programsWithTables lists the programs that have a links.json, so a
// user who guessed the wrong --program can see the real names.
func programsWithTables() []string {
	root := filepath.Dir(identity.Dir("bitbang"))
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, d.Name(), links.Filename)); err == nil {
			out = append(out, d.Name())
		}
	}
	return out
}

// validateTable runs the same checks the listener runs at startup. The
// scope list is every name, not this listener's, because `link` does not
// know which mode the listener will run in -- a scope it does not offer
// is a warning there, not an error.
func validateTable(path string) error {
	entries, _, err := links.Load(path)
	if err != nil {
		return err
	}
	_, _, err = links.Build(entries, links.ScopeNames(), "placeholder")
	return err
}
