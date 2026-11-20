package chat

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// TestE2EInteropCommunityMove: Reroute's owner moves a community to
// another community server (§16.6), and a Railcar member follows it:
// it keeps the channel and its history, and messages go both ways on the
// new server.
func TestE2EInteropCommunityMove(t *testing.T) {
	bin := testnet.Rerouted(t)
	comms := map[string]string{"c.test": "a.test", "e.test": "b.test"}
	o := testnet.StartWith(t, bin, []string{"a.test", "b.test"}, comms)
	env := []string{"REROUTE_COMMUNITY_URLS=" + testnet.CommunityURLs(o, comms)}
	ctx := context.Background()
	kat := newRerouteCLI(t, "kat")
	kat.env = env
	kat.run("v2", "register", "kat", "--server", o["a.test"], "--invite", testnet.SignupCode)
	ravi := newRailcar(t, o, "b.test", "ravi")

	kat.community("create", fmt.Sprintf(`{"server":"c.test","name":"Night shift","code":%q}`, testnet.CommunityCode), nil)
	var general uint64
	kat.community("add_channel", `{"server":"c.test","name":"general","topic":"Anything goes"}`, &general)
	var code string
	kat.community("make_code", `{"server":"c.test","days":7,"uses":5}`, &code)
	if _, err := ravi.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	until(t, "ravi gets the keys", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		return hasChannel(ravi, "c.test", "general")
	})
	gen := ravi.channel("c.test", "general")
	if _, err := ravi.e.SendText(ctx, gen.ID, "before the move", ""); err != nil {
		t.Fatal(err)
	}
	kat.run("community", "sync")

	kat.community("move", fmt.Sprintf(`{"server":"c.test","to":"e.test","code":%q}`, testnet.CommunityCode), nil)
	until(t, "ravi follows the move", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		_, ok := ravi.e.Community("e.test")
		return ok && hasChannel(ravi, "e.test", "general")
	})
	ngen := ravi.channel("e.test", "general")
	if !slices.Contains(ravi.bodies(ngen.ID), "1:before the move") {
		t.Fatalf("ravi's history after the move: %v", ravi.bodies(ngen.ID))
	}
	if _, err := ravi.e.SendText(ctx, ngen.ID, "after the move", ""); err != nil {
		t.Fatal(err)
	}
	kat.community("send", fmt.Sprintf(`{"server":"e.test","channel":%d,"text":"hello at the new server"}`, general), nil)
	until(t, "messages both ways on the new server", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		return slices.ContainsFunc(ravi.bodies(ngen.ID), func(s string) bool { return strings.HasSuffix(s, ":hello at the new server") }) &&
			slices.ContainsFunc(kat.channelMessages("e.test", general), func(m cliMsg) bool { return m.Body == "after the move" })
	})
}

// TestE2EInteropMoveCommunity: Railcar's owner moves a community to
// another community server (§16.6), with a Reroute member and a Railcar
// member, a restricted channel, and history. A move the new server refuses
// changes nothing. After the move, the members follow, numbering goes on
// from the old server's, and messages go every way on the new server, the
// restricted channel too.
func TestE2EInteropMoveCommunity(t *testing.T) {
	comms := map[string]string{"c.test": "a.test", "e.test": "b.test"}
	o := testnet.StartWith(t, testnet.Rerouted(t), []string{"a.test", "b.test"}, comms)
	env := []string{"REROUTE_COMMUNITY_URLS=" + testnet.CommunityURLs(o, comms)}
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	lee := newRailcar(t, o, "b.test", "lee")
	ravi := newRerouteCLI(t, "ravi")
	ravi.env = env
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	if _, err := kat.e.CreateCommunity(ctx, "c.test", "Night shift", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	gen := kat.channel("c.test", "general")
	code, err := kat.e.MakeCode(ctx, "c.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lee.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	ravi.community("join", fmt.Sprintf(`{"code":%q}`, code), nil)
	all := func() {
		settle()
		for range 2 {
			kat.sync()
			lee.sync()
			ravi.run("community", "sync")
		}
	}
	until(t, "lee gets the keys", func() bool { all(); return hasChannel(lee, "c.test", "general") })
	role, err := kat.e.AddRole(ctx, "c.test", "Crew", "", []string{"talk"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{kat.e.Client().ID(), lee.e.Client().ID()} {
		if err := kat.e.SetRoles(ctx, "c.test", id, []uint64{role}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := kat.e.AddChannel(ctx, "c.test", "backroom", "", ChannelOptions{See: []uint64{role}}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"one", "two"} {
		if _, err := kat.e.SendText(ctx, gen.ID, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	until(t, "lee sees the backroom", func() bool { all(); return hasChannel(lee, "c.test", "backroom") })

	// e.test refuses a wrong creation code: nothing changes.
	if err := kat.e.MoveCommunity(ctx, "c.test", "e.test", "wrong-code"); err == nil {
		t.Fatal("a move with a wrong creation code went through")
	}
	if cm := kat.e.st.Communities["c.test"]; cm == nil || cm.MovingTo != "" {
		t.Fatalf("after the refused move: %+v", cm)
	}
	if _, err := kat.e.SendText(ctx, gen.ID, "still on c.test", ""); err != nil {
		t.Fatal(err)
	}

	if err := kat.e.MoveCommunity(ctx, "c.test", "e.test", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	if _, ok := kat.e.Community("e.test"); !ok {
		t.Fatal("the owner didn't follow its own move")
	}
	ngen := kat.channel("e.test", "general")
	until(t, "the members follow", func() bool {
		all()
		_, ok := lee.e.Community("e.test")
		ravi.community("follow", `{"server":"c.test"}`, nil)
		return ok && hasChannel(lee, "e.test", "general") && hasChannel(lee, "e.test", "backroom")
	})
	if _, err := lee.e.SendText(ctx, ngen.ID, "lee on e.test", ""); err != nil {
		t.Fatal(err)
	}
	nback := kat.channel("e.test", "backroom")
	if _, err := kat.e.SendText(ctx, nback.ID, "crew on e.test", ""); err != nil {
		t.Fatal(err)
	}
	sent := false
	until(t, "messages every way on e.test", func() bool {
		all()
		if !sent {
			sent = ravi.try("community", "run", "send", fmt.Sprintf(`{"server":"e.test","channel":%d,"text":"ravi on e.test"}`, gen.Channel)) == nil
		}
		return slices.Contains(kat.bodies(ngen.ID), "4:lee on e.test") &&
			slices.ContainsFunc(kat.bodies(ngen.ID), func(s string) bool { return strings.HasSuffix(s, ":ravi on e.test") }) &&
			slices.ContainsFunc(ravi.channelMessages("e.test", gen.Channel), func(m cliMsg) bool { return m.Body == "lee on e.test" }) &&
			slices.ContainsFunc(lee.bodies(nback.ID), func(s string) bool { return strings.HasSuffix(s, ":crew on e.test") })
	})
	if got := kat.bodies(ngen.ID); !slices.Contains(got, "1:one") || !slices.Contains(got, "3:still on c.test") {
		t.Errorf("kat's #general after the move: %v", got)
	}
}

// try runs reroute-cli like run, and returns its error instead of failing
// the test, for a command that may not work yet.
func (r *rerouteCLI) try(args ...string) error {
	cmd := exec.Command(r.bin, append([]string{"--profile", r.profile}, positional(args)...)...)
	cmd.Env = append(os.Environ(), r.env...)
	out, err := cmd.CombinedOutput()
	r.t.Logf("$ reroute-cli %s\n%s", strings.Join(args, " "), out)
	return err
}
