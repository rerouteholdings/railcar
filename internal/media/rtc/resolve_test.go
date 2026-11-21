package rtc

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestLookupNet(t *testing.T) {
	var asked []string
	n, err := newLookupNet(func(_ context.Context, host string) ([]net.IP, error) {
		asked = append(asked, host)
		if host == "turn.example.net" {
			return []net.IP{net.IPv4(192, 0, 2, 7)}, nil
		}
		return nil, errors.New("no such host")
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := n.ResolveUDPAddr("udp4", "turn.example.net:3478")
	if err != nil || u.String() != "192.0.2.7:3478" {
		t.Fatalf("UDP: %v, %v", u, err)
	}
	tc, err := n.ResolveTCPAddr("tcp", "turn.example.net:443")
	if err != nil || tc.String() != "192.0.2.7:443" {
		t.Fatalf("TCP: %v, %v", tc, err)
	}
	if _, err := n.ResolveUDPAddr("udp4", "other.example.net:3478"); err == nil {
		t.Fatal("an unknown name resolved")
	}
	asked = nil
	if u, err := n.ResolveUDPAddr("udp4", "198.51.100.4:3478"); err != nil || u.Port != 3478 || len(asked) != 0 {
		t.Fatalf("an address was looked up: %v, %v, %q", u, err, asked)
	}
}
