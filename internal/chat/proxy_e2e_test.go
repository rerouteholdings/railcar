package chat

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/proxy"
	"github.com/rerouteholdings/railcar/internal/proxy/proxytest"
	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// newRailcarVia is newRailcarWith, with every connection through a proxy.
func newRailcarVia(t *testing.T, origins map[string]string, server, handle string, px proxy.Config, opts Options) *railcar {
	t.Helper()
	ctx := context.Background()
	c, _, err := client.Register(ctx, server, handle, testnet.SignupCode, filepath.Join(t.TempDir(), handle), client.Options{Origins: origins, Proxy: px})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("RAILCAR_E2E_LOGS") != "" {
		opts.Logger = slog.New(slog.NewTextHandler(testWriter{t, handle}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	e, err := New(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if err := e.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	return &railcar{t: t, e: e}
}

// TestE2ERailcarProxy runs a Railcar identity through a SOCKS5 proxy, and
// another through an HTTP CONNECT proxy, with a third connecting directly.
// Everything reaches the homeservers through the proxies, the live stream
// too; calls are off, and a ring shows as missed; and a dead proxy fails
// every request, with no direct connection instead.
func TestE2ERailcarProxy(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	socks := proxytest.SOCKS5(t, "kat", "hunter2", nil)
	httpp := proxytest.HTTP(t, "", "", nil)
	sp, _ := proxy.Parse(socks.URL)
	hp, _ := proxy.Parse(httpp.URL)
	kat := newRailcarVia(t, o, "a.test", "kat", sp, Options{})
	lee := newRailcarVia(t, o, "b.test", "lee", hp, Options{})
	ravi := newCaller(t, ctx, o, "b.test", "ravi", 660)
	befriendRailcars(t, kat, ravi.railcar, "ravi.b.test")
	befriendRailcars(t, lee, ravi.railcar, "ravi.b.test")
	ch := kat.chatWith("ravi.b.test")
	if _, err := kat.e.SendText(ctx, ch.ID, "through a proxy", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, ch.ID, "heard you", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kat.find(ch.ID, "heard you")
	ravi.find(ch.ID, "through a proxy")
	lch := lee.chatWith("ravi.b.test")
	if _, err := lee.e.SendText(ctx, lch.ID, "through an HTTP proxy", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.find(lch.ID, "through an HTTP proxy")

	// Every target the proxies were asked for is one of the servers.
	for _, p := range []*proxytest.Proxy{socks, httpp} {
		if len(p.Asked()) == 0 {
			t.Fatal("a proxy carried nothing")
		}
		for _, a := range p.Asked() {
			ok := false
			for _, origin := range o {
				ok = ok || strings.HasSuffix(origin, "//"+a)
			}
			if !ok {
				t.Errorf("a proxy was asked for %s, not a server", a)
			}
		}
	}

	// The live stream goes through the proxy too.
	before := len(socks.Asked())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- kat.e.Run(runCtx) }()
	waitEvent(t, kat.e, "the stream", func(ev Event) bool { return ev.Kind == EventStream && ev.Stream == client.EventConnected })
	if len(socks.Asked()) <= before {
		t.Error("the stream didn't go through the proxy")
	}

	// Calls are off: Kat can't call, and Ravi's ring is a missed call.
	if err := kat.e.StartCall(ctx, ch.ID); !errors.Is(err, ErrCallsOff) {
		t.Errorf("a call through a proxy: %v", err)
	}
	if err := ravi.e.StartCall(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the missed call", func() bool {
		return hasLine(t, kat.e, ch.ID, "Missed call from @ravi.b.test, since calls are off")
	})
	if phase(kat.e) == CallRinging {
		t.Error("Kat's device rang")
	}
	if err := kat.e.AnswerCall(ctx, ch.ID); !errors.Is(err, ErrCallsOff) {
		t.Errorf("answering through a proxy: %v", err)
	}
	ravi.e.HangUp(ctx)
	cancel()
	<-done

	// A dead proxy: nothing works, and nothing goes direct.
	socks.Close()
	time.Sleep(100 * time.Millisecond)
	err := kat.e.Sync(ctx)
	var u *proxy.Unreachable
	if !errors.As(err, &u) {
		t.Fatalf("a sync through a dead proxy: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Error("the error shows the proxy's password")
	}
}
