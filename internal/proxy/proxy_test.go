package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/proxy/proxytest"
)

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Config
		ok   bool
	}{
		{"", Config{}, true},
		{"none", Config{}, true},
		{"socks5://127.0.0.1:9050", Config{Kind: SOCKS5, Host: "127.0.0.1", Port: 9050}, true},
		{"socks5h://proxy.example.com:1080", Config{Kind: SOCKS5, Host: "proxy.example.com", Port: 1080}, true},
		{"socks5://kat:p%40ss@[::1]:1080", Config{Kind: SOCKS5, Host: "::1", Port: 1080, User: "kat", Password: "p@ss"}, true},
		{"http://proxy.example.com:8080", Config{Kind: HTTP, Host: "proxy.example.com", Port: 8080}, true},
		{"http://kat@proxy.example.com:8080/", Config{Kind: HTTP, Host: "proxy.example.com", Port: 8080, User: "kat"}, true},
		{"https://proxy.example.com:8443", Config{}, false},
		{"socks5://proxy.example.com", Config{}, false},
		{"socks5://proxy.example.com:0", Config{}, false},
		{"socks5://proxy.example.com:1080/path", Config{}, false},
		{"proxy.example.com:1080", Config{}, false},
	} {
		got, err := Parse(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v", c.in, got, err)
		}
	}
}

// TestNoPasswordShown checks that a password never reaches String or an
// error.
func TestNoPasswordShown(t *testing.T) {
	c, err := Parse("socks5://kat:hunter2@127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if s := c.String(); strings.Contains(s, "hunter2") || !strings.Contains(s, "kat") {
		t.Errorf("String() = %q", s)
	}
	for _, bad := range []string{"socks5://kat:hunter2@[::1:1080", "socks5://kat:hunter2@host", "ftp://kat:hunter2@host:1", "socks5://kat:hunter2@host:1/x"} {
		if _, err := Parse(bad); err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Parse(%q): %v", bad, err)
		}
	}
	_, err = c.Dial(context.Background(), "tcp", "example.com:443")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a dead proxy's error: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{"http_proxy": "http://127.0.0.1:3128", "ALL_PROXY": "socks5://127.0.0.1:9050"}
	c, err := FromEnv(func(k string) string { return env[k] })
	if err != nil || c.Kind != SOCKS5 || c.Port != 9050 {
		t.Fatalf("ALL_PROXY first: %+v %v", c, err)
	}
	delete(env, "ALL_PROXY")
	if c, _ := FromEnv(func(k string) string { return env[k] }); c.Kind != HTTP {
		t.Fatalf("http_proxy: %+v", c)
	}
}

// TestSettings checks the config file, flags over it, and that the
// environment counts only when asked for.
func TestSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "railcar.conf")
	if s, err := ParseFile(path); err != nil || s.Proxy.Set() {
		t.Fatalf("a missing file: %+v %v", s, err)
	}
	os.WriteFile(path, []byte("# Railcar\n\nproxy = socks5://127.0.0.1:9050\nproxy-calls = on\n"), 0o600)
	file, err := ParseFile(path)
	if err != nil || file.Proxy.Port != 9050 || !file.Calls {
		t.Fatalf("the file: %+v %v", file, err)
	}
	var flags Settings
	flags.Set("proxy", "none")
	flags.Set("proxy-calls", "off")
	got := file.Over(flags)
	if got.Proxy.Set() || got.Calls {
		t.Errorf("flags over the file: %+v", got)
	}
	if file.Over(Settings{}).Proxy.Port != 9050 {
		t.Error("no flags changed the file's proxy")
	}

	env := func(k string) string {
		if k == "ALL_PROXY" {
			return "socks5://127.0.0.1:9999"
		}
		return ""
	}
	if c, _ := (Settings{}).Resolve(env); c.Set() {
		t.Error("the environment counted without proxy-from-env")
	}
	var opt Settings
	opt.Set("proxy-from-env", "on")
	if c, _ := opt.Resolve(env); c.Port != 9999 {
		t.Errorf("proxy-from-env: %+v", c)
	}
	if c, _ := file.Over(opt).Resolve(env); c.Port != 9050 {
		t.Errorf("a set proxy over the environment: %+v", c)
	}

	for _, bad := range []string{"proxy socks5://x:1\n", "colour = red\n", "proxy-calls = maybe\n"} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := ParseFile(path); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// target is an HTTP server on the loopback address, which the test
// proxies know as railcar-target.invalid: a name the machine can't
// resolve.
func target(t *testing.T) (port int, hosts map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through "+r.Host)
	}))
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ = strconv.Atoi(p)
	return port, map[string]string{"railcar-target.invalid": "127.0.0.1"}
}

// TestThroughProxies sends HTTP requests through each kind of proxy, with
// and without credentials. The target's name resolves only at the proxy.
func TestThroughProxies(t *testing.T) {
	port, hosts := target(t)
	url := "http://railcar-target.invalid:" + strconv.Itoa(port) + "/"
	for _, c := range []struct {
		name string
		p    *proxytest.Proxy
	}{
		{"socks5", proxytest.SOCKS5(t, "", "", hosts)},
		{"socks5 with a password", proxytest.SOCKS5(t, "kat", "hunter2", hosts)},
		{"http", proxytest.HTTP(t, "", "", hosts)},
		{"http with a password", proxytest.HTTP(t, "kat", "hunter2", hosts)},
	} {
		cfg, err := Parse(c.p.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := cfg.HTTPClient(0).Get(url)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "through railcar-target.invalid:"+strconv.Itoa(port) {
			t.Errorf("%s: %q", c.name, body)
		}
		if c.p.ByName() != 1 || len(c.p.Asked()) != 1 || c.p.Asked()[0] != "railcar-target.invalid:"+strconv.Itoa(port) {
			t.Errorf("%s: the proxy was asked for %v, %d by name", c.name, c.p.Asked(), c.p.ByName())
		}
	}

	// Wrong credentials fail, and nothing goes around the proxy.
	p := proxytest.SOCKS5(t, "kat", "hunter2", hosts)
	cfg, _ := Parse(strings.Replace(p.URL, "hunter2", "wrong", 1))
	if _, err := cfg.HTTPClient(0).Get(url); err == nil || p.Refused() != 1 {
		t.Errorf("wrong credentials: %v, %d refused", err, p.Refused())
	}
}

// TestDeadProxyFailsClosed checks that a proxy that can't be reached fails
// every connection, with no direct one instead.
func TestDeadProxyFailsClosed(t *testing.T) {
	port, _ := target(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	for _, kind := range []string{"socks5", "http"} {
		cfg, _ := Parse(kind + "://" + dead)
		_, err := cfg.HTTPClient(0).Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
		var u *Unreachable
		if !errors.As(err, &u) {
			t.Errorf("%s: %v, not unreachable", kind, err)
		}
	}
}

// TestNoEnvironment checks that without a proxy, connections are direct
// even with the environment's proxy variables set.
func TestNoEnvironment(t *testing.T) {
	port, _ := target(t)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	resp, err := (Config{}).HTTPClient(0).Get("http://127.0.0.1:" + strconv.Itoa(port) + "/")
	if err != nil {
		t.Fatalf("a direct connection with proxy variables set: %v", err)
	}
	resp.Body.Close()
}
