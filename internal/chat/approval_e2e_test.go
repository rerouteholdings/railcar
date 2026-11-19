package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// TestE2EInteropApprovalJoins: joins that need approval (§16.6), both
// ways. Reroute approves Railcar's request into its community, and
// Railcar approves Reroute's into its own. A Railcar member checks the
// approval in each commit.
func TestE2EInteropApprovalJoins(t *testing.T) {
	bin := testnet.Rerouted(t)
	comms := map[string]string{"c.test": "a.test", "d.test": "b.test"}
	o := testnet.StartWith(t, bin, []string{"a.test", "b.test"}, comms)
	env := []string{"REROUTE_COMMUNITY_URLS=" + testnet.CommunityURLs(o, comms)}
	ctx := context.Background()
	kat := newRerouteCLI(t, "kat")
	max := newRerouteCLI(t, "max")
	kat.env, max.env = env, env
	kat.run("v2", "register", "kat", "--server", o["a.test"], "--invite", testnet.SignupCode)
	max.run("v2", "register", "max", "--server", o["a.test"], "--invite", testnet.SignupCode)
	ravi := newRailcar(t, o, "b.test", "ravi")
	lee := newRailcar(t, o, "b.test", "lee")

	// Reroute's community needs approval; Railcar asks, and Reroute
	// approves.
	kat.community("create", fmt.Sprintf(`{"server":"c.test","name":"Night shift","code":%q}`, testnet.CommunityCode), nil)
	var general uint64
	kat.community("add_channel", `{"server":"c.test","name":"general","topic":"Anything goes"}`, &general)
	var code string
	kat.community("make_code", `{"server":"c.test","days":7,"uses":5}`, &code)
	// Lee joins first with the code, and later checks Reroute's approval.
	if _, err := lee.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	until(t, "lee gets the keys", func() bool { kat.run("community", "sync"); lee.sync(); return hasChannel(lee, "c.test", "general") })
	kat.community("set_settings", `{"server":"c.test","public":false,"approval":true}`, nil)
	if _, err := ravi.e.JoinCommunity(ctx, code); !errors.Is(err, ErrAskedToJoin) {
		t.Fatalf("joining a community that needs approval: %v", err)
	}
	kat.run("community", "sync")
	out := kat.run("community", "run", "requests", `{"server":"c.test"}`)
	if !strings.Contains(out, ravi.e.c.ID()) {
		t.Fatalf("Reroute doesn't list Ravi's request: %s", out)
	}
	kat.community("approve", fmt.Sprintf(`{"server":"c.test","id":%q}`, ravi.e.c.ID()), nil)
	until(t, "ravi is in, with the keys", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		return hasChannel(ravi, "c.test", "general")
	})
	gen := ravi.channel("c.test", "general")
	kat.community("send", fmt.Sprintf(`{"server":"c.test","channel":%d,"text":"welcome, Ravi"}`, general), nil)
	if _, err := ravi.e.SendText(ctx, gen.ID, "thanks for letting me in", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "messages both ways", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		return slices.ContainsFunc(ravi.bodies(gen.ID), func(s string) bool { return strings.HasSuffix(s, ":welcome, Ravi") }) &&
			slices.ContainsFunc(kat.channelMessages("c.test", general), func(m cliMsg) bool { return m.Body == "thanks for letting me in" })
	})
	until(t, "lee hears ravi", func() bool {
		lee.sync()
		return slices.ContainsFunc(lee.bodies(lee.channel("c.test", "general").ID), func(s string) bool { return strings.HasSuffix(s, ":thanks for letting me in") })
	})
	if c, _ := lee.e.Community("c.test"); c.Stopped != "" {
		t.Fatalf("lee refused Reroute's approval commit: %s", c.Stopped)
	}

	// Railcar's community needs approval; Reroute asks, Railcar approves,
	// and Lee, a Railcar member, checks the approval in the commit.
	if _, err := ravi.e.CreateCommunity(ctx, "d.test", "Signal box", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	dgen := ravi.channel("d.test", "general")
	dcode, err := ravi.e.MakeCode(ctx, "d.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lee.e.JoinCommunity(ctx, dcode); err != nil {
		t.Fatal(err)
	}
	until(t, "lee gets the keys", func() bool { ravi.sync(); lee.sync(); return hasChannel(lee, "d.test", "general") })
	if err := ravi.e.SetApproval(ctx, "d.test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, dgen.ID, "said before max", ""); err != nil {
		t.Fatal(err)
	}
	max.community("ask", fmt.Sprintf(`{"server":"d.test","code":%q}`, dcode), nil)
	var ids []string
	until(t, "ravi sees max's request", func() bool {
		ravi.sync()
		ids, err = ravi.e.JoinRequests(ctx, "d.test")
		return err == nil && len(ids) == 1
	})
	if err := ravi.e.ApproveJoin(ctx, "d.test", ids[0]); err != nil {
		t.Fatal(err)
	}
	var view struct {
		NamesReady bool `json:"names_ready"`
	}
	until(t, "max is in, with the keys", func() bool {
		max.run("community", "sync")
		max.community("community", `{"server":"d.test"}`, &view)
		return view.NamesReady
	})
	max.community("send", fmt.Sprintf(`{"server":"d.test","channel":%d,"text":"max checking in"}`, dgen.Channel), nil)
	until(t, "ravi and lee hear max", func() bool {
		ravi.sync()
		lee.sync()
		in := func(r *railcar) bool {
			return slices.ContainsFunc(r.bodies(dgen.ID), func(s string) bool { return strings.HasSuffix(s, ":max checking in") })
		}
		return in(ravi) && in(lee)
	})
	if c, _ := lee.e.Community("d.test"); c.Stopped != "" {
		t.Fatalf("lee refused the approval commit: %s", c.Stopped)
	}

	// Ravi kicks Lee and rotates the metadata key (§16.2.4): Reroute gets
	// the new key and still reads the channel's name.
	before := len(ravi.e.st.Communities["d.test"].MetaKeys)
	if err := ravi.e.Kick(ctx, "d.test", lee.e.c.ID()); err != nil {
		t.Fatal(err)
	}
	if n := len(ravi.e.st.Communities["d.test"].MetaKeys); n != before+1 {
		t.Fatalf("the metadata keys after the kick: %d, then %d", before, n)
	}
	var after struct {
		NamesReady bool `json:"names_ready"`
		Channels   []struct {
			Name string `json:"name"`
		} `json:"channels"`
	}
	until(t, "max reads the names under the new key", func() bool {
		max.run("community", "sync")
		max.community("community", `{"server":"d.test"}`, &after)
		return after.NamesReady && len(after.Channels) > 0 && after.Channels[0].Name == "general"
	})
}
