package chat

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// befriend makes a Railcar identity and a reroute-cli identity contacts:
// Railcar sends the request with Reroute's invite code, and Reroute
// accepts, which creates their direct chat.
func befriend(t *testing.T, kat *railcar, r *rerouteCLI, katHandle string) {
	t.Helper()
	if _, err := kat.e.Add(context.Background(), lastLine(r.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	r.run("sync")
	r.run("accept", katHandle)
	settle()
	kat.sync()
}

// befriendCLI makes two reroute-cli identities contacts.
func befriendCLI(a, b *rerouteCLI, aHandle string) {
	a.run("add", lastLine(b.run("v2", "invite")))
	settle()
	b.run("sync")
	b.run("accept", aHandle)
	settle()
	a.run("sync")
}

// contactID returns the identifier of a contact by handle.
func (r *railcar) contactID(handle string) string {
	r.t.Helper()
	for _, c := range r.e.Contacts() {
		if c.Handle == handle {
			return c.ID
		}
	}
	r.t.Fatalf("no contact @%s", handle)
	return ""
}

// group returns a group chat by name.
func (r *railcar) group(name string) Chat {
	r.t.Helper()
	for _, c := range r.e.Chats() {
		if c.Kind == KindGroup && c.Name == name && !c.Left {
			return c
		}
	}
	r.t.Fatalf("no group chat %q in %+v", name, r.e.Chats())
	return Chat{}
}

func (r *railcar) has(chat, body string) bool {
	msgs, _ := r.e.Messages(chat)
	return slices.ContainsFunc(msgs, func(m Message) bool { return m.Body == body })
}

// requestID finds an add-request's ID in reroute-cli's history of a group.
func requestID(t *testing.T, history, who string) string {
	t.Helper()
	for _, l := range strings.Split(history, "\n") {
		if strings.Contains(l, who) && (strings.Contains(l, "add") || strings.Contains(l, "Add")) {
			for _, f := range strings.Fields(l) {
				if validMsgID(strings.Trim(f, "():,[]")) {
					return strings.Trim(f, "():,[]")
				}
			}
		}
	}
	t.Fatalf("no add-request for %s in reroute-cli's history", who)
	return ""
}

func TestE2EInteropGroups(t *testing.T) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	lee := newRerouteCLI(t, "lee")
	max := newRerouteCLI(t, "max")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	lee.run("v2", "register", "lee", "--server", o["a.test"], "--invite", testnet.SignupCode)
	max.run("v2", "register", "max", "--server", o["b.test"], "--invite", testnet.SignupCode)
	kat := newRailcar(t, o, "a.test", "kat")
	const katH, raviH, leeH, maxH = "kat.a.test", "ravi.b.test", "lee.a.test", "max.b.test"
	befriend(t, kat, ravi, katH)
	befriend(t, kat, lee, katH)
	befriend(t, kat, max, katH)
	befriendCLI(ravi, lee, raviH)
	raviID, leeID, maxID := kat.contactID(raviH), kat.contactID(leeH), kat.contactID(maxH)
	syncAll := func() {
		settle()
		for _, r := range []*rerouteCLI{ravi, lee, max} {
			r.run("sync")
		}
		kat.sync()
	}

	// Railcar creates a group with Ravi; messages go both ways.
	tie, err := kat.e.CreateGroup(ctx, "Tie line", []string{raviID})
	if err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "Tie line") {
		t.Log("Ravi's sync doesn't name the group")
	}
	if out := ravi.run("groups"); !strings.Contains(out, "Tie line") {
		t.Fatal("Ravi doesn't have Tie line")
	}
	if _, err := kat.e.SendText(ctx, tie.ID, "hello group, from Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "hello group, from Railcar") {
		t.Fatal("Ravi didn't get Kat's group message")
	}
	ravi.run("send", "Tie line", "hello group, from Reroute")
	settle()
	kat.sync()
	if !kat.has(tie.ID, "hello group, from Reroute") {
		t.Fatal("Kat didn't get Ravi's group message")
	}

	// Kat, an admin, adds Lee directly.
	if added, err := kat.e.AddMember(ctx, tie.ID, leeID); err != nil || !added {
		t.Fatalf("adding Lee: %v %v", added, err)
	}
	syncAll()
	lee.run("send", "Tie line", "Lee here")
	syncAll()
	if !kat.has(tie.ID, "Lee here") || !strings.Contains(ravi.run("history", "Tie line"), "Lee here") {
		t.Fatal("Lee's message didn't reach Kat and Ravi")
	}

	// Kat makes Ravi an admin, and Ravi removes Lee.
	if err := kat.e.SetAdmin(ctx, tie.ID, raviID, true); err != nil {
		t.Fatal(err)
	}
	syncAll()
	ravi.run("group-remove", "Tie line", leeH)
	syncAll()
	if ch, _ := kat.e.Chat(tie.ID); slices.Contains(ch.Members, leeID) || !slices.Contains(ch.Admins, raviID) {
		t.Fatalf("after Ravi removed Lee: %+v", ch)
	}

	// Reroute creates a group with Kat and Lee; it shows by its name.
	ravi.run("group-create", "Late line", katH, leeH)
	syncAll()
	late := kat.group("Late line")
	if !slices.Contains(late.Admins, raviID) || len(late.Members) != 3 {
		t.Fatalf("Late line: %+v", late)
	}
	kat.e.mu.Lock()
	_, _, ext, _ := kat.e.groupState(&late)
	kat.e.mu.Unlock()
	t.Logf("Reroute's new group: since %v, heads for %d, basis for %d", ext.Since, len(ext.Heads), len(ext.Basis))
	if _, err := kat.e.SendText(ctx, late.ID, "Railcar in Late line", ""); err != nil {
		t.Fatal(err)
	}
	ravi.run("send", "Late line", "Reroute in Late line")
	syncAll()
	if !kat.has(late.ID, "Reroute in Late line") || !strings.Contains(lee.run("history", "Late line"), "Railcar in Late line") {
		t.Fatal("messages in Late line")
	}

	// Kat isn't an admin there: she asks to add Max, Ravi approves, and
	// her commit adds him with the approval, which Reroute checks.
	if added, err := kat.e.AddMember(ctx, late.ID, maxID); err != nil || added {
		t.Fatalf("asking to add Max: %v %v", added, err)
	}
	settle()
	ravi.run("sync")
	ravi.run("group-approve", "Late line", requestID(t, ravi.run("history", "Late line"), maxH))
	syncAll()
	syncAll()
	if ch, _ := kat.e.Chat(late.ID); !slices.Contains(ch.Members, maxID) {
		t.Fatalf("Max wasn't added: %+v", ch)
	}
	max.run("send", "Late line", "Max made it")
	syncAll()
	if !kat.has(late.ID, "Max made it") || !strings.Contains(ravi.run("history", "Late line"), "Max made it") {
		t.Fatal("Max's message")
	}

	// The other way: Kat creates a group with Lee, who isn't an admin;
	// Lee asks to add Ravi, Kat approves, and Lee's commit adds him.
	desk, err := kat.e.CreateGroup(ctx, "Desk", []string{leeID})
	if err != nil {
		t.Fatal(err)
	}
	syncAll()
	lee.run("group-add", "Desk", raviH)
	settle()
	kat.sync()
	ch, _ := kat.e.Chat(desk.ID)
	if len(ch.Requests) != 1 || ch.Requests[0].Who != raviID {
		t.Fatalf("Lee's add-request: %+v", ch.Requests)
	}
	if err := kat.e.AnswerAdd(ctx, desk.ID, ch.Requests[0].ID, true); err != nil {
		t.Fatal(err)
	}
	syncAll()
	syncAll()
	if ch, _ := kat.e.Chat(desk.ID); !slices.Contains(ch.Members, raviID) || ch.Stopped != "" {
		t.Fatalf("Ravi wasn't added to Desk: %+v", ch)
	}

	// Renames both ways.
	name := "Front desk"
	if err := kat.e.SetGroupMeta(ctx, desk.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if out := lee.run("groups"); !strings.Contains(out, "Front desk") {
		t.Fatal("Lee didn't see the new name")
	}
	ravi.run("group-rename", "Front desk", "Back", "desk")
	syncAll()
	if ch, _ := kat.e.Chat(desk.ID); ch.Name != "Back desk" {
		t.Fatalf("the name after Ravi's rename: %q", ch.Name)
	}

	// Leaving: Kat leaves Late line, and Ravi commits it; then Lee leaves
	// Back desk, and Kat commits that.
	if err := kat.e.Leave(ctx, late.ID); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if out := ravi.run("group-members", "Late line"); strings.Contains(out, katH) {
		t.Fatalf("Kat is still in Late line:\n%s", out)
	}
	lee.run("group-leave", "Back desk")
	syncAll()
	if ch, _ := kat.e.Chat(desk.ID); slices.Contains(ch.Members, leeID) || ch.Stopped != "" {
		t.Fatalf("after Lee left Back desk: %+v", ch)
	}
	if _, err := kat.e.SendText(ctx, desk.ID, "after Lee left", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	if out := ravi.run("sync"); !strings.Contains(out, "after Lee left") {
		t.Fatal("Ravi didn't get Kat's message after Lee left")
	}
}

func TestE2ERailcarGroups(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	lee := newRailcar(t, o, "a.test", "lee")
	friends := func(a, b *railcar) {
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
		settle()
		a.sync()
	}
	friends(kat, ravi)
	friends(kat, lee)
	friends(ravi, lee)
	all := func() {
		settle()
		for range 2 {
			for _, r := range []*railcar{kat, ravi, lee} {
				r.sync()
			}
		}
	}
	katID, raviID, leeID := kat.e.Client().ID(), ravi.e.Client().ID(), lee.e.Client().ID()

	g, err := kat.e.CreateGroup(ctx, "Signal box", []string{raviID})
	if err != nil {
		t.Fatal(err)
	}
	all()
	rg := ravi.group("Signal box")
	if _, err := ravi.e.SendText(ctx, rg.ID, "from ravi", ""); err != nil {
		t.Fatal(err)
	}
	all()
	if !kat.has(g.ID, "from ravi") {
		t.Fatal("kat didn't get ravi's message")
	}

	// Ravi, not an admin, asks to add Lee; Kat approves; Ravi commits.
	if added, err := ravi.e.AddMember(ctx, rg.ID, leeID); err != nil || added {
		t.Fatalf("ravi's add-request: %v %v", added, err)
	}
	all()
	ch, _ := kat.e.Chat(g.ID)
	if len(ch.Requests) != 1 {
		t.Fatalf("kat's requests: %+v", ch.Requests)
	}
	if err := kat.e.AnswerAdd(ctx, g.ID, ch.Requests[0].ID, true); err != nil {
		t.Fatal(err)
	}
	all()
	for _, r := range []*railcar{kat, ravi, lee} {
		ch, _ := r.e.Chat(g.ID)
		if len(ch.Members) != 3 || ch.Stopped != "" || ch.Name != "Signal box" {
			t.Fatalf("%s's copy after the approved add: %+v", r.e.Client().Handle(), ch)
		}
	}

	// Kat, the only admin, leaves: admin goes to Ravi, who was there
	// longest, and the others commit her leave.
	if err := kat.e.Leave(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	all()
	for _, r := range []*railcar{ravi, lee} {
		ch, _ := r.e.Chat(g.ID)
		if slices.Contains(ch.Members, katID) || !slices.Equal(ch.Admins, []string{raviID}) || ch.Stopped != "" {
			t.Fatalf("%s's copy after kat left: %+v", r.e.Client().Handle(), ch)
		}
	}
	if ch, _ := kat.e.Chat(g.ID); !ch.Left {
		t.Fatalf("kat's copy after leaving: %+v", ch)
	}

	// Ravi removes Lee, and renames the group.
	if err := ravi.e.RemoveMember(ctx, g.ID, leeID); err != nil {
		t.Fatal(err)
	}
	name := "Signal box 2"
	if err := ravi.e.SetGroupMeta(ctx, g.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	all()
	if ch, _ := lee.e.Chat(g.ID); !ch.Left {
		t.Fatalf("lee's copy after the removal: %+v", ch)
	}
	if ch, _ := ravi.e.Chat(g.ID); len(ch.Members) != 1 || ch.Name != name {
		t.Fatalf("ravi's copy: %+v", ch)
	}
}
