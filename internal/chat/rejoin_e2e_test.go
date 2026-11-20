package chat

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// TestE2ERailcarRejoin has a Railcar device come back as a new device,
// as one offline too long does (§11.9): it keeps its history, and its
// contact, also Railcar, adds the new device to their direct chat and to a
// group whose third member isn't the rejoiner's contact. Messages go both
// ways after. A second rejoin within a day doesn't happen on its own.
func TestE2ERailcarRejoin(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	lee := newRailcar(t, o, "b.test", "lee")
	befriendRailcars(t, ravi, kat, "kat.a.test")
	befriendRailcars(t, ravi, lee, "lee.b.test")
	dch := kat.chatWith("ravi.b.test")
	g, err := ravi.e.CreateGroup(ctx, "Night shift", []string{kat.e.Client().ID(), lee.e.Client().ID()})
	if err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	lee.sync()
	if _, err := kat.e.SendText(ctx, dch.ID, "before coming back", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()

	old := bytes.Clone(kat.e.Client().DeviceKey())
	if err := kat.e.Rejoin(ctx); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(old, kat.e.Client().DeviceKey()) {
		t.Fatal("the device key didn't change")
	}
	if c, _ := kat.e.Chat(g.ID); !c.Rejoining || c.Stopped == "" {
		t.Fatalf("the group while waiting: %+v", c)
	}
	// Ravi's sync rule replaces Kat's old leaf with the new device, in
	// both chats.
	ravi.syncDevices(func() { kat.sync() })
	kat.sync()
	lee.sync()
	for _, id := range []string{dch.ID, g.ID} {
		c, _ := kat.e.Chat(id)
		if c.Rejoining || c.Stopped != "" {
			t.Fatalf("chat %s after the new device was added: %+v", id, c)
		}
		if !kat.hasLine(id, "This device is back in the chat") {
			t.Errorf("chat %s has no line for coming back", id)
		}
	}
	kat.find(dch.ID, "before coming back")
	if _, err := kat.e.SendText(ctx, g.ID, "back in the group", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, dch.ID, "welcome back", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	lee.sync()
	kat.sync()
	ravi.find(g.ID, "back in the group")
	lee.find(g.ID, "back in the group")
	kat.find(dch.ID, "welcome back")

	// The automatic rejoin waits a day.
	key := bytes.Clone(kat.e.Client().DeviceKey())
	kat.e.mu.Lock()
	kat.e.maybeRejoin(ctx, "a test")
	kat.e.mu.Unlock()
	if !bytes.Equal(key, kat.e.Client().DeviceKey()) {
		t.Error("a second rejoin within a day")
	}
}

// befriendRailcars makes two Railcar identities contacts, and waits for
// their direct chat on both sides.
func befriendRailcars(t *testing.T, a, b *railcar, bHandle string) {
	t.Helper()
	ctx := context.Background()
	code, err := b.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	b.sync()
	if _, err := b.e.Accept(ctx, a.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	a.waitChat(bHandle)
}

// TestE2EInteropRejoin: Railcar comes back as a new device and Reroute
// adds it again, then Reroute comes back as a new device and Railcar adds
// it again (§11.9). Messages go both ways each time.
func TestE2EInteropRejoin(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRerouteCLI(t, "ravi")
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	befriend(t, kat, ravi, "kat.a.test")
	ch := kat.chatWith("ravi.b.test")

	if err := kat.e.Rejoin(ctx); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		ravi.run("sync")
		settle()
		kat.sync()
		if c, _ := kat.e.Chat(ch.ID); !c.Rejoining {
			break
		}
	}
	if c, _ := kat.e.Chat(ch.ID); c.Rejoining || c.Stopped != "" {
		t.Fatalf("Reroute didn't add Railcar's new device: %+v", c)
	}
	if _, err := kat.e.SendText(ctx, ch.ID, "back from Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "back from Railcar") {
		t.Errorf("Reroute didn't get Railcar's message after it came back: %s", out)
	}
	ravi.run("send", "kat.a.test", "hello new device")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello new device")

	// Reroute comes back as a new device; Railcar's sync rule adds it.
	ravi.run("rotate-device")
	settle()
	kat.syncDevices(func() { ravi.run("sync") })
	for range 10 {
		out := ravi.run("sync")
		if _, err := kat.e.SendText(ctx, ch.ID, "hello again, Reroute", ""); err != nil {
			t.Fatal(err)
		}
		settle()
		out = ravi.run("sync")
		if strings.Contains(out, "hello again, Reroute") {
			ravi.run("send", "kat.a.test", "Reroute is back")
			settle()
			kat.sync()
			kat.find(ch.ID, "Reroute is back")
			return
		}
	}
	t.Fatal("Reroute's new device never read Railcar's message")
}

// TestE2ERailcarStuckDirect: a direct chat whose commits have failed for
// an hour offers re-creating it, once, and re-creating it carries the
// conversation on for both Railcar identities (§11.11).
func TestE2ERailcarStuckDirect(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	befriendRailcars(t, ravi, kat, "kat.a.test")
	ch := kat.chatWith("ravi.b.test")
	if _, err := ravi.e.SendText(ctx, ch.ID, "before it got stuck", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()

	kat.e.mu.Lock()
	kat.e.st.Chats[ch.ID].FailingSince = time.Now().Add(-2 * time.Hour).Unix()
	kat.e.checkStuck()
	kat.e.checkStuck()
	kat.e.mu.Unlock()
	c, _ := kat.e.Chat(ch.ID)
	if c.Stuck == "" {
		t.Fatal("no offer to re-create the chat")
	}
	n := 0
	msgs, _ := kat.e.Messages(ch.ID)
	for _, m := range msgs {
		if m.System && strings.HasPrefix(m.Body, "This chat seems stuck") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d offers, not one", n)
	}

	nc, err := kat.e.RecreateDirect(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nc.ID == ch.ID || nc.Stuck != "" {
		t.Fatalf("the re-created chat: %+v", nc)
	}
	settle()
	ravi.sync()
	rch := ravi.chatWith("kat.a.test")
	if rch.ID != nc.ID {
		t.Fatal("Ravi isn't in the re-created chat")
	}
	ravi.find(rch.ID, "before it got stuck")
	kat.find(nc.ID, "before it got stuck")
	if _, err := ravi.e.SendText(ctx, rch.ID, "in the new chat", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	kat.find(nc.ID, "in the new chat")
}

// TestE2EInteropRecreateDirect: Railcar re-creates its direct chat with
// Reroute, which carries on in the new one (§11.11).
func TestE2EInteropRecreateDirect(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRerouteCLI(t, "ravi")
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	befriend(t, kat, ravi, "kat.a.test")
	ch := kat.chatWith("ravi.b.test")
	nc, err := kat.e.RecreateDirect(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.SendText(ctx, nc.ID, "in the re-created chat", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "in the re-created chat") {
		t.Errorf("Reroute didn't read the re-created chat: %s", out)
	}
	ravi.run("send", "kat.a.test", "Reroute in the new chat")
	settle()
	kat.sync()
	kat.find(nc.ID, "Reroute in the new chat")
}
