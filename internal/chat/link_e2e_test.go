package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The linking tests (§7.2, §7.2.1, §11.9). They're skipped unless
// RAILCAR_REROUTED names a rerouted binary, and the interop ones unless
// RAILCAR_REROUTE_CLI names a reroute-cli binary.

// finishLink waits for the existing device to add this one, then starts
// its chat layer as the app does: the contact list first, then key
// packages.
func finishLink(t *testing.T, l *client.Link) *railcar {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "linked")
	var c *client.Client
	for range 40 {
		var err error
		c, err = l.Finish(ctx, dir)
		if err == nil {
			break
		}
		if !errors.Is(err, client.ErrNotLinkedYet) {
			t.Fatal(err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("the existing device didn't add this one")
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

// line returns the text after prefix on one of out's lines.
func line(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %q in %q", prefix, out)
	return ""
}

// waitChat syncs a Railcar device until it has a chat with handle.
func (r *railcar) waitChat(handle string, also ...func()) Chat {
	r.t.Helper()
	for range 20 {
		for _, f := range also {
			f()
		}
		r.sync()
		for _, c := range r.e.Contacts() {
			if c.Handle == handle && c.Chat != "" {
				if ch, ok := r.e.Chat(c.Chat); ok {
					return ch
				}
			}
		}
		settle()
	}
	r.t.Fatalf("no chat with @%s", handle)
	return Chat{}
}

// syncDevices runs the sync rule until every chat is in step.
func (r *railcar) syncDevices(also ...func()) {
	r.t.Helper()
	for range 20 {
		for _, f := range also {
			f()
		}
		r.sync()
		done, err := r.e.SyncDevices(context.Background())
		if err != nil {
			r.t.Log(err)
		}
		if done && err == nil {
			return
		}
		settle()
	}
	r.t.Fatal("the sync rule didn't finish")
}

func (r *railcar) hasLine(chat, text string) bool {
	msgs, _ := r.e.Messages(chat)
	for _, m := range msgs {
		if m.System && m.Body == text {
			return true
		}
	}
	return false
}

// TestE2EInteropLinkFromReroute: a Reroute identity with a chat approves
// Railcar as a new device, first by Railcar's link code and a typed
// confirmation code, then by an approval code Reroute shows. Each Railcar
// device gets the chat, and messages go between it and the contact, on
// another homeserver.
func TestE2EInteropLinkFromReroute(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ravi, lee := newRerouteCLI(t, "ravi"), newRerouteCLI(t, "lee")
	ctx := context.Background()
	opts := client.Options{Origins: o, DeviceName: "Railcar"}
	ravi.run("v2", "register", "ravi", "--server", o["a.test"], "--invite", testnet.SignupCode)
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	ravi.run("add", lastLine(lee.run("v2", "invite")))
	settle()
	lee.run("sync")
	lee.run("accept", "ravi.a.test")
	settle()
	ravi.run("sync")
	lee.run("send", "ravi.a.test", "hello Ravi, before your new device")
	settle()
	ravi.run("sync")

	// The link code, approved in Reroute with the confirmation code.
	l, err := client.StartLink(ctx, "a.test", "@ravi.a.test", opts)
	if err != nil {
		t.Fatal(err)
	}
	confirm := line(t, ravi.run("v2", "add-device", l.Code().String(), "--name", "Ravi's Railcar"), "confirmation code:")
	if err := l.Confirm(ctx, strings.ToLower(confirm)); err != nil {
		t.Fatal(err)
	}
	ravi.run("v2", "approve-finish")
	rc := finishLink(t, l)
	if rc.e.Client().LinkPending() {
		rc.sync()
	}
	if rc.e.Client().LinkPending() || len(rc.e.Client().AccountKey()) != 16 {
		t.Fatal("Railcar didn't open the link bundle")
	}
	ch := rc.waitChat("lee.b.test", func() { ravi.run("sync"); lee.run("sync") })
	if ch.Kind != KindDirect || len(ch.Members) != 2 {
		t.Fatalf("the chat: %+v", ch)
	}
	if _, err := rc.e.SendText(ctx, ch.ID, "hello Lee, from Ravi's Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := lee.run("sync"); !strings.Contains(out, "hello Lee, from Ravi's Railcar") {
		t.Fatal("Lee didn't get the message from Ravi's Railcar")
	}
	ravi.run("sync")
	if out := ravi.run("history", "lee.b.test"); !strings.Contains(out, "hello Lee, from Ravi's Railcar") {
		t.Error("Ravi's first device doesn't show what his Railcar sent")
	}
	lee.run("send", "ravi.a.test", "hello to all of Ravi's devices")
	settle()
	rc.sync()
	rc.find(ch.ID, "hello to all of Ravi's devices")

	// An approval code Reroute shows, entered in Railcar: no confirmation
	// code (§7.2.1).
	code := line(t, ravi.run("v2", "approval-code", "--name", "Ravi's second Railcar"), "link-scan ")
	l2, err := client.StartLinkWithApproval(ctx, code, opts)
	if err != nil {
		t.Fatal(err)
	}
	ravi.run("v2", "approval-finish")
	rc2 := finishLink(t, l2)
	ch2 := rc2.waitChat("lee.b.test", func() { ravi.run("sync"); lee.run("sync"); rc.sync() })
	if ch2.ID != ch.ID {
		t.Errorf("the second Railcar is in another chat")
	}
	lee.run("send", "ravi.a.test", "hello to Ravi's three devices")
	settle()
	rc.sync()
	rc2.sync()
	rc.find(ch.ID, "hello to Ravi's three devices")
	rc2.find(ch.ID, "hello to Ravi's three devices")
	if !rc.hasLine(ch.ID, "A new device was added to your account") {
		t.Error("the first Railcar shows no line for the second")
	}
	if _, err := rc2.e.SendText(ctx, ch.ID, "hello Lee, from the second Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := lee.run("sync"); !strings.Contains(out, "hello Lee, from the second Railcar") {
		t.Fatal("Lee didn't get the message from the second Railcar")
	}
}

// TestE2EInteropLinkToReroute: a Railcar identity with a chat approves
// reroute-cli as a new device, by its link code, and adds it to the chat;
// then another by an approval code Railcar shows; then removes the first.
func TestE2EInteropLinkToReroute(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	lee := newRerouteCLI(t, "lee")
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("accept", "kat.a.test")
	ch := kat.waitChat("lee.b.test")

	// Kat's new Reroute device shows a link code; Railcar approves it.
	desk := newRerouteCLI(t, "kat-desk")
	linkCode := line(t, desk.run("v2", "link-start", "kat.a.test", "--server", o["a.test"]), "add-device ")
	a, err := kat.e.Client().ApproveLinkCode(ctx, linkCode)
	if err != nil {
		t.Fatal(err)
	}
	desk.run("v2", "link-confirm", a.Confirmation())
	ok := false
	for range 20 {
		if ok, err = a.Check(ctx); ok || err != nil {
			break
		}
		settle()
	}
	if !ok {
		t.Fatalf("the confirmation code didn't arrive: %v", err)
	}
	if err := kat.e.AddDevice(ctx, a.Device(), "Kat's desk"); err != nil {
		t.Fatal(err)
	}
	desk.run("v2", "link-finish")
	kat.syncDevices(func() { desk.run("sync") })
	if !kat.hasLine(ch.ID, "A new device was added to your account") {
		t.Error("no line for the new device")
	}
	if _, err := kat.e.SendText(ctx, ch.ID, "hello Lee, to both of Kat's devices", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	if out := desk.run("sync"); !strings.Contains(out, "hello Lee, to both of Kat's devices") && !strings.Contains(desk.run("history", "lee.b.test"), "hello Lee, to both of Kat's devices") {
		t.Fatal("Kat's Reroute device didn't get the chat")
	}
	desk.run("send", "lee.b.test", "hello Lee, from Kat's desk")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello Lee, from Kat's desk")
	if out := lee.run("sync"); !strings.Contains(out, "hello Lee, from Kat's desk") && !strings.Contains(lee.run("history", "kat.a.test"), "hello Lee, from Kat's desk") {
		t.Error("Lee didn't get the message from Kat's desk")
	}

	// An approval code Railcar shows, read by another Reroute device.
	s, err := kat.e.Client().NewApprovalCode()
	if err != nil {
		t.Fatal(err)
	}
	phone := newRerouteCLI(t, "kat-phone")
	phone.run("v2", "link-scan", s.Code().String(), "--server", o["a.test"])
	var dev *client.NewDevice
	for range 20 {
		if dev, err = s.Check(ctx); dev != nil || err != nil {
			break
		}
		settle()
	}
	if dev == nil {
		t.Fatalf("no device used the approval code: %v", err)
	}
	if err := kat.e.AddDevice(ctx, *dev, "Kat's phone"); err != nil {
		t.Fatal(err)
	}
	phone.run("v2", "link-finish")
	kat.syncDevices(func() { phone.run("sync") })
	lee.run("sync")
	lee.run("send", "kat.a.test", "hello to Kat's three devices")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello to Kat's three devices")
	if !strings.Contains(phone.run("sync"), "hello to Kat's three devices") && !strings.Contains(phone.run("history", "lee.b.test"), "hello to Kat's three devices") {
		t.Error("Kat's phone didn't get Lee's message")
	}

	// Railcar removes the desk: its leaf leaves the chat, and Lee follows.
	if err := kat.e.RemoveDevice(ctx, a.Device().Key()); err != nil {
		t.Fatal(err)
	}
	kat.syncDevices()
	if !kat.hasLine(ch.ID, "A device was removed from your account") {
		t.Error("no line for the removed device")
	}
	lee.run("sync")
	lee.run("send", "kat.a.test", "hello after the desk left")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello after the desk left")
	if _, err := kat.e.SendText(ctx, ch.ID, "and Lee still hears Kat", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if !strings.Contains(lee.run("sync"), "and Lee still hears Kat") {
		t.Error("Lee didn't get Kat's message after the removal")
	}
}

// TestE2ERailcarLink: Railcar links Railcar, by a link code and by an
// approval code, across two homeservers' worth of chat, and removes a
// device again. The contact, also Railcar, sees a line each time.
func TestE2ERailcarLink(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	opts := client.Options{Origins: o, DeviceName: "Railcar"}
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	code, err := ravi.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	if _, err := ravi.e.Accept(ctx, kat.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	ch := kat.waitChat("ravi.b.test")

	// A link code and a typed confirmation code.
	l, err := client.StartLink(ctx, "a.test", "kat.a.test", opts)
	if err != nil {
		t.Fatal(err)
	}
	a, err := kat.e.Client().ApproveLinkCode(ctx, l.Code().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm(ctx, "0000-0000"); err != nil {
		t.Fatal(err)
	}
	settle()
	if _, err := a.Check(ctx); !errors.Is(err, client.ErrWrongConfirmation) {
		t.Fatalf("a wrong confirmation code: %v", err)
	}
	if err := l.Confirm(ctx, a.Confirmation()); err != nil {
		t.Fatal(err)
	}
	settle()
	if ok, err := a.Check(ctx); !ok {
		t.Fatalf("the confirmation code: %v", err)
	}
	if err := kat.e.AddDevice(ctx, a.Device(), "Kat's second Railcar"); err != nil {
		t.Fatal(err)
	}
	kat2 := finishLink(t, l)
	kat.syncDevices()
	ch2 := kat2.waitChat("ravi.b.test")
	if ch2.ID != ch.ID {
		t.Fatal("the new device is in another chat")
	}
	if _, err := kat2.e.SendText(ctx, ch.ID, "hello Ravi, from Kat's second device", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	rch := ravi.chatWith("kat.a.test")
	ravi.find(rch.ID, "hello Ravi, from Kat's second device")
	if !ravi.hasLine(rch.ID, "@kat.a.test has a new device") {
		t.Error("Ravi sees no line for Kat's new device")
	}
	kat.sync()
	kat.find(ch.ID, "hello Ravi, from Kat's second device")

	// An approval code; a second device using the same code is refused.
	s, err := kat.e.Client().NewApprovalCode()
	if err != nil {
		t.Fatal(err)
	}
	l3, err := client.StartLinkWithApproval(ctx, s.Code().String(), opts)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := s.Check(ctx)
	if err != nil || dev == nil {
		t.Fatalf("the approval code: %v", err)
	}
	if err := kat.e.AddDevice(ctx, *dev, "Kat's third Railcar"); err != nil {
		t.Fatal(err)
	}
	kat3 := finishLink(t, l3)
	kat.syncDevices()
	kat3.waitChat("ravi.b.test")
	ravi.sync()
	if _, err := ravi.e.SendText(ctx, rch.ID, "hello to Kat's three devices", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	for _, k := range []*railcar{kat, kat2, kat3} {
		k.sync()
		k.find(ch.ID, "hello to Kat's three devices")
	}

	s2, err := kat.e.Client().NewApprovalCode()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := client.StartLinkWithApproval(ctx, s2.Code().String(), opts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s2.Check(ctx); !errors.Is(err, client.ErrApprovalUsedTwice) {
		t.Errorf("two devices with one approval code: %v", err)
	}

	// Removing the third device takes its leaf out; Ravi follows.
	if err := kat.e.RemoveDevice(ctx, dev.Key()); err != nil {
		t.Fatal(err)
	}
	kat.syncDevices()
	ravi.sync()
	if !ravi.hasLine(rch.ID, "A device of @kat.a.test's was removed") {
		t.Error("Ravi sees no line for the removed device")
	}
	if c, _ := ravi.e.Chat(rch.ID); c.Stopped != "" || len(c.Members) != 2 {
		t.Fatalf("Ravi's chat after the removal: %+v", c)
	}
	if _, err := ravi.e.SendText(ctx, rch.ID, "after the removal", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat2.sync()
	kat2.find(ch.ID, "after the removal")
	// The homeserver ended the removed device's sessions (§7.6).
	if err := kat3.e.Sync(ctx); !errors.Is(err, client.ErrDeviceRemoved) {
		t.Errorf("the removed device synced: %v", err)
	}
}
