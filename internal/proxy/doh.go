package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DoHURL is the DNS over HTTPS resolver (RFC 8484) that lookups go to
// while a proxy is set, through the proxy: Cloudflare's. The machine then
// makes no DNS lookups of its own. Tests point it elsewhere.
var DoHURL = "https://cloudflare-dns.com/dns-query"

// dohClient is the HTTP client for DNS over HTTPS. Tests replace it.
var dohClient = func(c Config) *http.Client { return c.HTTPClient(15 * time.Second) }

// LookupTXT looks up a name's TXT records over DNS over HTTPS, through the
// proxy. Each record's strings come joined, as Go's resolver gives them.
func (c Config) LookupTXT(ctx context.Context, name string) ([]string, error) {
	answers, err := c.query(ctx, name, dnsmessage.TypeTXT)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range answers {
		if t, ok := a.Body.(*dnsmessage.TXTResource); ok {
			out = append(out, strings.Join(t.TXT, ""))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("proxy: no TXT records for %s", name)
	}
	return out, nil
}

// LookupIP looks up a host's addresses over DNS over HTTPS, through the
// proxy: IPv4 first, then IPv6. An address is returned as it is.
func (c Config) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	var out []net.IP
	var firstErr error
	for _, t := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		answers, err := c.query(ctx, host, t)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, a := range answers {
			switch b := a.Body.(type) {
			case *dnsmessage.AResource:
				out = append(out, net.IP(b.A[:]))
			case *dnsmessage.AAAAResource:
				out = append(out, net.IP(b.AAAA[:]))
			}
		}
	}
	if len(out) == 0 {
		if firstErr == nil {
			firstErr = fmt.Errorf("proxy: no addresses for %s", host)
		}
		return nil, firstErr
	}
	return out, nil
}

// query asks the DNS over HTTPS resolver one question, with POST and the
// DNS wire format (RFC 8484).
func (c Config) query(ctx context.Context, name string, t dnsmessage.Type) ([]dnsmessage.Resource, error) {
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, fmt.Errorf("proxy: %q isn't a DNS name", name)
	}
	q := dnsmessage.Message{
		Header:    dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: n, Type: t, Class: dnsmessage.ClassINET}},
	}
	body, err := q.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DoHURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := dohClient(c).Do(req)
	if err != nil {
		return nil, fmt.Errorf("proxy: DNS over HTTPS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy: DNS over HTTPS answered %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var m dnsmessage.Message
	if err := m.Unpack(raw); err != nil {
		return nil, fmt.Errorf("proxy: a DNS over HTTPS answer that doesn't parse: %w", err)
	}
	if m.Header.RCode != dnsmessage.RCodeSuccess {
		return nil, fmt.Errorf("proxy: DNS over HTTPS: %s for %s", m.Header.RCode, name)
	}
	return m.Answers, nil
}
