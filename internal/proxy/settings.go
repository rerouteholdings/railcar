package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Settings are Railcar's proxy settings, from the config file and the
// command line.
type Settings struct {
	// Proxy is the proxy; the zero value is none.
	Proxy Config
	// FromEnv takes the proxy from ALL_PROXY, HTTPS_PROXY, or HTTP_PROXY
	// when none is set otherwise. Off unless the user turns it on.
	FromEnv bool
	// Calls allows calls and voice channels while a proxy is set, through
	// the homeserver's relay only.
	Calls bool
	// set notes which settings the file or flags gave, for precedence.
	proxySet, fromEnvSet, callsSet bool
}

// ParseFile reads a config file: one setting a line, as key = value, with
// blank lines and lines starting with # ignored. The keys are proxy (a
// proxy URL, or none), proxy-from-env, and proxy-calls (on or off). A
// missing file is no settings.
func ParseFile(path string) (Settings, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	defer f.Close()
	var s Settings
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Settings{}, fmt.Errorf("%s:%d: a line that isn't key = value", path, n)
		}
		if err := s.Set(strings.TrimSpace(k), strings.TrimSpace(v)); err != nil {
			return Settings{}, fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	return s, sc.Err()
}

// Set sets one setting by its key, as in the config file.
func (s *Settings) Set(key, value string) error {
	switch key {
	case "proxy":
		c, err := Parse(value)
		if err != nil {
			return err
		}
		s.Proxy, s.proxySet = c, true
	case "proxy-from-env", "proxy-calls":
		var on bool
		switch strings.ToLower(value) {
		case "on", "true", "yes":
			on = true
		case "off", "false", "no":
		default:
			return fmt.Errorf("%s is on or off, not %q", key, value)
		}
		if key == "proxy-calls" {
			s.Calls, s.callsSet = on, true
		} else {
			s.FromEnv, s.fromEnvSet = on, true
		}
	default:
		return fmt.Errorf("an unknown setting %q", key)
	}
	return nil
}

// Over returns these settings, with every setting o gives in their place:
// the command line over the config file.
func (s Settings) Over(o Settings) Settings {
	if o.proxySet {
		s.Proxy, s.proxySet = o.Proxy, true
	}
	if o.fromEnvSet {
		s.FromEnv, s.fromEnvSet = o.FromEnv, true
	}
	if o.callsSet {
		s.Calls, s.callsSet = o.Calls, true
	}
	return s
}

// Resolve gives the proxy to use: the one set, or with FromEnv and none
// set, the environment's.
func (s Settings) Resolve(getenv func(string) string) (Config, error) {
	if s.proxySet || !s.FromEnv {
		return s.Proxy, nil
	}
	return FromEnv(getenv)
}
