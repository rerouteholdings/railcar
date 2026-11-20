package chat

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/proxy"
	"github.com/rerouteholdings/railcar/internal/proxy/proxytest"
	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// toneAudio gives a test identity a tone for a microphone, and keeps
// each call's mixer so the test can record what it hears.
type toneAudio struct {
	hz float64
	mu sync.Mutex
	mx *audio.Mixer
}

func (a *toneAudio) open() (AudioIO, error) {
	mix := audio.NewMixer()
	a.mu.Lock()
	a.mx = mix
	a.mu.Unlock()
	return AudioIO{Source: &audio.Tone{Hz: a.hz, Volume: 0.3}, Mix: mix, Close: func() {}}, nil
}

// record records what the identity hears for d.
func (a *toneAudio) record(d time.Duration) []int16 {
	a.mu.Lock()
	mx := a.mx
	a.mu.Unlock()
	r := audio.Record(mx)
	time.Sleep(d)
	r.Stop()
	return r.Samples()
}

// caller is a Railcar identity for call tests: on the live stream, with
// a tone for a microphone.
type caller struct {
	*railcar
	tone *toneAudio
}

func newCaller(t *testing.T, ctx context.Context, o map[string]string, server, handle string, hz float64) *caller {
	t.Helper()
	a := &toneAudio{hz: hz}
	r := newRailcarWith(t, o, server, handle, Options{Calls: CallOptions{Audio: a.open, Loopback: true}})
	if err := r.e.SetDirectCalls(true); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- r.e.Run(cctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitEvent(t, r.e, "the stream", func(ev Event) bool { return ev.Kind == EventStream && ev.Stream == client.EventConnected })
	return &caller{r, a}
}

// callFriends makes two live Railcar identities contacts, and returns the
// direct chat's ID.
func callFriends(t *testing.T, ctx context.Context, a, b *caller) string {
	t.Helper()
	code, err := b.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, b.e, "the request", func(ev Event) bool {
		return ev.Kind == EventContact && ev.Contact.State == StatePendingIn
	})
	ch, err := b.e.Accept(ctx, ev.Contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the chat", func() bool { _, ok := a.e.Chat(ch.ID); return ok })
	return ch.ID
}

func phase(e *Engine) CallPhase {
	c, ok := e.Call()
	if !ok {
		return ""
	}
	return c.Phase
}

// hears checks that a recording holds each of want, and none of not.
func hears(t *testing.T, who string, s []int16, want []float64, not float64) {
	t.Helper()
	n := audio.Power(s, not)
	for _, hz := range want {
		if p := audio.Power(s, hz); p < 50*n || p < 1 {
			t.Errorf("%s heard %.3g of %g Hz, and %.3g of their own %g Hz", who, p, hz, n, not)
		}
	}
}

func hasLine(t *testing.T, e *Engine, chat, prefix string) bool {
	msgs, err := e.Messages(chat)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.System && strings.HasPrefix(m.Body, prefix) {
			return true
		}
	}
	return false
}

