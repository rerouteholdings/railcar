// Command railcar is a terminal Tieroute client from Reroute Holdings LLC (README.md).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/rerouteholdings/railcar/internal/app"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `Railcar is a terminal client for Tieroute, a federated, end-to-end
encrypted (E2EE) chat protocol.

Usage:
  railcar [--profile DIR] [--key-file] [--log FILE] [--dev-server NAME=URL]...
  railcar --demo
  railcar --version

Options:
  --profile DIR          Keep the identity, keys, and messages in DIR.
                         The default is %s.
  --key-file             When there's no system keychain, keep the profile's
                         key in a file in the profile directory. Anyone who can
                         read the directory can then read your keys and messages.
  --log FILE             Write debug lines to FILE. They hold request paths and
                         errors, never message text or keys.
  --dev-server NAME=URL  Reach the server NAME at URL, over plain HTTP if the URL
                         says so. It's for local test networks, such as rerouted
                         in its development mode. Give it once for each server.
  --demo                 Run with made-up chats and no network, to try the
                         interface. Nothing is saved or sent.
  --version              Print the version and exit.
  -h, --help             Show this help.

Railcar is in early development: it has direct chats, group chats, and
communities, and signs in with another device. Files, presence, and backups
come next.
`

// devServers collects --dev-server flags.
type devServers map[string]string

func (d devServers) String() string { return "" }

func (d devServers) Set(s string) error {
	name, url, ok := strings.Cut(s, "=")
	if !ok || name == "" || !(strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
		return errors.New("use --dev-server NAME=URL, such as a.test=http://127.0.0.1:8080")
	}
	d[strings.ToLower(name)] = strings.TrimSuffix(url, "/")
	return nil
}

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
	keyFile := fs.Bool("key-file", false, "")
	logFile := fs.String("log", "", "")
	showVersion := fs.Bool("version", false, "")
	servers := devServers{}
	fs.Var(servers, "dev-server", "")
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

	var backend ui.Backend
	if *demo {
		backend = ui.NewDemoBackend(ui.DemoOptions{Live: true})
	} else {
		var logger *slog.Logger
		if *logFile != "" {
			f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				fmt.Fprintln(stderr, "railcar:", err)
				return 1
			}
			defer f.Close()
			logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
		backend = app.New(app.Options{
			ProfileDir: *profile,
			Logger:     logger,
			Client: client.Options{
				DeviceName: deviceName(),
				Origins:    servers,
				Store:      store.Options{AllowKeyFile: *keyFile},
			},
		})
	}
	defer backend.Close()
	a := ui.New(ui.Options{Backend: backend, Demo: *demo})
	if _, err := tea.NewProgram(a).Run(); err != nil {
		fmt.Fprintln(stderr, "railcar:", err)
		return 1
	}
	return 0
}

// deviceName names this device in the identity's chain, such as "Railcar
// on kat-laptop".
func deviceName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "Railcar"
	}
	host, _, _ = strings.Cut(host, ".")
	return "Railcar on " + host
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
