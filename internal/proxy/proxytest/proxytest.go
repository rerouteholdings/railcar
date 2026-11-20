// Package proxytest runs small SOCKS5 and HTTP CONNECT proxies in the test
// process, for checking that connections go through a proxy. They resolve
// hostnames from their own table, so a name the machine can't resolve
// still works through them, which shows the client made no lookup itself.
package proxytest

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// Proxy is a running test proxy.
type Proxy struct {
	// URL is the proxy's URL, for proxy.Parse.
	URL string
	ln  net.Listener

	mu      sync.Mutex
	hosts   map[string]string // hostname to IP address
	asked   []string          // each target as the client asked for it
	byName  int               // targets asked for by hostname, not address
	user    string
	pass    string
	refused int // connections refused for wrong credentials
	conns   map[net.Conn]bool
}

// Asked lists the targets clients asked the proxy for, as host:port.
func (p *Proxy) Asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// ByName counts the targets asked for by hostname, which the proxy
// resolved.
func (p *Proxy) ByName() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byName
}

// Refused counts connections refused for wrong credentials.
func (p *Proxy) Refused() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused
}

// Close stops the proxy, and cuts the connections it's carrying, as a
// proxy that goes away does.
func (p *Proxy) Close() {
	p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		c.Close()
	}
}

// track keeps a connection to cut on Close, or forgets it.
func (p *Proxy) track(c net.Conn, on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
}

func (p *Proxy) target(host string, port int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, net.JoinHostPort(host, strconv.Itoa(port)))
	if ip, ok := p.hosts[host]; ok {
		p.byName++
		host = ip
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func start(t *testing.T, scheme, user, pass string, hosts map[string]string, serve func(*Proxy, net.Conn)) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ln: ln, hosts: hosts, user: user, pass: pass, conns: map[net.Conn]bool{}}
	auth := ""
	if user != "" {
		auth = user + ":" + pass + "@"
	}
	p.URL = scheme + "://" + auth + ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.track(c, true)
			go func() {
				defer p.track(c, false)
				serve(p, c)
			}()
		}
	}()
	t.Cleanup(p.Close)
	return p
}

// SOCKS5 starts a SOCKS5 proxy (RFC 1928). With a user, it requires
// username and password authentication (RFC 1929). hosts resolves the
// hostnames clients send.
func SOCKS5(t *testing.T, user, pass string, hosts map[string]string) *Proxy {
	return start(t, "socks5", user, pass, hosts, serveSOCKS5)
}

func serveSOCKS5(p *Proxy, c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(r, methods); err != nil {
		return
	}
	want := byte(0)
	if p.user != "" {
		want = 2
	}
	c.Write([]byte{5, want})
	if want == 2 {
		var v [2]byte
		if _, err := io.ReadFull(r, v[:]); err != nil {
			return
		}
		u := make([]byte, v[1])
		io.ReadFull(r, u)
		var pl [1]byte
		io.ReadFull(r, pl[:])
		pw := make([]byte, pl[0])
		io.ReadFull(r, pw)
		if string(u) != p.user || string(pw) != p.pass {
			p.mu.Lock()
			p.refused++
			p.mu.Unlock()
			c.Write([]byte{1, 1})
			return
		}
		c.Write([]byte{1, 0})
	}
	var req [4]byte
	if _, err := io.ReadFull(r, req[:]); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		var ip [4]byte
		io.ReadFull(r, ip[:])
		host = net.IP(ip[:]).String()
	case 3:
		var n [1]byte
		io.ReadFull(r, n[:])
		name := make([]byte, n[0])
		io.ReadFull(r, name)
		host = string(name)
	case 4:
		var ip [16]byte
		io.ReadFull(r, ip[:])
		host = net.IP(ip[:]).String()
	default:
		return
	}
	var port [2]byte
	io.ReadFull(r, port[:])
	up, err := net.Dial("tcp", p.target(host, int(binary.BigEndian.Uint16(port[:]))))
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	p.track(up, true)
	defer p.track(up, false)
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	pipe(up, c, r)
}

// HTTP starts an HTTP proxy that takes CONNECT only. With a user, it
// requires Basic proxy authentication.
func HTTP(t *testing.T, user, pass string, hosts map[string]string) *Proxy {
	return start(t, "http", user, pass, hosts, serveHTTP)
}

func serveHTTP(p *Proxy, c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	req, err := http.ReadRequest(r)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}
	if p.user != "" {
		u, pw, ok := (&http.Request{Header: http.Header{"Authorization": req.Header["Proxy-Authorization"]}}).BasicAuth()
		if !ok || u != p.user || pw != p.pass {
			p.mu.Lock()
			p.refused++
			p.mu.Unlock()
			io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
			return
		}
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil {
		return
	}
	n, _ := strconv.Atoi(port)
	up, err := net.Dial("tcp", p.target(host, n))
	if err != nil {
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer up.Close()
	p.track(up, true)
	defer p.track(up, false)
	io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
	pipe(up, c, r)
}

func pipe(up, c net.Conn, r *bufio.Reader) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(up, r); done <- struct{}{} }()
	go func() { io.Copy(c, up); done <- struct{}{} }()
	<-done
}
