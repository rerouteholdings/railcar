// Package proxy sends Railcar's connections through a SOCKS5 or HTTP
// CONNECT proxy. It's a client setting, not part of Tieroute.
//
// With a proxy set, every TCP connection goes through it, and the proxy
// resolves hostnames, so the machine makes no DNS lookups of its own. If
// the proxy can't be reached, connections fail: nothing falls back to a
// direct connection. Without one, Railcar connects directly, and ignores
// the HTTP_PROXY and ALL_PROXY environment variables unless the user asks
// for them, so a proxy is always deliberate.
package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// The kinds of proxy.
const (
	SOCKS5 = "socks5"
	HTTP   = "http"
)

// Config is a proxy. The zero value is none: direct connections.
type Config struct {
	Kind     string // "", SOCKS5, or HTTP
	Host     string
	Port     int
	User     string
	Password string
}

// Parse reads a proxy URL: socks5://user:pass@host:port or
// http://host:port, with an optional user and password. An empty string
// or "none" is no proxy.
func Parse(s string) (Config, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "none" {
		return Config{}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		// The text isn't shown: it may hold a password.
		return Config{}, errors.New("proxy: that isn't a proxy URL like socks5://host:1080 or http://host:8080")
	}
	var c Config
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		c.Kind = SOCKS5
	case "http":
		c.Kind = HTTP
	default:
		return Config{}, fmt.Errorf("proxy: a %q proxy; Railcar takes socks5:// and http:// proxies", u.Scheme)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		return Config{}, errors.New("proxy: a proxy URL has only a host and port, and optionally a user and password")
	}
	c.Host = u.Hostname()
	port := u.Port()
	if port == "" {
		return Config{}, errors.New("proxy: the proxy URL needs a port, such as socks5://127.0.0.1:9050")
	}
	if c.Port, err = strconv.Atoi(port); err != nil || c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("proxy: %q isn't a port", port)
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	return c, nil
}

// FromEnv reads a proxy from ALL_PROXY, HTTPS_PROXY, or HTTP_PROXY, in that
// order, upper or lower case. Railcar reads them only when the user asks.
func FromEnv(getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	for _, name := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := getenv(name); v != "" {
			c, err := Parse(v)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", name, err)
			}
			return c, nil
		}
	}
	return Config{}, nil
}

// Set reports whether there's a proxy.
func (c Config) Set() bool { return c.Kind != "" }

// Addr is the proxy's host and port.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// String shows the proxy as a URL, with the password hidden, so it can go
// in logs and on screen.
func (c Config) String() string {
	if !c.Set() {
		return "none"
	}
	u := url.URL{Scheme: c.Kind, Host: c.Addr()}
	switch {
	case c.User != "" && c.Password != "":
		u.User = url.UserPassword(c.User, "xxxxx")
	case c.User != "":
		u.User = url.User(c.User)
	}
	return u.String()
}

// dialTimeout bounds reaching the proxy and its answer.
const dialTimeout = 30 * time.Second

// Unreachable reports a proxy that couldn't be reached at all.
type Unreachable struct {
	Proxy string // as String gives it
	Err   error
}

func (u *Unreachable) Error() string {
	return fmt.Sprintf("the proxy at %s can't be reached, and Railcar doesn't connect without it: %v", u.Proxy, u.Err)
}

func (u *Unreachable) Unwrap() error { return u.Err }

// Dial connects to addr, a host and port, through the proxy, which
// resolves the host. Without a proxy it dials directly.
func (c Config) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if !c.Set() {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("proxy: a %s connection, which a proxy doesn't carry", network)
	}
	// Reach the proxy itself first, so its failure says so.
	fwd := &net.Dialer{Timeout: dialTimeout}
	conn, err := fwd.DialContext(ctx, "tcp", c.Addr())
	if err != nil {
		return nil, &Unreachable{Proxy: c.String(), Err: err}
	}
	var out net.Conn
	switch c.Kind {
	case SOCKS5:
		out, err = c.socks(ctx, conn, addr)
	default:
		out, err = c.connect(ctx, conn, addr)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return out, nil
}

// connDialer hands x/net's SOCKS5 client the connection already made to
// the proxy.
type connDialer struct{ conn net.Conn }

func (d connDialer) Dial(string, string) (net.Conn, error) { return d.conn, nil }
func (d connDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return d.conn, nil
}

// socks asks a SOCKS5 proxy for a connection to addr, passing the hostname
// for the proxy to resolve (RFC 1928, address type 3).
func (c Config) socks(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	var auth *xproxy.Auth
	if c.User != "" {
		auth = &xproxy.Auth{User: c.User, Password: c.Password}
	}
	d, err := xproxy.SOCKS5("tcp", c.Addr(), auth, connDialer{conn})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("proxy: the SOCKS5 dialer can't take a context")
	}
	out, err := cd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("proxy: the SOCKS5 proxy at %s didn't connect to %s: %w", c.String(), addr, err)
	}
	return out, nil
}

// connect asks an HTTP proxy for a tunnel to addr with CONNECT, sending the
// hostname for the proxy to resolve.
func (c Config) connect(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(dialTimeout))
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if c.User != "" {
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.User+":"+c.Password)))
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("proxy: asking the HTTP proxy at %s for %s: %w", c.String(), addr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("proxy: the HTTP proxy at %s didn't answer: %w", c.String(), err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy: the HTTP proxy at %s refused %s: %s", c.String(), addr, resp.Status)
	}
	conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

// bufferedConn keeps bytes the proxy sent right after its answer.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// HTTPClient returns an HTTP client whose connections all go through the
// proxy, or directly without one. It never reads the environment's proxy
// variables.
func (c Config) HTTPClient(timeout time.Duration) *http.Client {
	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           c.Dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: t, Timeout: timeout}
}
