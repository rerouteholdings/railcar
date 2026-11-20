package chat

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// TestE2ERailcarLinkCommunities links a second Railcar device to an
// identity in a community (§7.2 step 5): the link bundle names the
// community, the new device leaves key packages with its server, and the
// first device adds it to the members group and a restricted channel's
// group and sends the keys. The new device reads and writes there with no
// code.
func TestE2ERailcarLinkCommunities(t *testing.T) {
	was := claimEvery
	claimEvery = 0
	t.Cleanup(func() { claimEvery = was })
	o := testnet.StartWith(t, testnet.Rerouted(t), []string{"a.test", "b.test"}, map[string]string{"c.test": "a.test"})
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	if _, err := ravi.e.CreateCommunity(ctx, "c.test", "Night shift", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	gen := ravi.channel("c.test", "general")
	code, err := ravi.e.MakeCode(ctx, "c.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	all := func(rs ...*railcar) {
		settle()
		for range 2 {
			for _, r := range rs {
				r.sync()
			}
		}
	}
	until(t, "kat gets the keys", func() bool { all(kat, ravi); return hasChannel(kat, "c.test", "general") })
	role, err := ravi.e.AddRole(ctx, "c.test", "Crew", "", []string{"talk"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{kat.e.Client().ID(), ravi.e.Client().ID()} {
		if err := ravi.e.SetRoles(ctx, "c.test", id, []uint64{role}); err != nil {
			t.Fatal(err)
		}
	}
	back, err := ravi.e.AddChannel(ctx, "c.test", "backroom", "", ChannelOptions{See: []uint64{role}})
	if err != nil {
		t.Fatal(err)
	}
	until(t, "kat sees the backroom", func() bool { all(kat, ravi); return hasChannel(kat, "c.test", "backroom") })

	kat2 := linkRailcar(t, o, kat, "a.test", "kat.a.test")
	until(t, "the new device gets the keys", func() bool {
		all(kat, kat2, ravi)
		return hasChannel(kat2, "c.test", "general") && hasChannel(kat2, "c.test", "backroom")
	})
	if cm := kat2.e.st.Communities["c.test"]; cm == nil || cm.Linking || len(cm.Groups) != 2 {
		t.Fatalf("the new device's community: %+v", cm)
	}
	if _, err := kat2.e.SendText(ctx, gen.ID, "from kat's second device", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, back, "crew only, after the new device", ""); err != nil {
		t.Fatal(err)
	}
	all(kat, kat2, ravi)
	if !slices.ContainsFunc(ravi.bodies(gen.ID), func(s string) bool { return strings.HasSuffix(s, ":from kat's second device") }) {
		t.Errorf("ravi's #general: %v", ravi.bodies(gen.ID))
	}
	if !slices.ContainsFunc(kat2.bodies(back), func(s string) bool { return strings.HasSuffix(s, ":crew only, after the new device") }) {
		t.Errorf("the new device's #backroom: %v", kat2.bodies(back))
	}
}
