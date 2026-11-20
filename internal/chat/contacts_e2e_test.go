package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// linkRailcar links a second Railcar device to r's identity, by a link
// code, and waits for it to have every chat.
func linkRailcar(t *testing.T, o map[string]string, r *railcar, server, handle string) *railcar {
	t.Helper()
	ctx := context.Background()
	l, err := client.StartLink(ctx, server, handle, client.Options{Origins: o, DeviceName: "Railcar"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.e.Client().ApproveLinkCode(ctx, l.Code().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm(ctx, a.Confirmation()); err != nil {
		t.Fatal(err)
	}
	settle()
	if ok, err := a.Check(ctx); !ok {
		t.Fatalf("the confirmation code: %v", err)
	}
	if err := r.e.AddDevice(ctx, a.Device(), "Second Railcar"); err != nil {
		t.Fatal(err)
	}
	r2 := finishLink(t, l)
	r.syncDevices(func() { r2.sync() })
	return r2
}

// contactState is a contact's state on a device, or "" when it isn't
// listed.
func (r *railcar) contactState(id string) string {
	for _, c := range r.e.Contacts() {
		if c.ID == id {
			return c.State
		}
	}
	return ""
}

// stopped says why a chat stopped on a device.
func (r *railcar) stopped(chat string) string {
	c, _ := r.e.Chat(chat)
	return c.Stopped
}

// TestE2ERailcarContacts removes, blocks, and unblocks between Railcar
// identities on two servers (§9.6). A removed contact's app ends the
// chat; a blocked one isn't told, and its messages and requests go
// nowhere. The remover's second device follows each change.
func TestE2ERailcarContacts(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	befriendRailcar := func() Chat {
		t.Helper()
		code, err := kat.e.Invite(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ravi.e.Add(ctx, code); err != nil {
			t.Fatal(err)
		}
		settle()
		kat.sync()
		if _, err := kat.e.Accept(ctx, ravi.e.Client().ID()); err != nil {
			t.Fatal(err)
		}
		return ravi.waitChat("kat.a.test")
	}
	ch := befriendRailcar()
	kat2 := linkRailcar(t, o, kat, "a.test", "kat.a.test")
	kat2.waitChat("ravi.b.test")
	raviID, katID := ravi.e.Client().ID(), kat.e.Client().ID()

	// Kat removes Ravi: the chat ends on each side, and on Kat's second
	// device, and Ravi is no longer Kat's contact.
	if err := kat.e.RemoveContact(ctx, raviID); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	kat2.sync()
	if got := ravi.stopped(ch.ID); got != "@kat.a.test left this chat" {
		t.Errorf("Ravi's chat after Kat removed him: %q", got)
	}
	if got := kat.stopped(ch.ID); got != "You removed @ravi.b.test as a contact" {
		t.Errorf("Kat's chat: %q", got)
	}
	if got := kat2.stopped(ch.ID); got != "You removed @ravi.b.test as a contact on another device" {
		t.Errorf("Kat's second device: %q", got)
	}
	for _, d := range []*railcar{kat, kat2} {
		if s := d.contactState(raviID); s != "" {
			t.Errorf("Ravi is still listed as %q", s)
		}
	}
	if _, err := kat.e.SendText(ctx, ch.ID, "after removing", ""); err == nil {
		t.Error("Kat sent in the ended chat")
	}

	// They become contacts again, in a new chat.
	ch = befriendRailcar()
	kat.waitChat("ravi.b.test")
	kat2.waitChat("ravi.b.test")
	if _, err := ravi.e.SendText(ctx, ch.ID, "hello again", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kat.find(ch.ID, "hello again")

	// Kat blocks Ravi: Ravi's app isn't told, and his messages and a new
	// request with a fresh code go nowhere.
	if err := kat.e.Block(ctx, raviID); err != nil {
		t.Fatal(err)
	}
	settle()
	kat2.sync()
	ravi.sync()
	if got := ravi.stopped(ch.ID); got != "" {
		t.Errorf("Ravi's app was told: %q", got)
	}
	if got := kat2.stopped(ch.ID); got != "You blocked @ravi.b.test on another device" {
		t.Errorf("Kat's second device: %q", got)
	}
	for _, d := range []*railcar{kat, kat2} {
		if s := d.contactState(raviID); s != StateBlocked {
			t.Errorf("Ravi is listed as %q, not blocked", s)
		}
	}
	if _, err := ravi.e.SendText(ctx, ch.ID, "are you there?", ""); err != nil {
		t.Fatal(err)
	}
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kat2.sync()
	for _, d := range []*railcar{kat, kat2} {
		if d.has(ch.ID, "are you there?") {
			t.Error("a blocked contact's message arrived")
		}
		if s := d.contactState(raviID); s != StateBlocked {
			t.Errorf("after Ravi's request, he's listed as %q", s)
		}
	}

	// Kat unblocks Ravi, and his next request arrives.
	if err := kat.e.Unblock(ctx, raviID); err != nil {
		t.Fatal(err)
	}
	if s := kat.contactState(raviID); s != "" {
		t.Errorf("after unblocking, Ravi is listed as %q", s)
	}
	code, err = kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	if s := kat.contactState(raviID); s != StatePendingIn {
		t.Errorf("Ravi's request after unblocking: %q", s)
	}
	// Blocking someone whose request waits drops the request.
	if err := kat.e.Block(ctx, raviID); err != nil {
		t.Fatal(err)
	}
	if s := kat.contactState(raviID); s != StateBlocked {
		t.Errorf("blocking a request left %q", s)
	}
	if err := ravi.e.Block(ctx, katID); err != nil {
		t.Fatal(err)
	}
}

// TestE2EInteropContacts removes and blocks between Railcar and Reroute
// (§9.6). Reroute removes and blocks without telling the other side, so
// Railcar's chat goes on there, and its messages go nowhere.
func TestE2EInteropContacts(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	lee := newRerouteCLI(t, "lee")
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	max := newRerouteCLI(t, "max")
	max.run("v2", "register", "max", "--server", o["b.test"], "--invite", testnet.SignupCode)
	befriend(t, kat, lee, "kat.a.test")
	befriend(t, kat, max, "kat.a.test")
	lch := kat.chatWith("lee.b.test")
	mch := kat.chatWith("max.b.test")

	// Railcar removes Lee: Reroute gets the leave, and Kat's messages stop.
	if err := kat.e.RemoveContact(ctx, kat.contactID("lee.b.test")); err != nil {
		t.Fatal(err)
	}
	settle()
	out := lee.run("sync")
	t.Logf("Reroute after Railcar removed it: %s", out)
	lee.run("send", "kat.a.test", "after you removed me")
	settle()
	kat.sync()
	if kat.has(lch.ID, "after you removed me") {
		t.Error("a removed contact's message arrived")
	}

	// Railcar blocks Max: Reroute isn't told, and a request from Max with
	// a fresh code is dropped.
	maxID := kat.contactID("max.b.test")
	if err := kat.e.Block(ctx, maxID); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := max.run("sync"); strings.Contains(strings.ToLower(out), "left") || strings.Contains(strings.ToLower(out), "block") {
		t.Errorf("Reroute was told: %s", out)
	}
	max.run("send", "kat.a.test", "are you there?")
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	max.run("add", code)
	settle()
	kat.sync()
	if kat.has(mch.ID, "are you there?") {
		t.Error("a blocked contact's message arrived")
	}
	if s := kat.contactState(maxID); s != StateBlocked {
		t.Errorf("after Max's request, he's listed as %q", s)
	}
	if err := kat.e.Unblock(ctx, maxID); err != nil {
		t.Fatal(err)
	}

	// Reroute blocks Railcar: a request from Kat with Lee's fresh code
	// goes nowhere. (Lee isn't a contact now, so Lee blocks.)
	lee.run("block", "kat.a.test")
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := lee.run("sync"); strings.Contains(out, "kat.a.test") {
		t.Errorf("Reroute got a blocked identity's request: %s", out)
	}
	lee.run("unblock", "kat.a.test")
}
