package chat

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The recovery tests (§6.3, §6.4.1, client API, Recover an identity).
// They're skipped unless RAILCAR_REROUTED names a rerouted binary, and the
// interop one unless RAILCAR_REROUTE_CLI names a reroute-cli binary.

// recoverRailcar signs in with the recovery words as a new device, and
// starts its chat layer as the app does.
func recoverRailcar(t *testing.T, origins map[string]string, server, handle string, words []string) *railcar {
	t.Helper()
	ctx := context.Background()
	c, err := client.Recover(ctx, server, handle, words, filepath.Join(t.TempDir(), "recovered"), client.Options{Origins: origins, DeviceName: "Recovered"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if err := e.ImportContacts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	return &railcar{t: t, e: e}
}

var wordsLine = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]?\s+)?([a-z]+)\s*$`)

// cliWords finds the 12 recovery words in reroute-cli's output: a line of
// 12 words, or 12 lines of one word each.
func cliWords(t *testing.T, out string) []string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 12 && regexp.MustCompile(`^[a-z ]+$`).MatchString(strings.TrimSpace(l)) {
			return f[len(f)-12:]
		}
	}
	var w []string
	for _, m := range wordsLine.FindAllStringSubmatch(out, -1) {
		w = append(w, m[1])
	}
	if len(w) >= 12 {
		return w[len(w)-12:]
	}
	t.Fatalf("no recovery words in %q", out)
	return nil
}

// TestE2EInteropRecover: an identity made by reroute-cli, with a direct
// chat with a Railcar contact, is recovered in Railcar with its words. The
// recovery removes Reroute's device; the contact's Railcar adds the new
// device to the chat, and messages go both ways.
func TestE2EInteropRecover(t *testing.T) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	out := ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	words := cliWords(t, out)
	kat := newRailcar(t, o, "a.test", "kat")

	if _, err := kat.e.Add(ctx, lastLine(ravi.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", "kat.a.test")
	settle()
	kat.sync()
	ch := kat.chatWith("ravi.b.test")
	if _, err := kat.e.SendText(ctx, ch.ID, "before the recovery", ""); err != nil {
		t.Fatal(err)
	}

	r := recoverRailcar(t, o, "b.test", "ravi.b.test", words)
	if n := len(r.e.Client().Chain().State.Devices); n != 1 {
		t.Fatalf("%d devices after the recovery", n)
	}
	// Kat sees Ravi's devices change, removes the old leaf, and adds the
	// new device (§11.9).
	kat.syncDevices()
	rch := r.waitChat("kat.a.test")
	if rch.ID != ch.ID {
		t.Fatalf("the recovered device is in %s, not %s", rch.ID, ch.ID)
	}
	if _, err := r.e.SendText(ctx, rch.ID, "back from the recovery", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kat.find(ch.ID, "back from the recovery")
	if _, err := kat.e.SendText(ctx, ch.ID, "welcome back", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	r.sync()
	r.find(rch.ID, "welcome back")

	// The words were used once: they still recover, with the next key.
	r2 := recoverRailcar(t, o, "b.test", "ravi.b.test", words)
	if st := r2.e.Client().Chain().State; st.RecoveryEntries != 2 || len(st.Devices) != 1 {
		t.Fatalf("after a second recovery: %d recovery entries, %d devices", st.RecoveryEntries, len(st.Devices))
	}
}

// TestE2ERailcarRecover: a Railcar identity is recovered in Railcar, and
// its Railcar contact on another homeserver adds the new device to their
// direct chat. Wrong words are refused before anything is sent.
func TestE2ERailcarRecover(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	c, words, err := client.Register(ctx, "a.test", "kat", testnet.SignupCode, filepath.Join(t.TempDir(), "kat"), client.Options{Origins: o})
	if err != nil {
		t.Fatal(err)
	}
	kat, err := New(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kat.Close)
	ravi := newRailcar(t, o, "b.test", "ravi")
	code, err := kat.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	if err := kat.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	kr := &railcar{t: t, e: kat}
	kr.sync()
	if _, err := kat.Accept(ctx, ravi.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	ch := ravi.waitChat("kat.a.test")

	bad := words.Words()
	bad[0], bad[1] = bad[1], bad[0]
	if _, err := client.Recover(ctx, "a.test", "kat.a.test", bad, filepath.Join(t.TempDir(), "bad"), client.Options{Origins: o}); err == nil {
		t.Fatal("swapped words recovered the identity")
	}

	r := recoverRailcar(t, o, "a.test", "kat.a.test", words.Words())
	ravi.syncDevices()
	rch := r.waitChat("ravi.b.test")
	if rch.ID != ch.ID {
		t.Fatalf("the recovered device is in %s, not %s", rch.ID, ch.ID)
	}
	if _, err := ravi.e.SendText(ctx, ch.ID, "are you back", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	r.sync()
	r.find(rch.ID, "are you back")
}

// TestE2EInteropRecoverWithReroute: a Railcar identity with a direct chat
// with a Reroute contact is recovered in Railcar. Reroute follows the
// recovery: it adds the new device to the chat, and messages go both
// ways.
func TestE2EInteropRecoverWithReroute(t *testing.T) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	c, words, err := client.Register(ctx, "a.test", "kat", testnet.SignupCode, filepath.Join(t.TempDir(), "kat"), client.Options{Origins: o})
	if err != nil {
		t.Fatal(err)
	}
	kat, err := New(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kat.Close)
	if err := kat.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := kat.Add(ctx, lastLine(ravi.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", "kat.a.test")
	settle()
	kr := &railcar{t: t, e: kat}
	kr.sync()
	ch := kr.chatWith("ravi.b.test")

	r := recoverRailcar(t, o, "a.test", "kat.a.test", words.Words())
	rch := r.waitChat("ravi.b.test", func() { ravi.run("sync") })
	if rch.ID != ch.ID {
		t.Fatalf("the recovered device is in %s, not %s", rch.ID, ch.ID)
	}
	if _, err := r.e.SendText(ctx, rch.ID, "recovered in Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "recovered in Railcar") {
		t.Fatal("Reroute didn't get the recovered device's message")
	}
	ravi.run("send", "kat.a.test", "glad you're back")
	settle()
	r.sync()
	r.find(rch.ID, "glad you're back")
}
