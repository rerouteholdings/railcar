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
	"github.com/rerouteholdings/railcar/internal/proxy"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "0.0.0-dev"

const usage = `Railcar is a terminal client for Tieroute, a federated, end-to-end
encrypted (E2EE) chat protocol.

Usage:
  railcar [--profile DIR] [--key-file] [--log FILE] [--proxy URL] [--proxy-calls]
          [--config FILE] [--dev-server NAME=URL]...
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
  --proxy URL            Send every connection through a proxy, which also looks
                         up hostnames: socks5://host:port or http://host:port,
                         with user:password@ before the host if it needs them,
                         or none. If the proxy can't be reached, Railcar doesn't
                         connect at all. Calls and voice channels are off with
                         a proxy, since they're UDP, which a proxy doesn't carry.
  --proxy-calls          With a proxy, allow calls and voice channels anyway,
                         only through your homeserver's relay. The relay sees
                         your real IP address. Its name is still looked up
                         through the proxy.
  --proxy-from-env       Without --proxy, take the proxy from ALL_PROXY,
                         HTTPS_PROXY, or HTTP_PROXY. Railcar ignores them
                         otherwise.
  --config FILE          Read settings from FILE, one key = value a line: proxy,
                         proxy-calls, and proxy-from-env, as the options above.
                         The default is railcar.conf in the profile directory.
                         Options on the command line win.
  --dev-server NAME=URL  Reach the server NAME at URL, over plain HTTP if the URL
                         says so. It's for local test networks, such as rerouted
                         in its development mode. Give it once for each server.
  --demo                 Run with made-up chats and no network, to try the
                         interface. Nothing is saved or sent.
  --version              Print the version and exit.
  -h, --help             Show this help.

Railcar is in early development: it has direct chats, group chats, and
communities, files, presence, read receipts, backups, moves to another
homeserver, calls, and voice channels, and signs in with another device.
Type :call in a chat to call; Ctrl+Y answers, Ctrl+R mutes, and Ctrl+X
hangs up. :audio-devices chooses the microphone and speakers.
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
	var flagged proxy.Settings
	proxyURL := fs.String("proxy", "", "")
	fs.BoolFunc("proxy-calls", "", func(v string) error { return flagged.Set("proxy-calls", onOff(v)) })
	fs.BoolFunc("proxy-from-env", "", func(v string) error { return flagged.Set("proxy-from-env", onOff(v)) })
	configFile := fs.String("config", "", "")
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

	// The proxy URL is parsed here rather than by the flag package, which
	// would print a bad value, password and all.
	proxyGiven := false
	fs.Visit(func(f *flag.Flag) { proxyGiven = proxyGiven || f.Name == "proxy" })
	if proxyGiven {
		if err := flagged.Set("proxy", *proxyURL); err != nil {
			fmt.Fprintln(stderr, "railcar:", err)
			return 2
		}
	}
	if *configFile == "" {
		*configFile = filepath.Join(*profile, "railcar.conf")
	}
	settings, err := proxy.ParseFile(*configFile)
	if err != nil {
		fmt.Fprintln(stderr, "railcar:", err)
		return 2
	}
	settings = settings.Over(flagged)
	px, err := settings.Resolve(os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "railcar:", err)
		return 2
	}
	warnReadable(stderr, *configFile, settings)

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
		if logger != nil {
			logger.Info("railcar: starting", "proxy", px.String(), "proxy_calls", settings.Calls)
		}
		backend = app.New(app.Options{
			ProfileDir: *profile,
			Logger:     logger,
			Client: client.Options{
				DeviceName: deviceName(),
				Origins:    servers,
				Store:      store.Options{AllowKeyFile: *keyFile},
				Proxy:      px,
			},
			ProxyCalls: settings.Calls,
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

// onOff turns a boolean flag's value into a setting's.
func onOff(v string) string {
	if v == "false" {
		return "off"
	}
	return "on"
}

// warnReadable warns when a config file holding a proxy password can be
// read by other users.
func warnReadable(w io.Writer, path string, s proxy.Settings) {
	fi, err := os.Stat(path)
	if err != nil || s.Proxy.Password == "" || fi.Mode().Perm()&0o077 == 0 {
		return
	}
	fmt.Fprintf(w, "railcar: %s holds a proxy password and other users can read it. Run chmod 600 on it.\n", path)
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
