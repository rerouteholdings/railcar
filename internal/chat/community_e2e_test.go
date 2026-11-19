package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// cliJSON runs a reroute-cli community command and decodes its answer.
func (r *rerouteCLI) community(cmd string, args string, out any) {
	r.t.Helper()
	s := r.run("community", "run", cmd, args)
	if out != nil {
		if i := strings.IndexAny(s, "{[\"0123456789"); i >= 0 {
			s = s[i:]
		}
		if err := json.Unmarshal([]byte(s), out); err != nil {
			r.t.Fatalf("reroute-cli community %s: %v\n%s", cmd, err, s)
		}
	}
}

type cliMsg struct {
	Seq     uint64 `json:"seq"`
	Body    string `json:"body"`
	From    string `json:"from_handle"`
	Deleted bool   `json:"deleted"`
}

func (r *rerouteCLI) channelMessages(server string, channel uint64) []cliMsg {
	var res struct {
		Messages []cliMsg `json:"messages"`
	}
	r.community("messages", fmt.Sprintf(`{"server":%q,"channel":%d}`, server, channel), &res)
	return res.Messages
}

func (r *railcar) channel(server, name string) Chat {
	r.t.Helper()
	for _, c := range r.e.Chats() {
		if c.Kind == KindChannel && c.Community == server && c.Name == name {
			return c
		}
	}
	r.t.Fatalf("no #%s on %s in %+v", name, server, r.e.Chats())
	return Chat{}
}

func (r *railcar) bodies(chat string) []string {
	msgs, _ := r.e.Messages(chat)
	var out []string
	for _, m := range msgs {
		if !m.System && !m.Gap && m.Status != Failed {
			out = append(out, fmt.Sprintf("%d:%s", m.Seq, m.Body))
		}
	}
	return out
}

