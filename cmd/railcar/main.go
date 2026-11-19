// Command railcar is a terminal Tieroute client from Reroute Holdings LLC (README.md).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"github.com/rerouteholdings/railcar/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `Railcar is a terminal client for Tieroute, a federated, end-to-end
encrypted (E2EE) chat protocol.

Usage:
  railcar [--profile DIR]
  railcar --demo
  railcar --version

Options:
  --profile DIR  Keep the identity, keys, and messages in DIR.
                 The default is %s.
  --demo         Run with made-up chats and no network, to try the interface.
                 Nothing is saved or sent.
  --version      Print the version and exit.
  -h, --help     Show this help.

Railcar is in early development, and it can't connect to a homeserver yet.
Use --demo to try the interface.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	defaultProfile := defaultProfileDir()
	fs := flag.NewFlagSet("railcar", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stdout, usage, defaultProfile) }
	profile := fs.String("profile", defaultProfile, "")
	demo := fs.Bool("demo", false, "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "Run railcar --help to see the options.")
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "railcar: unexpected argument %q. Run railcar --help to see the options.\n", fs.Arg(0))
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "railcar", version)
		return 0
	}
	if !*demo {
		// The protocol isn't ready, so only the demo runs for now. The
		// profile directory is where it will keep its data.
		fmt.Fprintf(stderr, "railcar: Railcar can't connect to a homeserver yet. Run railcar --demo to try the interface.\n(The profile directory would be %s.)\n", *profile)
		return 1
	}

	backend := ui.NewDemoBackend(ui.DemoOptions{Live: true})
	defer backend.Close()
	app := ui.New(ui.Options{Backend: backend, Demo: true})
	if _, err := tea.NewProgram(app).Run(); err != nil {
		fmt.Fprintln(stderr, "railcar:", err)
		return 1
	}
	return 0
}

// defaultProfileDir is railcar in the system's configuration directory,
// such as ~/Library/Application Support/railcar on macOS.
func defaultProfileDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "railcar"
	}
	return filepath.Join(dir, "railcar")
}