// TestE2ERailcarCalls has Railcar identities on two servers call each
// other (§15.2): a call where each hears the other's tone through SFrame,
// with mute state; a declined call; a missed call; and a group call of
// three, a full mesh, that goes on when the caller leaves.
func TestE2ERailcarCalls(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	kat := newCaller(t, ctx, o, "a.test", "kat", 440)
	ravi := newCaller(t, ctx, o, "b.test", "ravi", 660)
	chat := callFriends(t, ctx, kat, ravi)

	// Kat calls; Ravi's device rings, and he answers.
	if err := kat.e.StartCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ravi's ring", func() bool { return phase(ravi.e) == CallRinging })
	if c, _ := ravi.e.Call(); c.From != kat.e.Client().ID() || !c.Incoming {
		t.Fatalf("the ring: %+v", c)
	}
	if err := ravi.e.AnswerCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to connect", func() bool { return phase(kat.e) == CallActive && phase(ravi.e) == CallActive })
	time.Sleep(time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660}, 440)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440}, 660)

	// Ravi mutes; Kat sees it, and hears silence.
	if err := ravi.e.SetCallMuted(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat to see Ravi muted", func() bool {
		c, _ := kat.e.Call()
		return len(c.People) == 1 && c.People[0].Muted
	})
	time.Sleep(500 * time.Millisecond)
	if p := audio.Power(kat.tone.record(time.Second), 660); p > 0.01 {
		t.Errorf("Ravi is muted, and Kat hears %g", p)
	}
	if err := ravi.e.SetCallMuted(false); err != nil {
		t.Fatal(err)
	}

	// Kat hangs up, and the call ends for Ravi.
	if err := kat.e.HangUp(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to end for Ravi", func() bool { p := phase(ravi.e); return p == CallEnded || p == "" })
	waitFor(t, "the call lines", func() bool {
		return hasLine(t, kat.e, chat, "Call, 0:0") && hasLine(t, ravi.e, chat, "Call, 0:0")
	})

	// Ravi declines a call, and Kat sees it declined.
	waitFor(t, "the calls to clear", func() bool { return phase(kat.e) == "" && phase(ravi.e) == "" })
	if err := kat.e.StartCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ravi's ring", func() bool { return phase(ravi.e) == CallRinging })
	if err := ravi.e.DeclineCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat to see the decline", func() bool { return hasLine(t, kat.e, chat, "@ravi.b.test declined the call") })

	// Ravi calls and gives up: Kat's device shows a missed call.
	waitFor(t, "the calls to clear", func() bool { return phase(kat.e) == "" && phase(ravi.e) == "" })
	if err := ravi.e.StartCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Kat's ring", func() bool { return phase(kat.e) == CallRinging })
	if err := ravi.e.HangUp(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a missed call", func() bool { return hasLine(t, kat.e, chat, "Missed call from @ravi.b.test") })

	// A group call of three: everyone hears both others.
	lee := newCaller(t, ctx, o, "a.test", "lee", 880)
	callFriends(t, ctx, kat, lee)
	g, err := kat.e.CreateGroup(ctx, "Night shift", []string{ravi.e.Client().ID(), lee.e.Client().ID()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the group", func() bool {
		_, r := ravi.e.Chat(g.ID)
		_, l := lee.e.Chat(g.ID)
		return r && l
	})
	waitFor(t, "the calls to clear", func() bool { return phase(kat.e) == "" && phase(ravi.e) == "" })
	if err := kat.e.StartCall(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both rings", func() bool { return phase(ravi.e) == CallRinging && phase(lee.e) == CallRinging })
	if err := ravi.e.AnswerCall(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := lee.e.AnswerCall(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	connected := func(e *Engine, n int) func() bool {
		return func() bool {
			c, ok := e.Call()
			got := 0
			for _, p := range c.People {
				if p.Connected {
					got++
				}
			}
			return ok && c.Phase == CallActive && got == n
		}
	}
	waitFor(t, "the mesh", func() bool { return connected(kat.e, 2)() && connected(ravi.e, 2)() && connected(lee.e, 2)() })
	time.Sleep(time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660, 880}, 440)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440, 880}, 660)
	hears(t, "lee", lee.tone.record(2*time.Second), []float64{440, 660}, 880)

	// Kat leaves; Ravi and Lee carry on.
	if err := kat.e.HangUp(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the others to see Kat go", func() bool { return connected(ravi.e, 1)() && connected(lee.e, 1)() })
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{880}, 660)
	if n := kat.e.CallIn(g.ID); n != 2 {
		t.Errorf("Kat sees %d devices in the call, not 2", n)
	}
	ravi.e.HangUp(ctx)
	waitFor(t, "the call to end for Lee", func() bool { p := phase(lee.e); return p == CallEnded || p == "" })
}

// start runs a reroute-cli command in the background, and returns a
// function that waits for it and returns its output.
func (r *rerouteCLI) start(args ...string) func() string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, r.bin, append([]string{"--profile", r.profile}, args...)...)
	cmd.Env = append(os.Environ(), r.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	return func() string {
		r.t.Helper()
		err := cmd.Wait()
		cancel()
		r.t.Logf("$ reroute-cli %s\n%s", strings.Join(args, " "), out.String())
		if err != nil {
			r.t.Errorf("reroute-cli %s: %v", args[0], err)
		}
		return out.String()
	}
}

// writeTone writes secs seconds of a tone as a 48 kHz, 16-bit, mono WAV
// file, as reroute-cli's --play takes.
func writeTone(t *testing.T, path string, hz float64, secs int) {
	t.Helper()
	n := audio.SampleRate * secs
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+2*n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(audio.SampleRate), uint32(2 * audio.SampleRate), uint16(2), uint16(16)} {
		binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(2*n))
	for i := range n {
		binary.Write(&b, binary.LittleEndian, int16(0.3*32767*math.Sin(2*math.Pi*hz*float64(i)/audio.SampleRate)))
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readWAV reads the samples of a 16-bit WAV file reroute-cli recorded.
func readWAV(t *testing.T, path string) []int16 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b, []byte("data"))
	if i < 0 {
		t.Fatalf("%s isn't a WAV file", path)
	}
	b = b[i+8:]
	s := make([]int16, len(b)/2)
	for j := range s {
		s[j] = int16(binary.LittleEndian.Uint16(b[2*j:]))
	}
	return s
}

