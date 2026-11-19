package client

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// The end-to-end test runs against a real rerouted. It's off by default so
// go test stays offline. To run it, build rerouted and point
// RAILCAR_REROUTED at the binary:
//
//	(cd ../rerouted && go build -o /tmp/rerouted ./cmd/rerouted)
//	RAILCAR_REROUTED=/tmp/rerouted go test ./internal/tieroute/client -run E2E -v

const signupCode = "railcar-e2e-signup"

// standIn makes stand-in key packages: random bytes, since the homeserver
// never parses them (§2.3). The MLS layer replaces it.
type standIn struct{}

func (standIn) KeyPackages(n int, lastResort bool) ([][]byte, error) {
	out := make([][]byte, n)
	for i := range out {
		out[i] = make([]byte, 64)
		rand.Read(out[i])
	}
	return out, nil
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startRerouted runs a rerouted named name in dev mode on a free port, and
// returns its origin.
func startRerouted(t *testing.T, bin, name string) string {
	t.Helper()
	port := freePort(t)
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	conf := fmt.Sprintf("REROUTED_NAME=%s\nREROUTED_HANDLES=%s\nREROUTED_LISTEN=127.0.0.1:%d\nREROUTED_PUBLIC_URL=%s\nREROUTED_DEV=on\nREROUTED_STORE=temp\nREROUTED_DATA_DIR=%s\nREROUTED_REGISTRATION=invite\nREROUTED_REGISTRATION_CODE=%s\n",
		name, name, port, origin, filepath.Join(dir, "data"), signupCode)
	if err := os.WriteFile(env, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-env", env)
	logf, _ := os.Create(filepath.Join(dir, "log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(dir, "log"))
			t.Logf("rerouted log:\n%s", b)
		}
	})
	for range 100 {
		if resp, err := http.Get(origin + "/.well-known/tieroute/server"); err == nil {
			resp.Body.Close()
			return origin
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("rerouted didn't start")
	return ""
}

func waitFor(t *testing.T, events <-chan Event, what string, match func(Event) bool) Event {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case e, ok := <-events:
			if !ok {
				t.Fatalf("stream closed waiting for %s", what)
			}
			t.Logf("stream: %s", e.Kind)
			if e.Kind == EventStopped {
				t.Fatalf("stream stopped waiting for %s: %v", what, e.Err)
			}
			if match(e) {
				return e
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestE2E(t *testing.T) {
	bin := os.Getenv("RAILCAR_REROUTED")
	if bin == "" {
		t.Skip("set RAILCAR_REROUTED to a rerouted binary to run")
	}
	keyring.MockInit()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	origin := startRerouted(t, bin, "a.test")
	opts := Options{Origins: map[string]string{"a.test": origin}, DeviceName: "Kat's laptop"}
	dir := filepath.Join(t.TempDir(), "kat")

	// Sign up: a wrong code is refused, and the same signup then works.
	s, err := NewSignup(ctx, "a.test", opts)
	if err != nil {
		t.Fatal(err)
	}
	words := s.Words()
	if len(words.Words()) != 12 {
		t.Fatal("no recovery words")
	}
	if _, err := s.Register(ctx, dir, "kat", "wrong-code-here"); !wire.IsCode(err, wire.CodeBadInvite) {
		t.Fatalf("a wrong code: %v", err)
	}
	c, err := s.Register(ctx, dir, "kat", signupCode)
	if err != nil {
		t.Fatal(err)
	}
	if c.Handle() != "kat.a.test" || !c.Session().SignedIn {
		t.Fatalf("handle %q, session %+v", c.Handle(), c.Session())
	}
	if skew, ok := c.Clock().Skew(); !ok || skew > time.Minute || skew < -time.Minute {
		t.Errorf("clock skew %v %v", skew, ok)
	}
	t.Logf("registered %s as @%s", c.ID(), c.Handle())

	// The recovery words find recovery key 1 in the chain (§6.1).
	rec, err := identity.RecoveryFromWords(words.Words())
	if err != nil {
		t.Fatal(err)
	}
	if i, err := rec.Find(&c.Chain().State); err != nil || i != 1 {
		t.Fatalf("recovery key %d, %v", i, err)
	}

	// Key packages with their bindings (§3.2.2).
	n, err := c.UploadKeyPackages(ctx, standIn{}, 5, UploadOptions{LastResort: true})
	if err != nil {
		t.Fatalf("upload key packages: %v", err)
	}
	if cnt, lr, err := c.KeyPackageCount(ctx); err != nil || cnt != 5 || n != 5 || !lr {
		t.Errorf("key packages: %d %d %v %v", n, cnt, lr, err)
	}

	// Open the profile again, as a restart does, and sign in afresh.
	c2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.SignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if c2.ID() != c.ID() || c2.Chain().State.Head != c.Chain().State.Head {
		t.Fatal("the reopened profile differs")
	}

	// The handle resolves both ways (§5.3).
	r, err := c2.ResolveHandle(ctx, "@kat.a.test")
	if err != nil || r.ID != c.ID() {
		t.Fatalf("resolve: %+v %v", r, err)
	}

	// The live stream: a link delivery to the identity's own inbox, and a
	// chain-alert notice from adding a device.
	sctx, stopStream := context.WithCancel(ctx)
	events := c2.Stream(sctx, StreamOptions{})
	waitFor(t, events, "ready", func(e Event) bool { return e.Kind == EventConnected })

	if res, err := c2.SendLink(ctx, []byte("a stand-in link bundle")); err != nil || res.Status != "sent" {
		t.Fatalf("send link: %+v %v", res, err)
	}
	e := waitFor(t, events, "the link delivery", func(e Event) bool {
		return e.Kind == EventItem && e.Item.Delivery != nil && e.Item.Delivery.Kind == "link"
	})
	if string(e.Item.Delivery.Data) != "a stand-in link bundle" || e.Item.Delivery.From != c.ID() {
		t.Errorf("delivery %+v", e.Item.Delivery)
	}

	dev2, _ := identity.GenerateDevice("Kat's desktop")
	if _, err := c2.Append(ctx, identity.AddDevice(dev2.Key.Public(), dev2.Name)); err != nil {
		t.Fatalf("append: %v", err)
	}
	e = waitFor(t, events, "the chain-alert", func(e Event) bool {
		return e.Kind == EventItem && e.Item.Notice != nil && e.Item.Notice.T == NoticeChainAlert
	})
	if e.Item.Notice.Level != "notice" {
		t.Errorf("chain-alert level %q", e.Item.Notice.Level)
	}
	if err := c2.Ack(ctx, e.Item.Cursor); err != nil {
		t.Fatal(err)
	}
	stopStream()
	for range events {
	}

	// The inbox after the ack is empty for this device.
	page, err := c2.Inbox(ctx, c2.Cursor(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Errorf("%d items after the ack", len(page.Items))
	}

	// A second identity on the same server fetches Kat's chain.
	ravi, _, err := Register(ctx, "a.test", "ravi", signupCode, filepath.Join(t.TempDir(), "ravi"), Options{Origins: opts.Origins})
	if err != nil {
		t.Fatal(err)
	}
	kc, err := ravi.FetchChain(ctx, c.ID(), FetchOptions{Home: "a.test"})
	if err != nil || len(kc.State.Devices) != 2 {
		t.Fatalf("ravi fetches kat's chain: %v", err)
	}

	// Signing out revokes the token on the server.
	old := c2.token()
	if err := c2.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if c2.Session().SignedIn {
		t.Error("still signed in")
	}
	a := *c2.api
	a.token = func() string { return old }
	if err := a.call(ctx, http.MethodGet, "keypackages/count", nil, nil, nil); !wire.IsCode(err, wire.CodeUnauthenticated) {
		t.Errorf("the old token after signing out: %v", err)
	}
}
