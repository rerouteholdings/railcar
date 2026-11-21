package client

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/rerouteholdings/railcar/internal/proxy"
)

// TestProxyOptions checks that the client's HTTP traffic follows its proxy
// setting, fails closed with a dead proxy, and that discovery makes no DNS
// lookups of its own through a proxy.
func TestProxyOptions(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	px, err := proxy.Parse("socks5://" + dead)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Proxy: px}
	if !o.resolver().NoDNS || (Options{}).resolver().NoDNS {
		t.Error("NoDNS doesn't follow the proxy")
	}
	_, err = o.resolver().ServerDoc(context.Background(), "chat.example.com")
	var u *proxy.Unreachable
	if !errors.As(err, &u) {
		t.Errorf("a server document through a dead proxy: %v", err)
	}
	if o.resolver().TXT == nil || (Options{}).resolver().TXT != nil {
		t.Error("DNS over HTTPS doesn't follow the proxy")
	}
	// The DNS fallback goes over HTTPS through the proxy too, so it fails
	// at the dead proxy rather than asking the system's resolver.
	_, err = o.resolver().LookupHandle(context.Background(), "kat.example.com")
	if !errors.As(err, &u) {
		t.Errorf("a handle lookup through a dead proxy: %v", err)
	}
	if _, err := (&Client{opts: o}).LookupIP(context.Background(), "turn.example.net"); !errors.As(err, &u) {
		t.Errorf("a relay lookup through a dead proxy: %v", err)
	}
}
