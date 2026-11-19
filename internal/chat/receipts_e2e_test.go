package chat

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// TestE2ERailcarReceipts has two Railcar identities on the live stream
// ask for read receipts and answer (§13.3): nothing goes back before the
// reader agrees, and a receipt marks what the sender asked about. Then
// Railcar asks Reroute, which shows the message.
func TestE2ERailcarReceipts(t *testing.T) {
	bin := testnet.Rerouted(t)
	lee := newRerouteCLI(t, "lee")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	for _, r := range []*railcar{kat, ravi} {
		done := make(chan error, 1)
		go func() { done <- r.e.Run(ctx) }()
		defer func() { cancel(); <-done }()
		waitEvent(t, r.e, "the stream", func(ev Event) bool { return ev.Kind == EventStream && ev.Stream == client.EventConnected })
	}

	code, err := ravi.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, ravi.e, "Kat's request", func(ev Event) bool {
		return ev.Kind == EventContact && ev.Contact.State == StatePendingIn
	})
	rch, err := ravi.e.Accept(ctx, ev.Contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, kat.e, "the direct chat", func(ev Event) bool { return ev.Kind == EventChat && ev.Chat.ID == rch.ID })

	// A message that doesn't ask raises no question.
	if _, err := kat.e.SendText(ctx, rch.ID, "no receipt asked", ""); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ravi.e, "the first message", func(ev Event) bool { return ev.Kind == EventMessage && ev.Message.Body == "no receipt asked" })
	if q := ravi.e.ReceiptQuestion(rch.ID); q != "" {
		t.Fatalf("a question without anyone asking: %q", q)
	}

	// Kat asks. Ravi reads, but hasn't agreed, so nothing goes back.
	if err := kat.e.SetAskReceipts(true); err != nil {
		t.Fatal(err)
	}
	asked, err := kat.e.SendText(ctx, rch.ID, "did you read this?", "")
	if err != nil || !asked.AskedReceipt {
		t.Fatal(asked, err)
	}
	waitEvent(t, ravi.e, "the asking message", func(ev Event) bool { return ev.Kind == EventMessage && ev.Message.ID == asked.ID })
	if q := ravi.e.ReceiptQuestion(rch.ID); q != kat.e.c.ID() {
		t.Fatalf("the question names %q", q)
	}
	if err := ravi.e.MarkRead(rch.ID); err != nil {
		t.Fatal(err)
	}
	settle()
	if m := kat.find(rch.ID, "did you read this?"); len(m.ReadBy) != 0 {
		t.Fatalf("a receipt before Ravi agreed: %+v", m)
	}

	// Ravi agrees, and the receipt marks Kat's message.
	if err := ravi.e.SetReadReceipts(ctx, rch.ID, true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, kat.e, "the receipt", func(ev Event) bool {
		return ev.Kind == EventMessage && ev.Message.ID == asked.ID && slices.Contains(ev.Message.ReadBy, ravi.e.c.ID())
	})
	if q := ravi.e.ReceiptQuestion(rch.ID); q != "" {
		t.Fatalf("still asking after the answer: %q", q)
	}
	if m := kat.find(rch.ID, "no receipt asked"); len(m.ReadBy) != 0 {
		t.Fatalf("a receipt for a message that didn't ask: %+v", m)
	}

	// The next one is acknowledged when Ravi reads it.
	next, err := kat.e.SendText(ctx, rch.ID, "and this one?", "")
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ravi.e, "the next message", func(ev Event) bool { return ev.Kind == EventMessage && ev.Message.ID == next.ID })
	if err := ravi.e.MarkRead(rch.ID); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, kat.e, "the next receipt", func(ev Event) bool {
		return ev.Kind == EventMessage && ev.Message.ID == next.ID && len(ev.Message.ReadBy) == 1
	})

	// Declining is silent.
	if err := ravi.e.SetReadReceipts(ctx, rch.ID, false); err != nil {
		t.Fatal(err)
	}
	last, err := kat.e.SendText(ctx, rch.ID, "and now?", "")
	if err != nil {
		t.Fatal(err)
	}
	waitEvent(t, ravi.e, "the last message", func(ev Event) bool { return ev.Kind == EventMessage && ev.Message.ID == last.ID })
	ravi.e.MarkRead(rch.ID)
	settle()
	if m := kat.find(rch.ID, "and now?"); len(m.ReadBy) != 0 {
		t.Fatalf("a receipt after Ravi declined: %+v", m)
	}

	// Reroute shows a message that asks.
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("accept", "kat.a.test")
	until(t, "the chat with Lee", func() bool {
		for _, c := range kat.e.Contacts() {
			if c.Handle == "lee.b.test" && c.Chat != "" {
				return true
			}
		}
		return false
	})
	if _, err := kat.e.SendText(ctx, kat.chatWith("lee.b.test").ID, "Lee, a receipt please", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Lee reads it", func() bool {
		lee.run("sync")
		return strings.Contains(lee.run("history", "kat.a.test"), "Lee, a receipt please")
	})
}
