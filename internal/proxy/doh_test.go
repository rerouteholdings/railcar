package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// dohServer answers DNS over HTTPS questions from a table, and counts them.
func dohServer(t *testing.T, txt map[string][]string, a map[string][4]byte) (*httptest.Server, *int) {
	t.Helper()
	n := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*n++
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var q dnsmessage.Message
		if err := q.Unpack(raw); err != nil || len(q.Questions) != 1 {
			http.Error(w, "bad message", http.StatusBadRequest)
			return
		}
		qq := q.Questions[0]
		name := qq.Name.String()
		m := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RCode: dnsmessage.RCodeSuccess}, Questions: q.Questions}
		h := dnsmessage.ResourceHeader{Name: qq.Name, Type: qq.Type, Class: dnsmessage.ClassINET, TTL: 60}
		switch qq.Type {
		case dnsmessage.TypeTXT:
			v, ok := txt[name]
			if !ok {
				m.Header.RCode = dnsmessage.RCodeNameError
			}
			for _, s := range v {
				m.Answers = append(m.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.TXTResource{TXT: []string{s[:len(s)/2], s[len(s)/2:]}}})
			}
		case dnsmessage.TypeA:
			if ip, ok := a[name]; ok {
				m.Answers = append(m.Answers, dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: ip}})
			}
		}
		out, err := m.Pack()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	oldURL, oldClient := DoHURL, dohClient
	DoHURL = srv.URL
	dohClient = func(Config) *http.Client { return srv.Client() }
	t.Cleanup(func() { DoHURL, dohClient = oldURL, oldClient })
	return srv, n
}

func TestDoHLookups(t *testing.T) {
	_, n := dohServer(t,
		map[string][]string{"_tieroute.kat.example.com.": {"v=tr1 server=example.net"}},
		map[string][4]byte{"turn.example.net.": {192, 0, 2, 7}})
	c := Config{Kind: SOCKS5, Host: "127.0.0.1", Port: 1080}
	ctx := context.Background()

	txt, err := c.LookupTXT(ctx, "_tieroute.kat.example.com")
	if err != nil || len(txt) != 1 || txt[0] != "v=tr1 server=example.net" {
		t.Fatalf("TXT: %q, %v", txt, err)
	}
	if _, err := c.LookupTXT(ctx, "_tieroute.ravi.example.com"); err == nil {
		t.Fatal("a missing name gave TXT records")
	}

	ips, err := c.LookupIP(ctx, "turn.example.net")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.IPv4(192, 0, 2, 7)) {
		t.Fatalf("A: %v, %v", ips, err)
	}
	if _, err := c.LookupIP(ctx, "none.example.net"); err == nil {
		t.Fatal("a name with no addresses gave some")
	}

	before := *n
	ips, err = c.LookupIP(ctx, "198.51.100.4")
	if err != nil || !ips[0].Equal(net.ParseIP("198.51.100.4")) || *n != before {
		t.Fatalf("an address was looked up: %v, %v", ips, err)
	}
}