// TestE2EInteropCalls calls between Railcar and Reroute's command-line
// client, across two servers (§15.2): each way, each hears the other's
// tone, decrypted from SFrame; Reroute sees Railcar's mute state, and
// Railcar sees Reroute's.
func TestE2EInteropCalls(t *testing.T) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	dir := t.TempDir()
	writeTone(t, filepath.Join(dir, "ravi.wav"), 660, 12)

	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	kat := newCaller(t, ctx, o, "a.test", "kat", 440)
	if _, err := kat.e.Add(ctx, lastLine(ravi.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", "kat.a.test")
	var chat string
	waitFor(t, "the chat", func() bool {
		for _, c := range kat.e.Contacts() {
			if c.Handle == "ravi.b.test" && c.Chat != "" {
				chat = c.Chat
				return true
			}
		}
		return false
	})

	// Railcar calls; Reroute answers.
	heard := filepath.Join(dir, "ravi-heard.wav")
	wait := ravi.start("answer", "--play", filepath.Join(dir, "ravi.wav"), "--record", heard, "--seconds", "6", "--direct", "--muted", "--wait", "40")
	time.Sleep(2 * time.Second)
	if err := kat.e.StartCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to connect", func() bool { return phase(kat.e) == CallActive })
	waitFor(t, "Kat to see Reroute muted", func() bool {
		c, _ := kat.e.Call()
		return len(c.People) == 1 && c.People[0].Muted
	})
	wait()
	waitFor(t, "the call to end", func() bool { p := phase(kat.e); return p == CallEnded || p == "" })
	if s := readWAV(t, heard); audio.Power(s, 440) < 50*audio.Power(s, 660) || audio.Power(s, 440) < 1 {
		t.Errorf("Reroute heard %.3g of Railcar's 440 Hz", audio.Power(s, 440))
	}
	waitFor(t, "the call line", func() bool { return hasLine(t, kat.e, chat, "Call, 0:0") })

	// Reroute calls; Railcar answers, hears it, and mutes.
	waitFor(t, "the call to clear", func() bool { return phase(kat.e) == "" })
	wait = ravi.start("call", "kat.a.test", "--play", filepath.Join(dir, "ravi.wav"), "--record", heard, "--seconds", "8", "--direct")
	defer func() {
		if t.Failed() && wait != nil {
			wait()
		}
	}()
	waitFor(t, "Railcar's ring", func() bool { return phase(kat.e) == CallRinging })
	if err := kat.e.AnswerCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to connect", func() bool { return phase(kat.e) == CallActive })
	time.Sleep(time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660}, 440)
	if err := kat.e.SetCallMuted(true); err != nil {
		t.Fatal(err)
	}
	out := wait()
	wait = nil
	if !strings.Contains(out, "muted in the call: @kat.a.test") {
		t.Errorf("Reroute didn't show Railcar muted")
	}
	if s := readWAV(t, heard); audio.Power(s, 440) < 1 {
		t.Errorf("Reroute heard %.3g of Railcar's 440 Hz", audio.Power(s, 440))
	}
	waitFor(t, "the call to end", func() bool { p := phase(kat.e); return p == CallEnded || p == "" })
}

