package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/testnet"
)

func voiceOf(e *Engine) VoiceInfo {
	v, _ := e.Voice()
	return v
}

// keyedOthers counts the other people in this device's voice channel
// whose keys it has.
func keyedOthers(e *Engine) int {
	n := 0
	for _, p := range voiceOf(e).People {
		if !p.Self && p.Keyed {
			n++
		}
	}
	return n
}

func (c *caller) channelChat(server, name string) string {
	c.t.Helper()
	var id string
	waitFor(c.t, "#"+name, func() bool {
		for _, ch := range c.e.Chats() {
			if ch.Kind == KindChannel && ch.Community == server && ch.Name == name {
				id = ch.ID
				return true
			}
		}
		return false
	})
	return id
}

// TestE2ERailcarVoice has two Railcar identities meet in a voice channel
// through the community server's SFU (§16.4): each hears the other's
// tone, decrypted from SFrame under the other's voice key; a mute shows;
// a third joins late and gets both keys sent again; and leaving works.
func TestE2ERailcarVoice(t *testing.T) {
	bin := testnet.Rerouted(t)
	comms := map[string]string{"c.test": "a.test"}
	o := testnet.StartWith(t, bin, []string{"a.test", "b.test"}, comms)
	ctx := context.Background()
	kat := newCaller(t, ctx, o, "a.test", "kat", 440)
	ravi := newCaller(t, ctx, o, "b.test", "ravi", 660)
	if _, err := kat.e.CreateCommunity(ctx, "c.test", "Night shift", "", testnet.CommunityCode); err != nil {
		t.Fatal(err)
	}
	lounge, err := kat.e.AddChannel(ctx, "c.test", "Lounge", "", ChannelOptions{Voice: true})
	if err != nil {
		t.Fatal(err)
	}
	code, err := kat.e.MakeCode(ctx, "c.test", 7, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	if got := ravi.channelChat("c.test", "Lounge"); got != lounge {
		t.Fatalf("Ravi's Lounge is %s, not %s", got, lounge)
	}

	// Relay-only calls don't cover voice channels: with no relay on this
	// network, Kat still joins, directly (§15.2).
	if err := kat.e.SetDirectCalls(false); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.JoinVoice(ctx, lounge); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat's visit", func() bool { return voiceOf(kat.e).Joined })
	if err := ravi.e.JoinVoice(ctx, lounge); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both keys", func() bool { return keyedOthers(kat.e) == 1 && keyedOthers(ravi.e) == 1 })
	time.Sleep(2 * time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660}, 440)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440}, 660)
	waitFor(t, "Ravi speaking, for Kat", func() bool {
		for _, p := range voiceOf(kat.e).People {
			if p.ID == ravi.e.Client().ID() && p.Level > 0.05 {
				return true
			}
		}
		return false
	})

	// Ravi mutes, and Kat sees it.
	if err := ravi.e.SetVoiceMuted(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat to see Ravi muted", func() bool {
		for _, p := range voiceOf(kat.e).People {
			if p.ID == ravi.e.Client().ID() {
				return p.Muted
			}
		}
		return false
	})
	if err := ravi.e.SetVoiceMuted(false); err != nil {
		t.Fatal(err)
	}

	// Lee joins late: Kat and Ravi send their keys again.
	lee := newCaller(t, ctx, o, "b.test", "lee", 880)
	if _, err := lee.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	lee.channelChat("c.test", "Lounge")
	if err := lee.e.JoinVoice(ctx, lounge); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Lee to get both keys", func() bool { return keyedOthers(lee.e) == 2 })
	time.Sleep(2 * time.Second)
	hears(t, "lee", lee.tone.record(2*time.Second), []float64{440, 660}, 880)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660, 880}, 440)

	// Kat leaves; the others see it.
	if err := kat.e.LeaveVoice(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the others to see Kat leave", func() bool { return len(voiceOf(ravi.e).People) == 2 && len(voiceOf(lee.e).People) == 2 })
	if _, ok := kat.e.Voice(); ok {
		t.Error("Kat still has a voice channel")
	}
	if c, _ := kat.e.Chat(lounge); len(c.InVoice) != 2 {
		t.Errorf("Kat's list shows %d in Lounge, not 2", len(c.InVoice))
	}

	// Kat kicks Lee: his leaves leave the members group, which carries
	// Lounge, so Ravi replaces his voice key with generation 1 (§16.4).
	// Kat joins again, and hears Ravi under it.
	if err := kat.e.Kick(ctx, "c.test", lee.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	gen := func() uint8 {
		var g uint8
		ravi.e.voice.doWait(func() error {
			if c := ravi.e.voice.cur; c != nil {
				g = c.gen
			}
			return nil
		})
		return g
	}
	waitFor(t, "Ravi's new voice key", func() bool { return gen() == 1 })
	if err := kat.e.JoinVoice(ctx, lounge); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat to get Ravi's new key", func() bool { return keyedOthers(kat.e) == 1 })
	time.Sleep(2 * time.Second)
	hears(t, "kat, after Ravi's new key", kat.tone.record(2*time.Second), []float64{660}, 440)
}

// TestE2EInteropVoice has Railcar and Reroute's command-line client meet
// in a voice channel through the community server's SFU, in a community
// Reroute created, and each hears the other.
func TestE2EInteropVoice(t *testing.T) {
	bin := testnet.Rerouted(t)
	comms := map[string]string{"c.test": "a.test"}
	o := testnet.StartWith(t, bin, []string{"a.test", "b.test"}, comms)
	env := []string{"REROUTE_COMMUNITY_URLS=" + testnet.CommunityURLs(o, comms)}
	ctx := context.Background()
	kat := newRerouteCLI(t, "kat")
	kat.env = env
	kat.run("v2", "register", "kat", "--server", o["a.test"], "--invite", testnet.SignupCode)
	ravi := newCaller(t, ctx, o, "b.test", "ravi", 660)
	dir := t.TempDir()
	writeTone(t, filepath.Join(dir, "kat.wav"), 440, 20)

	kat.community("create", fmt.Sprintf(`{"server":"c.test","name":"Night shift","code":%q}`, testnet.CommunityCode), nil)
	var lounge uint64
	kat.community("add_channel", `{"server":"c.test","name":"Lounge","voice":true,"max":10,"cast":1}`, &lounge)
	var code string
	kat.community("make_code", `{"server":"c.test","days":7,"uses":5}`, &code)
	if _, err := ravi.e.JoinCommunity(ctx, code); err != nil {
		t.Fatal(err)
	}
	var chat string
	for range 20 {
		kat.run("community", "sync")
		if hasChannel(ravi.railcar, "c.test", "Lounge") {
			chat = ravi.channelChat("c.test", "Lounge")
			break
		}
		time.Sleep(time.Second)
	}
	if chat == "" {
		t.Fatal("Ravi never got the channel names")
	}

	// Railcar is in Lounge first; Reroute joins, and each hears the other.
	if err := ravi.e.JoinVoice(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ravi's visit", func() bool { return voiceOf(ravi.e).Joined })
	heard := filepath.Join(dir, "kat-heard.wav")
	wait := kat.start("community", "voice", "c.test", fmt.Sprint(lounge), "--play", filepath.Join(dir, "kat.wav"), "--record", heard, "--seconds", "8")
	waitFor(t, "Reroute's key", func() bool { return keyedOthers(ravi.e) == 1 })
	time.Sleep(2 * time.Second)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440}, 660)
	wait()
	if s := readWAV(t, heard); audio.Power(s, 660) < 1 || audio.Power(s, 660) < 50*audio.Power(s, 440) {
		t.Errorf("Reroute heard %.3g of Railcar's 660 Hz", audio.Power(s, 660))
	}
	waitFor(t, "Reroute to leave", func() bool { return len(voiceOf(ravi.e).People) == 1 })

	// Reroute is in Lounge first, muted by deafening; Railcar joins late,
	// and gets its key sent again.
	wait = kat.start("community", "voice", "c.test", fmt.Sprint(lounge), "--play", filepath.Join(dir, "kat.wav"), "--record", heard, "--seconds", "10", "--deafen")
	if err := ravi.e.LeaveVoice(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Reroute in Lounge", func() bool {
		c, _ := ravi.e.Chat(chat)
		return len(c.InVoice) == 1
	})
	if err := ravi.e.JoinVoice(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Reroute's key, sent again", func() bool { return keyedOthers(ravi.e) == 1 })
	waitFor(t, "Reroute deafened", func() bool {
		for _, p := range voiceOf(ravi.e).People {
			if !p.Self {
				return p.Deafened && p.Muted
			}
		}
		return false
	})
	wait()
}
