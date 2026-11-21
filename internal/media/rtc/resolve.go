package rtc

import (
	"context"
	"errors"
	"net"
	"strconv"
	"time"

	transport "github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
)

// Resolver looks up a host's addresses, such as over DNS over HTTPS
// through a proxy, so that WebRTC makes no DNS lookups of its own.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// lookupNet is the system's network for WebRTC, with every hostname looked
// up by its Resolver. TLS to a turns: relay still checks the relay's name,
// since only the address is looked up.
type lookupNet struct {
	transport.Net
	resolve Resolver
}

func newLookupNet(r Resolver) (transport.Net, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &lookupNet{Net: base, resolve: r}, nil
}

// lookup gives an address with its host looked up, if it's a name.
func (n *lookupNet) lookup(address string) (net.IP, int, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, 0, err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, 0, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip, p, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ips, err := n.resolve(ctx, host)
	if err != nil {
		return nil, 0, err
	}
	if len(ips) == 0 {
		return nil, 0, errors.New("rtc: no addresses for " + host)
	}
	return ips[0], p, nil
}

func (n *lookupNet) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	ip, port, err := n.lookup(address)
	if err != nil {
		return nil, err
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

func (n *lookupNet) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	ip, port, err := n.lookup(address)
	if err != nil {
		return nil, err
	}
	return &net.TCPAddr{IP: ip, Port: port}, nil
}

func (n *lookupNet) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	if ip := net.ParseIP(address); ip != nil {
		return &net.IPAddr{IP: ip}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ips, err := n.resolve(ctx, address)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("rtc: no addresses for " + address)
	}
	return &net.IPAddr{IP: ips[0]}, nil
}

// address gives an address with its host replaced by a looked-up one.
func (n *lookupNet) address(address string) (string, error) {
	ip, port, err := n.lookup(address)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func (n *lookupNet) Dial(network, address string) (net.Conn, error) {
	a, err := n.address(address)
	if err != nil {
		return nil, err
	}
	return n.Net.Dial(network, a)
}

func (n *lookupNet) CreateDialer(d *net.Dialer) transport.Dialer {
	return lookupDialer{n: n, d: n.Net.CreateDialer(d)}
}

type lookupDialer struct {
	n *lookupNet
	d transport.Dialer
}

func (d lookupDialer) Dial(network, address string) (net.Conn, error) {
	a, err := d.n.address(address)
	if err != nil {
		return nil, err
	}
	return d.d.Dial(network, a)
}
