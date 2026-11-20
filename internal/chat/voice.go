package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/rerouteholdings/railcar/internal/media/rtc"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/sframe"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// keyGrace is how long a voice key for a visit that no voice-state lists
// is kept, since keys can arrive before the state that lists them (§16.4).
const keyGrace = time.Minute

// VoiceKey is a sender key for one visit to a voice channel (§16.4).
type VoiceKey struct {
	T       string     `json:"t"`
	TS      int64      `json:"ts"`
	Channel uint64     `json:"channel"`
	Visit   uint32     `json:"visit"`
	Gen     uint8      `json:"gen"`
	Key     wire.Bytes `json:"key"`
}

// VoiceMute is a device's mute state in a voice channel (§16.4).
type VoiceMute struct {
	T        string `json:"t"`
	TS       int64  `json:"ts"`
	Channel  uint64 `json:"channel"`
	Visit    uint32 `json:"visit"`
	Muted    bool   `json:"muted"`
	Deafened bool   `json:"deafened,omitempty"`
}

// VoicePerson is a visit in the voice channel this device is in.
type VoicePerson struct {
	ID       string
	Visit    uint32
	Self     bool
	Muted    bool
	Deafened bool
	Cast     []string
	Keyed    bool    // this device has the visit's key, so it can hear it
	Level    float64 // how loud they are now, from 0 to 1
}

// VoiceInfo is this device's place in a voice channel, for the
// interface.
type VoiceInfo struct {
	Chat     string
	Server   string
	Channel  uint64
	Visit    uint32
	Joined   bool // the community server took the visit
	Muted    bool
	Deafened bool
	Level    float64
	People   []VoicePerson
	Relay    bool
	Ended    string
}

// vkey is one generation of a visit's base key, kept by the visit ID and
// generation; each stream's keys come from it (§16.4).
type vkey struct {
	id    string
	base  []byte
	keys  map[uint8]*sframe.Key // by stream
	at    time.Time
	seen  bool      // a voice-state listed its visit
	until time.Time // for a replaced generation: when it goes
}

type vstate struct {
	ID    string   `json:"id"`
	Dev   string   `json:"dev"`
	Visit uint32   `json:"visit"`
	Cast  []string `json:"cast"`
}

// visit is this device's visit to a voice channel.
type visit struct {
	server, chat string
	channel      uint64
	visit        uint32
	gen          uint8
	base         []byte
	sdp          string
	joined       bool
	first        bool // the next voice-state is the first after joining
	listed       map[uint32]bool
	peer         *rtc.Peer
	media        *rtc.Media
	audio        AudioIO
	opts         rtc.Options
	cancel       context.CancelFunc
	deafened     bool
	ended        string
	relay        relayLease
}

// voice is the engine's voice channel state. Like calls, one goroutine
// does the work, in order.
type voice struct {
	e    *Engine
	opts CallOptions

	mu      sync.Mutex
	queue   []func()
	wake    chan struct{}
	started bool
	sends   map[string]func(any) error  // each community stream's writer
	keys    map[string]map[uint64]*vkey // by channel chat ID, then KID
	states  map[string][]vstate         // the latest voice-state, by channel chat ID
	mutes   map[string]map[uint32]VoiceMute
	cur     *visit
	last    VoiceInfo
	hasLast bool
}

func newVoice(e *Engine, o CallOptions) *voice {
	return &voice{
		e: e, opts: o, wake: make(chan struct{}, 1), sends: map[string]func(any) error{},
		keys: map[string]map[uint64]*vkey{}, states: map[string][]vstate{}, mutes: map[string]map[uint32]VoiceMute{},
	}
}

func (v *voice) do(f func()) {
	v.mu.Lock()
	v.queue = append(v.queue, f)
	if !v.started {
		v.started = true
		go v.run()
	}
	v.mu.Unlock()
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *voice) doWait(f func() error) error {
	done := make(chan error, 1)
	v.do(func() { done <- f() })
	return <-done
}