// TestE2ECallsRelayOnly calls with direct connections off, the default,
// so media goes only through each homeserver's TURN relay (§15.1,
// §15.2): Railcar to Railcar across two servers, with new relay
// credentials partway through, then Railcar to Reroute, then a voice
// channel, and last a call from an identity behind a proxy, with calls
// allowed. It needs coturn.
func TestE2ECallsRelayOnly(t *testing.T) {
	bin := testnet.Rerouted(t)
	o, relays := testnet.StartRelayed(t, bin, map[string]string{"c.test": "a.test"}, "a.test", "b.test")
	ctx := context.Background()
	kat := newCaller(t, ctx, o, "a.test", "kat", 440)
	ravi := newCaller(t, ctx, o, "b.test", "ravi", 660)
	for _, c := range []*caller{kat, ravi} {
		if err := c.e.SetDirectCalls(false); err != nil {
			t.Fatal(err)
		}
	}
	chat := callFriends(t, ctx, kat, ravi)
	// Renew the relay credentials 3 seconds in, as a long call does
	// before ttl runs out.
	was := relayRefresh
	relayRefresh = func(time.Duration) time.Duration { return 3 * time.Second }
	t.Cleanup(func() { relayRefresh = was })
	fetched := relayFetches.Load()
	if err := kat.e.StartCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ravi's ring", func() bool { return phase(ravi.e) == CallRinging })
	if err := ravi.e.AnswerCall(ctx, chat); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			for n, f := range relays.Logs {
				b, _ := os.ReadFile(f)
				t.Logf("%s's relay:\n%s", n, b)
			}
		}
	})
	waitFor(t, "the call to connect", func() bool { return phase(kat.e) == CallActive && phase(ravi.e) == CallActive })
	if c, _ := kat.e.Call(); !c.Relay {
		t.Error("the call isn't relay-only")
	}
	time.Sleep(time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660}, 440)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440}, 660)
	waitFor(t, "new relay credentials on both sides", func() bool { return relayFetches.Load() >= fetched+2 })
	time.Sleep(time.Second)
	hears(t, "kat, after new credentials", kat.tone.record(2*time.Second), []float64{660}, 440)
	kat.e.HangUp(ctx)
	for _, n := range []string{"a.test", "b.test"} {
		b, _ := os.ReadFile(relays.Logs[n])
		if !strings.Contains(strings.ToLower(string(b)), "allocation") {
			t.Errorf("%s's relay carried nothing", n)
		}
	}

	// Reroute's command-line client, relay-only too (no --direct).
	lee := newRerouteCLI(t, "lee")
	dir := t.TempDir()
	writeTone(t, filepath.Join(dir, "lee.wav"), 880, 12)
	lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode)
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("accept", "kat.a.test")
	var lchat string
	waitFor(t, "the chat with Lee", func() bool {
		for _, c := range kat.e.Contacts() {
			if c.Handle == "lee.b.test" && c.Chat != "" {
				lchat = c.Chat
				return true
			}
		}
		return false
	})
	waitFor(t, "the call to clear", func() bool { return phase(kat.e) == "" })
	heard := filepath.Join(dir, "lee-heard.wav")
	wait := lee.start("answer", "--play", filepath.Join(dir, "lee.wav"), "--record", heard, "--seconds", "6", "--wait", "40")
	time.Sleep(2 * time.Second)
	if err := kat.e.StartCall(ctx, lchat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the call to connect", func() bool { return phase(kat.e) == CallActive })
	time.Sleep(time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{880}, 440)
	wait()
	if s := readWAV(t, heard); audio.Power(s, 440) < 1 {
		t.Errorf("Reroute heard %.3g of Railcar's 440 Hz", audio.Power(s, 440))
	}

	// A voice channel, with each homeserver's relay offered; the
	// relay-only setting is for calls (§15.2).
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
	ravi.channelChat("c.test", "Lounge")
	waitFor(t, "the call to clear", func() bool { return phase(kat.e) == "" })
	for _, c := range []*caller{kat, ravi} {
		if err := c.e.JoinVoice(ctx, lounge); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both keys", func() bool { return keyedOthers(kat.e) == 1 && keyedOthers(ravi.e) == 1 })
	if !voiceOf(kat.e).Relay {
		t.Error("the visit wasn't offered the relay")
	}
	fetched = relayFetches.Load()
	waitFor(t, "new relay credentials in the voice channel", func() bool { return relayFetches.Load() >= fetched+2 })
	time.Sleep(2 * time.Second)
	hears(t, "kat", kat.tone.record(2*time.Second), []float64{660}, 440)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{440}, 660)
	// Through a proxy, with calls allowed: only the relay, and a call
	// still connects and carries audio (§15.2).
	if err := ravi.e.LeaveVoice(ctx); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.LeaveVoice(ctx); err != nil {
		t.Fatal(err)
	}
	socks := proxytest.SOCKS5(t, "", "", nil)
	px, _ := proxy.Parse(socks.URL)
	maxTone := &toneAudio{hz: 1320}
	max := &caller{newRailcarVia(t, o, "a.test", "max", px, Options{Calls: CallOptions{Audio: maxTone.open, Loopback: true, ProxyCalls: true}}), maxTone}
	mctx, mcancel := context.WithCancel(ctx)
	mdone := make(chan error, 1)
	go func() { mdone <- max.e.Run(mctx) }()
	t.Cleanup(func() { mcancel(); <-mdone })
	waitEvent(t, max.e, "the stream", func(ev Event) bool { return ev.Kind == EventStream && ev.Stream == client.EventConnected })
	mchat := callFriends(t, ctx, max, ravi)
	waitFor(t, "the call to clear", func() bool { return phase(ravi.e) == "" })
	if err := max.e.StartCall(ctx, mchat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ravi's ring from Max", func() bool { return phase(ravi.e) == CallRinging })
	if err := ravi.e.AnswerCall(ctx, mchat); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the proxied call to connect", func() bool { return phase(max.e) == CallActive && phase(ravi.e) == CallActive })
	if c, _ := max.e.Call(); !c.Relay {
		t.Error("the proxied call isn't relay-only")
	}
	time.Sleep(time.Second)
	hears(t, "ravi", ravi.tone.record(2*time.Second), []float64{1320}, 660)
	max.e.HangUp(ctx)
}
