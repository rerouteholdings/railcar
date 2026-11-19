package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// TestE2EInteropMove: a Railcar identity moves to another homeserver
// (§6.6), asking first and appending with the new server's reservation,
// and its Reroute and Railcar contacts keep talking to it. Then the
// Reroute contact moves, and Railcar keeps talking to it.
func TestE2EInteropMove(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test", "c.test")
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	lee := newRerouteCLI(t, "lee")
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("accept", "kat.a.test")
	lch := kat.waitChat("lee.b.test")
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
	rch := kat.waitChat("ravi.b.test")

	// A refused move changes nothing: the wrong registration code.
	if err := kat.e.Move(ctx, "c.test", "kat", "not-the-code"); err == nil {
		t.Fatal("a move with a bad registration code went through")
	}
	if kat.e.Client().Server() != "a.test" || kat.e.Client().PendingMove() != nil {
		t.Fatal("the refused move changed something")
	}
	if err := kat.e.Move(ctx, "c.test", "kat", testnet.SignupCode); err != nil {
		t.Fatal(err)
	}
	if kat.e.Client().Server() != "c.test" || kat.e.Client().Handle() != "kat.c.test" {
		t.Fatalf("after the move: %s, @%s", kat.e.Client().Server(), kat.e.Client().Handle())
	}
	settle()

	// Both contacts reach the new home, and hear from it.
	lee.run("sync")
	lee.run("send", "kat.c.test", "hello Kat, at your new home")
	if _, err := ravi.e.SendText(ctx, rch.ID, "hello from Ravi, after your move", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Kat gets both", func() bool {
		kat.sync()
		return kat.hasMessage(lch.ID, "hello Kat, at your new home") && kat.hasMessage(rch.ID, "hello from Ravi, after your move")
	})
	if _, err := kat.e.SendText(ctx, lch.ID, "hello Lee, from c.test", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Lee gets it", func() bool { return strings.Contains(lee.run("sync"), "hello Lee, from c.test") })
	if _, err := kat.e.SendText(ctx, rch.ID, "hello Ravi, from c.test", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Ravi gets it", func() bool { ravi.sync(); return ravi.hasMessage(rch.ID, "hello Ravi, from c.test") })

	// Lee moves to a.test; Railcar follows.
	lee.run("move", o["a.test"], "lee", "--invite", testnet.SignupCode)
	settle()
	kat.sync()
	if _, err := kat.e.SendText(ctx, lch.ID, "hello Lee, at your new home", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Lee gets it at a.test", func() bool { return strings.Contains(lee.run("sync"), "hello Lee, at your new home") })
	lee.run("send", "kat.c.test", "from Lee's new home")
	until(t, "Kat hears from Lee's new home", func() bool { kat.sync(); return kat.hasMessage(lch.ID, "from Lee's new home") })
}

// hasMessage reports whether a chat has a message with this text.
func (r *railcar) hasMessage(chat, body string) bool {
	msgs, _ := r.e.Messages(chat)
	for _, m := range msgs {
		if m.Body == body {
			return true
		}
	}
	return false
}