func (v *voice) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		v.mu.Lock()
		q := v.queue
		v.queue = nil
		v.mu.Unlock()
		for _, f := range q {
			f()
		}
		select {
		case <-v.wake:
		case <-tick.C:
			if c := v.cur; c != nil {
				if c.ended == "" && c.peer != nil && c.relay.due(time.Now()) {
					v.e.renewRelay(v.do, func(r *client.Relay, err error) { v.renewed(c, r, err) })
				}
				v.emit()
			}
		}
	}
}

// renewed takes new relay credentials during a long visit (client API,
// "Call relays"), for when the connection gathers candidates again.
func (v *voice) renewed(c *visit, r *client.Relay, err error) {
	if v.cur != c || c.ended != "" || c.peer == nil {
		return
	}
	if err != nil {
		v.e.log.Info("chat: renewing the call relay", "err", err)
		c.relay.failed()
		return
	}
	c.opts.ICEServers = iceServers(r)
	c.relay.set(r)
	if err := c.peer.PC.SetConfiguration(c.opts.Config()); err != nil {
		v.e.log.Info("chat: renewing the call relay", "err", err)
	}
}

// connected notes a community stream's writer, and rejoins a voice
// channel whose visit a dropped connection ended.
func (v *voice) connected(server string, send func(any) error) {
	v.do(func() {
		v.sends[server] = send
		if c := v.cur; c != nil && c.server == server && c.ended == "" && !c.joined && c.peer == nil {
			if err := v.join(c.chat); err != nil {
				v.e.log.Info("chat: rejoining a voice channel", "err", err)
			}
		}
	})
}

// disconnected ends the visit on a community server whose stream
// dropped: a closed stream ends it (community server API, "Voice"). It's
// joined again when the stream comes back.
func (v *voice) disconnected(server string) {
	v.do(func() {
		delete(v.sends, server)
		if c := v.cur; c != nil && c.server == server && c.ended == "" {
			v.stopMedia(c)
			c.joined, c.peer = false, nil
			v.emit()
		}
	})
}

// JoinVoice joins a voice channel through its community server's
// selective forwarding unit (§16.4). A visit this device is on ends
// first.
func (e *Engine) JoinVoice(ctx context.Context, chat string) error {
	v := e.voice
	return v.doWait(func() error {
		if c := v.cur; c != nil && c.ended == "" {
			if c.chat == chat {
				return errors.New("chat: you're in this voice channel already")
			}
			v.leave(c, "")
		}
		if info, ok := e.Call(); ok && info.Phase != CallEnded && info.Phase != CallRinging {
			return errors.New("chat: hang up the call first")
		}
		return v.join(chat)
	})
}

