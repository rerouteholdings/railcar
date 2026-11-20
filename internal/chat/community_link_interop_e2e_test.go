package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// TestE2EInteropLinkCommunities links devices into a community between
// Railcar and Reroute (§7.2 step 5). A Reroute identity in a community
// links a Railcar device, which joins the community from the link bundle
// and reads there once Reroute adds it; then a Railcar identity in the
// community links a Reroute device, which Railcar adds.
func TestE2EInteropLinkCommunities(t *testing.T) {
	was := claimEvery
	claimEvery = 0
	t.Cleanup(func() { claimEvery = was })
	comms := map[string]string{"c.test": "a.test"}
	o := testnet.StartWith(t, testnet.Rerouted(t), []string{"a.test", "b.test"}, comms)
	env := []string{"REROUTE_COMMUNITY_URLS=" + testnet.CommunityURLs(o, comms)}
	ctx := context.Background()
	opts := client.Options{Origins: o, DeviceName: "Railcar"}
	kat := newRailcar(t, o, "b.test", "kat")
	if _, err := kat.e.CreateCommunity(ctx, "c.test", "Night shift", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	gen := kat.channel("c.test", "general")
	code, err := kat.e.MakeCode(ctx, "c.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	ravi := newRerouteCLI(t, "ravi")
	ravi.env = env
	ravi.run("v2", "register", "ravi", "--server", o["a.test"], "--invite", testnet.SignupCode)
	ravi.community("join", fmt.Sprintf(`{"code":%q}`, code), nil)
	until(t, "ravi is in the community", func() bool {
		kat.sync()
		ravi.run("community", "sync")
		if _, err := kat.e.SendText(ctx, gen.ID, "welcome, Ravi", ""); err != nil {
			t.Fatal(err)
		}
		settle()
		ravi.run("community", "sync")
		return slices.ContainsFunc(ravi.channelMessages("c.test", gen.Channel), func(m cliMsg) bool { return m.Body == "welcome, Ravi" })
	})

	// Reroute's Ravi links a Railcar device.
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
	rc.sync()
	if cm := rc.e.st.Communities["c.test"]; cm == nil {
		t.Fatal("Railcar didn't take the community from Reroute's link bundle")
	}
	until(t, "Reroute adds its Railcar device to the community", func() bool {
		ravi.run("sync")
		ravi.run("community", "sync")
		kat.sync()
		rc.sync()
		return hasChannel(rc, "c.test", "general")
	})
	if _, err := rc.e.SendText(ctx, gen.ID, "from Ravi's Railcar", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	if !slices.ContainsFunc(kat.bodies(gen.ID), func(s string) bool { return strings.HasSuffix(s, ":from Ravi's Railcar") }) {
		t.Errorf("kat's #general: %v", kat.bodies(gen.ID))
	}

	// Railcar's Kat links a Reroute device, which Railcar adds.
	desk := newRerouteCLI(t, "kat-desk")
	desk.env = env
	linkCode := line(t, desk.run("v2", "link-start", "kat.b.test", "--server", o["b.test"]), "add-device ")
	a, err := kat.e.Client().ApproveLinkCode(ctx, linkCode)
	if err != nil {
		t.Fatal(err)
	}
	desk.run("v2", "link-confirm", a.Confirmation())
	for range 20 {
		if ok, err := a.Check(ctx); ok || err != nil {
			break
		}
		settle()
	}
	if err := kat.e.AddDevice(ctx, a.Device(), "Kat's desk"); err != nil {
		t.Fatal(err)
	}
	desk.run("v2", "link-finish")
	kat.syncDevices(func() { desk.run("sync"); desk.run("community", "sync") })
	if _, err := kat.e.SendText(ctx, gen.ID, "to Kat's desk", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "Kat's Reroute device reads the community", func() bool {
		desk.run("community", "sync")
		return slices.ContainsFunc(desk.channelMessages("c.test", gen.Channel), func(m cliMsg) bool { return m.Body == "to Kat's desk" })
	})
}