func TestE2EInteropCommunities(t *testing.T) {
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

	// Reroute creates a community; two messages go in before Ravi joins.
	kat.community("create", fmt.Sprintf(`{"server":"c.test","name":"Night shift","code":%q}`, testnet.CommunityCode), nil)
	var general uint64
	kat.community("add_channel", `{"server":"c.test","name":"general","topic":"Anything goes"}`, &general)
	kat.community("send", fmt.Sprintf(`{"server":"c.test","channel":%d,"text":"before ravi 1"}`, general), nil)
	kat.community("send", fmt.Sprintf(`{"server":"c.test","channel":%d,"text":"before ravi 2"}`, general), nil)
	var code string
	kat.community("make_code", `{"server":"c.test","days":7,"uses":5}`, &code)

	// Railcar joins with the code; Reroute sends the keys, after
	// uploading what came before.
	if _, err := ravi.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	until(t, "ravi gets the keys", func() bool {
		kat.run("community", "sync")
		ravi.sync()
		return hasChannel(ravi, "c.test", "general")
	})
	gen := ravi.channel("c.test", "general")
	if gen.Topic != "Anything goes" {
		t.Errorf("the topic: %q", gen.Topic)
	}
	if got := ravi.bodies(gen.ID); !slices.Contains(got, "1:before ravi 1") || !slices.Contains(got, "2:before ravi 2") {
		t.Fatalf("ravi's #general after the keys: %v", got)
	}

	// Messages both ways, with sequence numbers.
	if _, err := ravi.e.SendText(ctx, gen.ID, "hello from Railcar", ""); err != nil {
		t.Fatal(err)
	}
	kat.community("send", fmt.Sprintf(`{"server":"c.test","channel":%d,"text":"hello from Reroute"}`, general), nil)
	settle()
	kat.run("community", "sync")
	ravi.sync()
	if !slices.ContainsFunc(kat.channelMessages("c.test", general), func(m cliMsg) bool { return m.Seq == 3 && m.Body == "hello from Railcar" }) {
		t.Fatal("kat didn't get ravi's message as 3")
	}
	if got := ravi.bodies(gen.ID); !slices.Contains(got, "4:hello from Reroute") {
		t.Fatalf("ravi's #general: %v", got)
	}

	// Railcar creates a community on d.test, with a message before Max
	// joins; Max joins with a Railcar code, and Railcar sends the keys.
	if _, err := ravi.e.CreateCommunity(ctx, "d.test", "Signal box", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	dgen := ravi.channel("d.test", "general")
	if _, err := ravi.e.SendText(ctx, dgen.ID, "said before max", ""); err != nil {
		t.Fatal(err)
	}
	dcode, err := ravi.e.MakeCode(ctx, "d.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	max.community("join", fmt.Sprintf(`{"code":%q}`, dcode), nil)
	var view struct {
		NamesReady bool `json:"names_ready"`
		Channels   []struct {
			ID   uint64 `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	until(t, "max gets the keys", func() bool {
		ravi.sync()
		max.run("community", "sync")
		max.community("community", `{"server":"d.test"}`, &view)
		return view.NamesReady
	})
	if !view.NamesReady || len(view.Channels) == 0 || view.Channels[0].Name != "general" {
		t.Fatalf("max's view of d.test: %+v", view)
	}
	var hist struct {
		Added int   `json:"added"`
		Gaps  []any `json:"gaps"`
	}
	max.community("fetch_history", fmt.Sprintf(`{"server":"d.test","channel":%d}`, dgen.Channel), &hist)
	if !slices.ContainsFunc(max.channelMessages("d.test", dgen.Channel), func(m cliMsg) bool { return m.Seq == 1 && m.Body == "said before max" }) {
		t.Fatalf("max didn't read Railcar's history segment: %+v", hist)
	}
	max.community("send", fmt.Sprintf(`{"server":"d.test","channel":%d,"text":"max checking in"}`, dgen.Channel), nil)
	settle()
	ravi.sync()
	if got := ravi.bodies(dgen.ID); !slices.Contains(got, "2:max checking in") {
		t.Fatalf("ravi's d.test #general: %v", got)
	}

	// A restricted channel: Ravi gives Max a role, and adds a channel
	// only that role sees.
	role, err := ravi.e.AddRole(ctx, "d.test", "Crew", "#e0a030", []string{"talk"})
	if err != nil {
		t.Fatal(err)
	}
	maxID := strings.Fields(lastLineWith(max.run("v2", "whoami"), "@"))[1]
	if err := ravi.e.SetRoles(ctx, "d.test", maxID, []uint64{role}); err != nil {
		t.Fatal(err)
	}
	if err := ravi.e.SetRoles(ctx, "d.test", ravi.e.c.ID(), []uint64{role}); err != nil {
		t.Fatal(err)
	}
	back, err := ravi.e.AddChannel(ctx, "d.test", "backroom", "", ChannelOptions{See: []uint64{role}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, back, "crew only", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	max.run("community", "sync")
	_, bch, _ := parseChanID(back)
	if !slices.ContainsFunc(max.channelMessages("d.test", bch), func(m cliMsg) bool { return m.Body == "crew only" }) {
		t.Fatal("max didn't get the restricted channel's message")
	}
}

func lastLineWith(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}

func hasChannel(r *railcar, server, name string) bool {
	for _, c := range r.e.Chats() {
		if c.Kind == KindChannel && c.Community == server && c.Name == name {
			return true
		}
	}
	return false
}

// until retries a check, a second apart, for up to 20 seconds.
func until(t *testing.T, what string, f func() bool) {
	t.Helper()
	for range 20 {
		if f() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: never happened", what)
}

func TestE2ERailcarCommunities(t *testing.T) {
	bin := testnet.Rerouted(t)
	comms := map[string]string{"c.test": "a.test"}
	o := testnet.StartWith(t, bin, []string{"a.test", "b.test"}, comms)
	ctx := context.Background()
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	lee := newRailcar(t, o, "b.test", "lee")
	all := func() {
		settle()
		for range 2 {
			for _, r := range []*railcar{kat, ravi, lee} {
				r.sync()
			}
		}
	}
	if _, err := kat.e.CreateCommunity(ctx, "c.test", "Night shift", "After hours", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	gen := kat.channel("c.test", "general")
	code, err := kat.e.MakeCode(ctx, "c.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	until(t, "ravi gets the keys", func() bool { all(); return hasChannel(ravi, "c.test", "general") })
	// everyone holds only talk by default, so Ravi can't make codes
	// (§16.2.3).
	if _, err := ravi.e.MakeCode(ctx, "c.test", 7, 5); err == nil {
		t.Fatal("a member with only everyone's permissions made a code")
	}
	for _, s := range []string{"one", "two", "three"} {
		if _, err := kat.e.SendText(ctx, gen.ID, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	rm, err := ravi.e.SendText(ctx, gen.ID, "from ravi", "")
	if err != nil {
		t.Fatal(err)
	}
	all()
	if got := ravi.bodies(gen.ID); !slices.Equal(got, []string{"1:one", "2:two", "3:three", "4:from ravi"}) {
		t.Fatalf("ravi's #general: %v", got)
	}

	// Lee joins later, and reads what came before from history.
	if _, err := lee.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	until(t, "lee reads the history", func() bool {
		all()
		return hasChannel(lee, "c.test", "general") && slices.Contains(lee.bodies(gen.ID), "1:one") && slices.Contains(lee.bodies(gen.ID), "4:from ravi")
	})

	// The owner deletes Ravi's message; everyone sees it deleted.
	if err := kat.e.Delete(ctx, gen.ID, rm.ID); err != nil {
		t.Fatal(err)
	}
	all()
	msgs, _ := lee.e.Messages(gen.ID)
	if i := findMessage(msgs, rm.ID); i < 0 || !msgs[i].Deleted {
		t.Fatalf("lee's copy of the deleted message: %+v", msgs)
	}

	// Read positions: Ravi reads #general, and the server keeps it.
	if err := ravi.e.MarkRead(gen.ID); err != nil {
		t.Fatal(err)
	}
	pos, err := ravi.e.c.Community("c.test").ReadPositions(ctx)
	if err != nil || pos[gen.Channel] < 4 {
		t.Fatalf("ravi's read positions: %v %v", pos, err)
	}

	// A timeout: the community server refuses Ravi's message.
	if err := kat.e.Timeout(ctx, "c.test", ravi.e.c.ID(), time.Minute); err != nil {
		t.Fatal(err)
	}
	all()
	if _, err := ravi.e.SendText(ctx, gen.ID, "while timed out", ""); err == nil {
		t.Fatal("a timed-out member's message went through")
	}

	// A kick: Lee is out, and can't send.
	if err := kat.e.Kick(ctx, "c.test", lee.e.c.ID()); err != nil {
		t.Fatal(err)
	}
	all()
	if _, err := lee.e.SendText(ctx, gen.ID, "still here?", ""); err == nil {
		t.Fatal("a kicked member's message went through")
	}
	if _, err := kat.e.SendText(ctx, gen.ID, "after the kick", ""); err != nil {
		t.Fatal(err)
	}
	all()
	if !slices.Contains(ravi.bodies(gen.ID), "6:after the kick") {
		t.Fatalf("ravi after the kick: %v", ravi.bodies(gen.ID))
	}

	// Typing in a channel reaches a member on the live stream, and isn't
	// numbered or kept (§13.3).
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- ravi.e.Run(rctx) }()
	defer func() { cancel(); <-done }()
	got := false
	for i := 0; i < 20 && !got; i++ {
		if err := kat.e.SetTyping(ctx, gen.ID, true); err != nil {
			t.Fatal(err)
		}
		timeout := time.After(time.Second)
	wait:
		for {
			select {
			case ev := <-ravi.e.Events():
				if ev.Kind == EventTyping && ev.ChatID == gen.ID && ev.Who == kat.e.c.ID() && ev.On {
					got = true
					break wait
				}
			case <-timeout:
				break wait
			}
		}
	}
	if !got {
		t.Fatal("Kat's typing never reached Ravi")
	}
	if _, err := kat.e.SendText(ctx, gen.ID, "after typing", ""); err != nil {
		t.Fatal(err)
	}
	until(t, "ravi gets the message after typing", func() bool { return slices.Contains(ravi.bodies(gen.ID), "7:after typing") })
}