func (v *voice) join(chat string) error {
	e := v.e
	server, channel, ok := parseChanID(chat)
	if !ok {
		return errors.New("chat: that isn't a channel")
	}
	e.mu.Lock()
	ch := e.st.Chats[chat]
	var err error
	switch {
	case ch == nil:
		err = errors.New("chat: no such channel")
	case !ch.Voice:
		err = errors.New("chat: that isn't a voice channel")
	default:
		if cm := e.st.Communities[server]; cm == nil || cm.Left {
			err = errors.New("chat: you're not in that community")
		} else if cm.Stopped != "" {
			err = fmt.Errorf("chat: this community is stopped: %s", cm.Stopped)
		}
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	send := v.sends[server]
	if send == nil {
		return errors.New("chat: not connected to the community server yet")
	}
	c := v.cur
	if c == nil || c.chat != chat || c.ended != "" {
		c = &visit{server: server, chat: chat, channel: channel}
	}
	c.ended, c.joined = "", false
	c.opts = rtc.Options{Loopback: v.opts.Loopback, RelayOnly: !e.DirectCalls()}
	tctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	relay, terr := e.c.Turn(tctx)
	cancel()
	c.relay.set(relay)
	switch {
	case terr == nil:
		c.opts.ICEServers = iceServers(relay)
	case c.opts.RelayOnly:
		var we *wire.Error
		if errors.As(terr, &we) && we.Code == "not-implemented" {
			return errors.New("chat: your homeserver has no call relay, so the community server would see your IP address. Allow direct calls to join anyway")
		}
		return fmt.Errorf("chat: getting the call relay: %w", terr)
	}
	api, err := rtc.NewAPI(c.opts)
	if err != nil {
		return err
	}
	if c.audio.Source == nil {
		if c.audio, err = e.openAudio(v.opts); err != nil {
			return fmt.Errorf("chat: the microphone and speakers: %w", err)
		}
	}
	c.base = make([]byte, 32)
	rand.Read(c.base)
	c.visit, c.gen = randVisit(), 0
	media, err := rtc.NewMedia(c.audio.Source, c.audio.Mix, voiceRing{v, chat})
	if err != nil {
		return err
	}
	if c.media != nil {
		media.SetMuted(c.media.Muted())
	}
	c.media = media
	c.audio.Mix.SetDeafened(c.deafened)
	peer, err := rtc.NewPeer(api, c.opts, func(cand rtc.Candidate) {}, func(t *webrtc.TrackRemote, kind string) {
		go media.Receive(t, kind)
	})
	if err != nil {
		return err
	}
	c.peer = peer
	if err := peer.AddSections(media.Track); err != nil {
		return err
	}
	if c.sdp, err = peer.Offer(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(context.Background())
	c.cancel = stop
	go media.Run(ctx)
	v.cur = c
	if err := send(map[string]any{"t": "voice-join", "channel": channel, "visit": c.visit, "sdp": c.sdp}); err != nil {
		v.stopMedia(c)
		return fmt.Errorf("chat: joining the voice channel: %w", err)
	}
	v.emit()
	return nil
}

func randVisit() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// LeaveVoice leaves the voice channel this device is in.
func (e *Engine) LeaveVoice(ctx context.Context) error {
	v := e.voice
	return v.doWait(func() error {
		c := v.cur
		if c == nil || c.ended != "" {
			return errors.New("chat: you're not in a voice channel")
		}
		v.leave(c, "You left")
		return nil
	})
}

func (v *voice) leave(c *visit, why string) {
	if send := v.sends[c.server]; send != nil {
		send(map[string]any{"t": "voice-leave", "channel": c.channel})
	}
	v.stopMedia(c)
	if c.audio.Close != nil {
		c.audio.Close()
		c.audio = AudioIO{}
	}
	if why == "" {
		why = "You left"
	}
	c.ended = why
	v.emit()
	v.cur = nil
	v.emit()
}

func (v *voice) stopMedia(c *visit) {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.peer != nil {
		c.peer.Close()
		c.peer = nil
	}
}

// SetVoiceMuted mutes or unmutes the microphone in the voice channel,
// and tells the others (§16.4).
func (e *Engine) SetVoiceMuted(on bool) error {
	v := e.voice
	return v.doWait(func() error {
		c := v.cur
		if c == nil || c.media == nil {
			return errors.New("chat: you're not in a voice channel")
		}
		if !on && c.deafened {
			c.deafened = false
			c.audio.Mix.SetDeafened(false)
		}
		c.media.SetMuted(on)
		v.sendMute(c)
		v.emit()
		return nil
	})
}

// SetVoiceDeafened stops or starts playing the voice channel. Deafened,
// the microphone is muted too (§16.4).
func (e *Engine) SetVoiceDeafened(on bool) error {
	v := e.voice
	return v.doWait(func() error {
		c := v.cur
		if c == nil || c.media == nil {
			return errors.New("chat: you're not in a voice channel")
		}
		c.deafened = on
		c.audio.Mix.SetDeafened(on)
		if on {
			c.media.SetMuted(true)
		}
		v.sendMute(c)
		v.emit()
		return nil
	})
}

// Voice returns this device's voice channel, if it's in one.
func (e *Engine) Voice() (VoiceInfo, bool) {
	v := e.voice
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.last, v.hasLast
}

// sendKey sends the visit's voice-key in the group that carries the
// channel (§16.4).
func (v *voice) sendKey(c *visit, ephemeral bool) error {
	k := VoiceKey{T: CVoiceKey, Channel: c.channel, Visit: c.visit, Gen: c.gen, Key: c.base}
	return v.e.voiceSend(c.server, c.channel, k, ephemeral)
}

// sendMute sends voice-mute, as an ephemeral delivery (§16.4).
func (v *voice) sendMute(c *visit) {
	if !c.joined {
		return
	}
	m := VoiceMute{T: CVoiceMute, Channel: c.channel, Visit: c.visit, Muted: c.media.Muted() || c.deafened, Deafened: c.deafened}
	if err := v.e.voiceSend(c.server, c.channel, m, true); err != nil {
		v.e.log.Info("chat: sending voice-mute", "err", err)
	}
}

// voiceSend sends a VoiceKey or VoiceMute to the group that carries a
// voice channel, with the channel's ID, stamped with the homeserver's
// time.
func (e *Engine) voiceSend(server string, channel uint64, content any, ephemeral bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ts := e.c.Clock().Now().Unix()
	switch c := content.(type) {
	case VoiceKey:
		c.TS = ts
		content = c
	case VoiceMute:
		c.TS = ts
		content = c
	}
	cm, c, err := e.communityOf(server)
	if err != nil {
		return err
	}
	cch := c.State().Channels[channel]
	if cch == nil || cch.Kind != "voice" {
		return errors.New("chat: that isn't a voice channel")
	}
	g := e.groupFor(cm, cch)
	if g == nil {
		return errors.New("chat: this device isn't in the channel's group yet")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ch := channel
	_, err = e.csend(ctx, cm, g, &ch, content, ephemeral)
	return err
}

// content takes voice-key and voice-mute content from a channel's group.
// The caller holds e.mu, so this only queues the work.
func (v *voice) content(server string, channel uint64, from, t string, am AppMessage) error {
	chat := chanID(server, channel)
	switch t {
	case CVoiceKey:
		var k VoiceKey
		if err := wire.Unmarshal(am.C, &k); err != nil || len(k.Key) != 32 || k.Channel != channel {
			return errors.New("chat: a malformed voice-key")
		}
		v.do(func() { v.takeKey(chat, from, k) })
	case CVoiceMute:
		var m VoiceMute
		if err := wire.Unmarshal(am.C, &m); err != nil || m.Channel != channel {
			return errors.New("chat: a malformed voice-mute")
		}
		v.do(func() { v.takeMute(chat, from, m) })
	}
	return nil
}

func (v *voice) takeKey(chat, from string, k VoiceKey) {
	if len(k.Key) != 32 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	m := v.keys[chat]
	if m == nil {
		m = map[uint64]*vkey{}
		v.keys[chat] = m
	}
	kid := uint64(k.Visit)<<8 | uint64(k.Gen)
	if old := m[kid]; old != nil && bytes.Equal(old.base, k.Key) {
		return // sent again for a newcomer
	}
	vk := &vkey{id: from, base: bytes.Clone(k.Key), keys: map[uint8]*sframe.Key{}, at: time.Now()}
	for _, s := range v.states[chat] {
		if s.Visit == k.Visit {
			if s.ID != from {
				return // the visit is someone else's
			}
			vk.seen = true
		}
	}
	m[kid] = vk
	// An older generation of the same visit is kept 10 seconds, for frames
	// in flight, as in calls (§15.2, §16.4).
	for old, ok := range m {
		if old>>8 == kid>>8 && old < kid && ok.until.IsZero() {
			ok.until = time.Now().Add(oldKeysFor)
		}
	}
}

func (v *voice) takeMute(chat, from string, m VoiceMute) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Only from the identity the latest voice-state lists for the visit.
	owner := ""
	for _, s := range v.states[chat] {
		if s.Visit == m.Visit {
			owner = s.ID
		}
	}
	if owner != from {
		return
	}
	mm := v.mutes[chat]
	if mm == nil {
		mm = map[uint32]VoiceMute{}
		v.mutes[chat] = mm
	}
	if old, ok := mm[m.Visit]; ok && old.TS > m.TS {
		return
	}
	mm[m.Visit] = m
}

// frame takes voice signaling from a community server's stream.
func (v *voice) frame(server string, raw jsontext.Value) {
	var f struct {
		T       string   `json:"t"`
		Channel uint64   `json:"channel"`
		Visit   uint32   `json:"visit"`
		SDP     string   `json:"sdp"`
		Data    string   `json:"data"`
		In      []vstate `json:"in"`
		From    uint64   `json:"from"`
		To      uint64   `json:"to"`
		Error   string   `json:"error"`
		Msg     string   `json:"msg"`
		Ref     struct {
			T     string `json:"t"`
			Visit uint32 `json:"visit"`
		} `json:"ref"`
	}
	if wire.Unmarshal(raw, &f) != nil {
		return
	}
	if f.T == "error" && !strings.HasPrefix(f.Ref.T, "voice-") {
		return
	}
	v.do(func() {
		v.handleFrame(server, f.T, f.Channel, f.Visit, f.SDP, f.Data, f.In, f.From, f.To, f.Error, f.Msg, f.Ref.T, f.Ref.Visit)
	})
}

func (v *voice) handleFrame(server, t string, channel uint64, vis uint32, sdp, data string, in []vstate, from, to uint64, code, msg, refT string, refVisit uint32) {
	chat := chanID(server, channel)
	c := v.cur
	mine := c != nil && c.server == server && c.ended == ""
	switch t {
	case "voice-state":
		v.mu.Lock()
		prev := v.states[chat]
		v.states[chat] = in
		listed := map[uint32]string{}
		for _, s := range in {
			listed[s.Visit] = s.ID
		}
		// Keys and mute states of visits that left, or that belong to
		// someone else, go.
		for kid, k := range v.keys[chat] {
			owner, ok := listed[uint32(kid>>8)]
			switch {
			case ok && owner != k.id:
				delete(v.keys[chat], kid)
			case ok:
				k.seen = true
			case k.seen || time.Since(k.at) > keyGrace:
				delete(v.keys[chat], kid)
			}
		}
		for vis := range v.mutes[chat] {
			if _, ok := listed[vis]; !ok {
				delete(v.mutes[chat], vis)
			}
		}
		v.mu.Unlock()
		if !mine || c.chat != chat || !c.joined {
			return
		}
		if c.first {
			// The first after joining lists who just got this device's
			// key (§16.4).
			c.first = false
			return
		}
		was := map[uint32]bool{}
		for _, s := range prev {
			was[s.Visit] = true
		}
		for _, s := range in {
			if !was[s.Visit] && s.Visit != c.visit {
				// A newcomer: send the key again, ephemeral (§16.4).
				if err := v.sendKey(c, true); err != nil {
					v.e.log.Info("chat: sending a voice key again", "err", err)
				}
				if c.media.Muted() || c.deafened {
					v.sendMute(c)
				}
				break
			}
		}
		v.emit()
	case "voice-joined":
		if !mine || c.chat != chat || vis != c.visit {
			return
		}
		if sdp != "" {
			if _, err := c.peer.Remote(sdp, c.media.Track); err != nil {
				v.fail(c, "The community server's answer didn't work: "+err.Error())
				return
			}
		}
		c.joined, c.first = true, true
		if err := v.sendKey(c, false); err != nil {
			v.fail(c, "Your voice key couldn't be sent: "+err.Error())
			return
		}
		s, err := sframe.NewSender(c.base, sframe.VoiceKID(c.visit, c.gen, sframe.StreamVoice))
		if err != nil {
			return
		}
		c.media.SetSender(s)
		if c.media.Muted() || c.deafened {
			v.sendMute(c)
		}
		v.emit()
	case "voice-sdp":
		if !mine || c.chat != chat || c.peer == nil {
			return
		}
		answer, err := c.peer.Remote(data, c.media.Track)
		if err != nil {
			v.e.log.Info("chat: the community server's session description", "err", err)
			return
		}
		if answer != "" {
			v.sends[server](map[string]any{"t": "voice-sdp", "channel": channel, "data": answer})
		}
	case "voice-ice":
		if mine && c.chat == chat && c.peer != nil {
			c.peer.AddCandidate(data)
		}
	case "voice-moved":
		if !mine || c.channel != from {
			return
		}
		// A moderator moved this device: join the other channel with a
		// new visit (§16.7).
		v.stopMedia(c)
		send := v.sends[server]
		if send != nil {
			send(map[string]any{"t": "voice-leave", "channel": from})
		}
		c.ended = "Moved"
		next := chanID(server, to)
		if err := v.join(next); err != nil {
			v.fail(c, "You were moved, and joining the other channel failed: "+err.Error())
		}
	case "error":
		if !mine {
			return
		}
		switch {
		case refT == "voice-join" && code == "already-exists" && refVisit == c.visit:
			// The visit ID is taken: pick another (§16.4).
			c.visit = randVisit()
			v.sends[server](map[string]any{"t": "voice-join", "channel": c.channel, "visit": c.visit, "sdp": c.sdp})
		case refT == "voice-join":
			why := "The community server refused the visit"
			if code == "not-allowed" {
				why = "You can't join this voice channel, or it's full"
			}
			v.fail(c, why)
		case refT == "voice-sdp" && code == "bad-request":
			// Our offer crossed the server's: it keeps its own.
		default:
			v.e.log.Info("chat: a voice error", "code", code, "msg", msg)
		}
	}
}

func (v *voice) fail(c *visit, why string) {
	v.leave(c, why)
	v.mu.Lock()
	v.last = VoiceInfo{Chat: c.chat, Server: c.server, Channel: c.channel, Ended: why}
	v.hasLast = true
	v.mu.Unlock()
	v.e.emit(Event{Kind: EventVoice, Voice: &VoiceInfo{Chat: c.chat, Server: c.server, Channel: c.channel, Ended: why}})
}

// emit builds the visit for the interface and sends an event.
func (v *voice) emit() {
	c := v.cur
	ev := Event{Kind: EventVoice}
	if c == nil {
		v.mu.Lock()
		v.last, v.hasLast = VoiceInfo{}, false
		v.mu.Unlock()
		v.e.emit(ev)
		return
	}
	info := VoiceInfo{Chat: c.chat, Server: c.server, Channel: c.channel, Visit: c.visit, Joined: c.joined, Deafened: c.deafened, Relay: c.opts.RelayOnly, Ended: c.ended}
	var levels map[string]float64
	if c.media != nil {
		info.Muted, info.Level = c.media.Muted() || c.deafened, c.media.Level()
		levels = c.media.Speaking()
	}
	v.mu.Lock()
	for _, s := range v.states[c.chat] {
		p := VoicePerson{ID: s.ID, Visit: s.Visit, Cast: s.Cast, Self: s.Visit == c.visit}
		if mm, ok := v.mutes[c.chat][s.Visit]; ok {
			p.Muted, p.Deafened = mm.Muted, mm.Deafened
		}
		for kid := range v.keys[c.chat] {
			if uint32(kid>>8) == s.Visit {
				p.Keyed = true
			}
		}
		if p.Self {
			p.Muted, p.Deafened, p.Keyed, p.Level = info.Muted, info.Deafened, true, info.Level
		} else {
			p.Level = levels[s.ID]
		}
		info.People = append(info.People, p)
	}
	slices.SortFunc(info.People, func(a, b VoicePerson) int { return strings.Compare(a.ID, b.ID) })
	v.last, v.hasLast = info, true
	v.mu.Unlock()
	ev.Voice = &info
	v.e.emit(ev)
}

// voiceRing opens voice channel frames with the keys members sent.
type voiceRing struct {
	v    *voice
	chat string
}

func (r voiceRing) Key(kid uint64) (string, *sframe.Key, bool) {
	visit, gen, stream := sframe.SplitVoiceKID(kid)
	if stream > sframe.StreamCastVideo {
		return "", nil, false // a stream byte this version doesn't know: dropped
	}
	r.v.mu.Lock()
	defer r.v.mu.Unlock()
	m := r.v.keys[r.chat]
	at := uint64(visit)<<8 | uint64(gen)
	k := m[at]
	if k == nil {
		return "", nil, false
	}
	if !k.until.IsZero() && time.Now().After(k.until) {
		delete(m, at)
		return "", nil, false
	}
	key := k.keys[stream]
	if key == nil {
		var err error
		if key, err = sframe.Derive(k.base, kid); err != nil {
			return "", nil, false
		}
		k.keys[stream] = key
	}
	return k.id, key, true
}

// Kind says which stream a voice channel frame is, from its KID's last
// byte, since the forwarding unit's media sections don't say (§16.4).
func (r voiceRing) Kind(kid uint64) string {
	switch _, _, stream := sframe.SplitVoiceKID(kid); stream {
	case sframe.StreamCastAudio:
		return "cast"
	case sframe.StreamCastVideo:
		return "video"
	}
	return "voice"
}
