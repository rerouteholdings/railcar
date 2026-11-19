package chat

import (
	"context"
	"fmt"
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
