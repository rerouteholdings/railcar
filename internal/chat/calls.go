package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/media/rtc"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/sframe"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Call limits and timing (§15.2).
const (
	MaxCall      = 10               // people in a call
	ringFor      = 60 * time.Second // how long a ring lasts
	oldKeysFor   = 10 * time.Second // a past epoch's keys, after a commit
	connectLimit = 30 * time.Second // to connect to a device after offering
)

// The ops of call content (§15.2).
const (
	opRing    = "ring"
	opAccept  = "accept"
	opDecline = "decline"
	opSDP     = "sdp"
	opICE     = "ice"
	opHangup  = "hangup"
	opState   = "state"
)

// CallContent is call signaling and state (§15.2).
type CallContent struct {
	T        string     `json:"t"`
	TS       int64      `json:"ts"`
	Call     wire.Bytes `json:"call"`
	Op       string     `json:"op"`
	To       string     `json:"to,omitempty"`
	Dev      wire.Bytes `json:"dev,omitempty"`
	Data     string     `json:"data,omitempty"`
	Media    []string   `json:"media,omitempty"`
	Muted    *bool      `json:"muted,omitempty"`
	Deafened bool       `json:"deafened,omitempty"`
	Cast     *[]string  `json:"cast,omitempty"`
}

func parseCall(raw []byte) (CallContent, error) {
	var c CallContent
	if err := wire.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("chat: malformed call content: %w", err)
	}
	if len(c.Call) != 16 {
		return c, errors.New("chat: call content without a 16-byte call ID")
	}
	switch c.Op {
	case opRing, opAccept, opDecline, opHangup, opState:
	case opSDP, opICE:
		if c.To == "" || len(c.Dev) != 32 || c.Data == "" {
			return c, errors.New("chat: sdp or ice without to, dev, and data")
		}
	default:
		return c, fmt.Errorf("chat: a call op %q", c.Op)
	}
	return c, nil
}

// CallPhase is where this device is in a call.
type CallPhase string

// The phases of a call.
const (
	CallRinging    CallPhase = "ringing"    // someone is calling this device
	CallCalling    CallPhase = "calling"    // this device rang, and waits for an answer
	CallConnecting CallPhase = "connecting" // answered, and connecting
	CallActive     CallPhase = "active"     // connected to someone
	CallEnded      CallPhase = "ended"
)

// CallPerson is someone else in a call.
type CallPerson struct {
	ID        string
	Connected bool
	Muted     bool
	Deafened  bool
	Cast      []string
	Level     float64 // how loud they are now, from 0 to 1
}

// CallRing is a ring this device hasn't answered.
type CallRing struct {
	Chat string
	Call string
	From string
	At   time.Time
}

// CallInfo is this device's call, for the interface.
type CallInfo struct {
	Chat     string
	Call     string
	Phase    CallPhase
	Incoming bool
	From     string    // who rang
	Since    time.Time // when it connected; zero before
	Muted    bool
	Deafened bool
	Level    float64 // this device's microphone
	People   []CallPerson
	// Waiting are rings from other chats while this call goes on.
	Waiting []CallRing
	// Ended says why the call ended, once it has.
	Ended string
	// Relay is false when the call may connect directly, which shows IP
	// addresses.
	Relay bool
}

// AudioIO is where a call's audio comes from and goes.
type AudioIO struct {
	Source audio.Source
	Mix    *audio.Mixer
	Close  func()
}

// CallOptions adjust calls and voice channels.
type CallOptions struct {
	// Audio opens the microphone and speakers. Nil uses the machine's
	// default devices.
	Audio func() (AudioIO, error)
	// Loopback adds loopback ICE candidates, for tests on one machine.
	Loopback bool
}

func (o CallOptions) open() (AudioIO, error) {
	if o.Audio != nil {
		return o.Audio()
	}
	mix := audio.NewMixer()
	d, err := audio.Open(mix)
	if err != nil {
		return AudioIO{}, err
	}
	return AudioIO{Source: d, Mix: mix, Close: d.Close}, nil
}

// callDev is a device in a call.
type callDev struct {
	id       string
	dev      []byte
	peer     *rtc.Peer
	offered  bool // this device sent it an offer it hasn't answered
	muted    bool
	deafened bool
	cast     []string
	ice      []string // candidates before the connection exists
}

// call is this device's call.
type call struct {
	chat     string
	id       []byte
	group    bool
	incoming bool
	from     string
	phase    CallPhase
	ringAt   time.Time
	since    time.Time
	ended    string
	devs     map[string]*callDev // other devices in the call, by device key
	audio    AudioIO
	media    *rtc.Media
	api      *webrtc.API
	rtcOpts  rtc.Options
	keys     *callKeys
	cancel   context.CancelFunc
	ctx      context.Context
	deafened bool
	relay    relayLease
}

// knownCall is a call going on in a chat, as far as this device saw.
type knownCall struct {
	id    []byte
	from  string
	at    time.Time
	in    map[string]string // devices in it: device key to identity
	rang  bool              // this device was rung
	acked bool              // the ring was shown as missed or answered
}

// calls is the engine's call state. One goroutine, run, handles every
// event in order, so signaling for a call never races.
type calls struct {
	e    *Engine
	opts CallOptions

	mu      sync.Mutex
	queue   []func()
	wake    chan struct{}
	active  *call
	rings   map[string]*CallRing  // by call ID
	known   map[string]*knownCall // by chat
	missed  map[string]bool       // calls already shown as missed or ended, by call ID
	over    map[string]bool       // direct calls that ended, by call ID
	started bool
	// last is the call as the interface last saw it.
	last    CallInfo
	hasLast bool
}

func newCalls(e *Engine, o CallOptions) *calls {
	return &calls{e: e, opts: o, wake: make(chan struct{}, 1), rings: map[string]*CallRing{}, known: map[string]*knownCall{}, missed: map[string]bool{}, over: map[string]bool{}}
}

// do queues f for the call goroutine.
func (cs *calls) do(f func()) {
	cs.mu.Lock()
	cs.queue = append(cs.queue, f)
	if !cs.started {
		cs.started = true
		go cs.run()
	}
	cs.mu.Unlock()
	select {
	case cs.wake <- struct{}{}:
	default:
	}
}

// doWait queues f and waits for it to run.
func (cs *calls) doWait(f func() error) error {
	done := make(chan error, 1)
	cs.do(func() { done <- f() })
	return <-done
}

func (cs *calls) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		cs.mu.Lock()
		q := cs.queue
		cs.queue = nil
		cs.mu.Unlock()
		for _, f := range q {
			f()
		}
		select {
		case <-cs.wake:
		case <-tick.C:
			cs.tick()
		}
	}
}

// snapshot builds the call for the interface. Only the call goroutine
// calls it.
func (cs *calls) snapshot() (CallInfo, bool) {
	c := cs.active
	if c == nil {
		return CallInfo{}, false
	}
	info := CallInfo{
		Chat: c.chat, Call: wire.EncodeB64(c.id), Phase: c.phase, Incoming: c.incoming, From: c.from,
		Since: c.since, Ended: c.ended, Deafened: c.deafened, Relay: c.rtcOpts.RelayOnly,
	}
	var levels map[string]float64
	if c.media != nil {
		info.Muted, info.Level = c.media.Muted(), c.media.Level()
		levels = c.media.Speaking()
	}
	// A person is muted only when every device of theirs in the call is.
	byID := map[string]*CallPerson{}
	var order []string
	for _, d := range c.devs {
		p := byID[d.id]
		if p == nil {
			p = &CallPerson{ID: d.id, Muted: true, Deafened: true}
			byID[d.id] = p
			order = append(order, d.id)
		}
		if d.peer != nil && d.peer.PC.ConnectionState() == webrtc.PeerConnectionStateConnected {
			p.Connected = true
		}
		p.Muted = p.Muted && d.muted
		p.Deafened = p.Deafened && d.deafened
		p.Cast = append(p.Cast, d.cast...)
		p.Level = max(p.Level, levels[d.id])
	}
	slices.Sort(order)
	for _, id := range order {
		info.People = append(info.People, *byID[id])
	}
	cs.mu.Lock()
	for _, r := range cs.rings {
		if r.Chat != c.chat {
			info.Waiting = append(info.Waiting, *r)
		}
	}
	cs.mu.Unlock()
	return info, true
}

// emitCall tells the interface the call changed.
func (cs *calls) emitCall() {
	info, ok := cs.snapshot()
	ev := Event{Kind: EventCall}
	if ok {
		ev.Call = &info
	}
	cs.mu.Lock()
	cs.last, cs.hasLast = info, ok
	for _, r := range cs.rings {
		ev.Rings = append(ev.Rings, *r)
	}
	cs.mu.Unlock()
	cs.e.emit(ev)
}

// send signs and sends call content to a chat's group. Every op but ring
// is ephemeral (§15.2).
func (cs *calls) send(chat string, c CallContent) error {
	e := cs.e
	c.T = CCall
	c.TS = e.c.Clock().Now().Unix()
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return e.sendContent(ctx, ch, c, c.Op != opRing)
}

// sendTo sends sdp or ice to one device in the call.
func (cs *calls) sendTo(c *call, d *callDev, op, data string) {
	if err := cs.send(c.chat, CallContent{Call: c.id, Op: op, To: d.id, Dev: d.dev, Data: data}); err != nil {
		cs.e.log.Info("chat: sending call signaling", "op", op, "err", err)
	}
}

// StartCall rings a chat's other people: a contact in a direct chat, or
// everyone in a group chat (§15.2). If a call is going on in the chat
// already, it joins that one.
func (e *Engine) StartCall(ctx context.Context, chat string) error {
	cs := e.calls
	return cs.doWait(func() error {
		if cs.active != nil && cs.active.phase != CallEnded {
			return errors.New("chat: you're in a call already")
		}
		e.mu.Lock()
		ch, err := e.sendable(chat)
		var kind string
		if err == nil {
			kind = ch.Kind
			if ch.Kind == KindChannel {
				err = errors.New("chat: calls are for direct chats and group chats; join a voice channel instead")
			} else if ch.Left {
				err = errors.New("chat: you're no longer in this group")
			}
		}
		e.mu.Unlock()
		if err != nil {
			return err
		}
		if k := cs.known[chat]; k != nil && len(k.in) > 0 {
			// One call per chat: join the one going on.
			return cs.join(chat, k.id, k.from, kind == KindGroup)
		}
		id := make([]byte, 16)
		rand.Read(id)
		c, err := cs.newCall(chat, id, kind == KindGroup)
		if err != nil {
			return err
		}
		c.phase, c.from, c.ringAt = CallCalling, e.c.ID(), time.Now()
		cs.setActive(c)
		cs.known[chat] = &knownCall{id: id, from: e.c.ID(), at: time.Now(), in: map[string]string{}}
		if err := cs.send(chat, CallContent{Call: id, Op: opRing, Media: []string{"audio"}}); err != nil {
			cs.end(c, "The call couldn't start: "+err.Error(), false)
			return err
		}
		cs.emitCall()
		return nil
	})
}

// newCall sets up a call's media: the relay, the audio devices, and the
// SFrame keys. The caller makes it active.
func (cs *calls) newCall(chat string, id []byte, group bool) (*call, error) {
	e := cs.e
	o := rtc.Options{Loopback: cs.opts.Loopback, RelayOnly: !e.DirectCalls()}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	relay, err := e.c.Turn(ctx)
	cancel()
	switch {
	case err == nil:
		o.ICEServers = iceServers(relay)
	case o.RelayOnly:
		var we *wire.Error
		if errors.As(err, &we) && we.Code == "not-implemented" {
			return nil, errors.New("chat: your homeserver has no call relay, so calls can't hide your IP address. Allow direct calls to call anyway")
		}
		return nil, fmt.Errorf("chat: getting the call relay: %w", err)
	}
	api, err := rtc.NewAPI(o)
	if err != nil {
		return nil, err
	}
	aio, err := cs.opts.open()
	if err != nil {
		return nil, fmt.Errorf("chat: the microphone and speakers: %w", err)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	c := &call{chat: chat, id: id, group: group, devs: map[string]*callDev{}, audio: aio, api: api, rtcOpts: o, ctx: cctx, cancel: ccancel}
	c.relay.set(relay)
	c.keys = &callKeys{e: e, chat: chat, call: id, old: map[uint64]oldKey{}}
	if err := c.keys.refresh(); err != nil {
		ccancel()
		aio.Close()
		return nil, err
	}
	c.media, err = rtc.NewMedia(aio.Source, aio.Mix, c.keys)
	if err != nil {
		ccancel()
		aio.Close()
		return nil, err
	}
	c.media.SetSender(c.keys.sender())
	go c.media.Run(cctx)
	return c, nil
}

func (cs *calls) setActive(c *call) {
	cs.mu.Lock()
	cs.active = c
	cs.mu.Unlock()
}

// AnswerCall answers the call ringing in a chat, or joins the call going
// on in a group chat (§15.2). A call this device is in ends first.
func (e *Engine) AnswerCall(ctx context.Context, chat string) error {
	cs := e.calls
	return cs.doWait(func() error {
		var ring *CallRing
		cs.mu.Lock()
		for _, r := range cs.rings {
			if r.Chat == chat || chat == "" {
				ring = r
				break
			}
		}
		cs.mu.Unlock()
		k := cs.known[chat]
		if ring == nil && (k == nil || len(k.in) == 0) {
			return errors.New("chat: no call to answer here")
		}
		if ring != nil {
			chat = ring.Chat
			k = cs.known[chat]
		}
		if cs.active != nil && cs.active.phase != CallEnded {
			if cs.active.chat == chat && cs.active.phase != CallRinging {
				return errors.New("chat: you're in this call already")
			}
			if cs.active.phase != CallRinging {
				cs.hangup(cs.active, "You ended the call to answer another")
			}
		}
		e.mu.Lock()
		ch, ok := e.st.Chats[chat]
		group := ok && ch.Kind == KindGroup
		e.mu.Unlock()
		if k == nil {
			return errors.New("chat: the call has ended")
		}
		if len(k.in) >= MaxCall-1 {
			return errors.New("chat: the call is full")
		}
		return cs.join(chat, k.id, k.from, group)
	})
}

// join answers or joins a call: it sends accept, and waits for offers
// from the devices in it (§15.2).
func (cs *calls) join(chat string, id []byte, from string, group bool) error {
	var c *call
	if a := cs.active; a != nil && a.chat == chat && bytes.Equal(a.id, id) && a.phase == CallRinging {
		c = a
	}
	if c == nil || c.media == nil {
		var err error
		if c, err = cs.newCall(chat, id, group); err != nil {
			return err
		}
	}
	c.incoming, c.from, c.phase = true, from, CallConnecting
	cs.setActive(c)
	cs.mu.Lock()
	delete(cs.rings, wire.EncodeB64(id))
	cs.mu.Unlock()
	if k := cs.known[chat]; k != nil {
		k.acked = true
	}
	cs.missed[wire.EncodeB64(id)] = true
	if err := cs.send(chat, CallContent{Call: id, Op: opAccept}); err != nil {
		cs.end(c, "The call couldn't connect: "+err.Error(), false)
		return err
	}
	cs.emitCall()
	return nil
}

// DeclineCall declines the call ringing in a chat (§15.2).
func (e *Engine) DeclineCall(ctx context.Context, chat string) error {
	cs := e.calls
	return cs.doWait(func() error {
		cs.mu.Lock()
		var ring *CallRing
		for k, r := range cs.rings {
			if r.Chat == chat || chat == "" {
				ring = r
				delete(cs.rings, k)
				break
			}
		}
		cs.mu.Unlock()
		if ring == nil {
			return errors.New("chat: no call is ringing here")
		}
		if a := cs.active; a != nil && a.phase == CallRinging && a.chat == ring.Chat {
			cs.mu.Lock()
			cs.active = nil
			cs.mu.Unlock()
		}
		cs.missed[ring.Call] = true
		id, _ := wire.DecodeB64(ring.Call)
		err := cs.send(ring.Chat, CallContent{Call: id, Op: opDecline})
		cs.systemLine(ring.Chat, "You declined a call from "+cs.who(ring.From))
		cs.emitCall()
		return err
	})
}

// HangUp leaves the call this device is in.
func (e *Engine) HangUp(ctx context.Context) error {
	cs := e.calls
	return cs.doWait(func() error {
		c := cs.active
		if c == nil || c.phase == CallEnded {
			return errors.New("chat: you're not in a call")
		}
		if c.phase == CallRinging {
			return errors.New("chat: answer or decline the call first")
		}
		cs.hangup(c, "")
		return nil
	})
}

// SetCallMuted mutes or unmutes the microphone in the call, and tells
// the others (§15.2).
func (e *Engine) SetCallMuted(on bool) error {
	cs := e.calls
	return cs.doWait(func() error {
		c := cs.active
		if c == nil || c.media == nil || c.phase == CallEnded || c.phase == CallRinging {
			return errors.New("chat: you're not in a call")
		}
		if !on && c.deafened {
			c.deafened = false
			c.audio.Mix.SetDeafened(false)
		}
		c.media.SetMuted(on)
		cs.sendState(c)
		cs.emitCall()
		return nil
	})
}

// SetCallDeafened stops or starts playing the call. Deafened, the
// microphone is muted too (§15.2).
func (e *Engine) SetCallDeafened(on bool) error {
	cs := e.calls
	return cs.doWait(func() error {
		c := cs.active
		if c == nil || c.media == nil || c.phase == CallEnded || c.phase == CallRinging {
			return errors.New("chat: you're not in a call")
		}
		c.deafened = on
		c.audio.Mix.SetDeafened(on)
		if on {
			c.media.SetMuted(true)
		}
		cs.sendState(c)
		cs.emitCall()
		return nil
	})
}

func (cs *calls) sendState(c *call) {
	muted := c.media.Muted()
	if err := cs.send(c.chat, CallContent{Call: c.id, Op: opState, Muted: &muted, Deafened: c.deafened}); err != nil {
		cs.e.log.Info("chat: sending call state", "err", err)
	}
}

// Call returns this device's call, if there is one, as of the last
// change or second.
func (e *Engine) Call() (CallInfo, bool) {
	cs := e.calls
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.last, cs.hasLast
}

// CallRings returns the rings this device hasn't answered.
func (e *Engine) CallRings() []CallRing {
	cs := e.calls
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []CallRing
	for _, r := range cs.rings {
		out = append(out, *r)
	}
	return out
}

// CallIn returns how many devices are in the call going on in a chat, as
// far as this device knows.
func (e *Engine) CallIn(chat string) int {
	cs := e.calls
	n := make(chan int, 1)
	cs.do(func() {
		if k := cs.known[chat]; k != nil {
			n <- len(k.in)
			return
		}
		n <- 0
	})
	return <-n
}

// hangup sends hangup and ends the call.
func (cs *calls) hangup(c *call, why string) {
	if err := cs.send(c.chat, CallContent{Call: c.id, Op: opHangup}); err != nil {
		cs.e.log.Info("chat: sending hangup", "err", err)
	}
	cs.end(c, why, true)
}

// end stops a call's media and connections, and adds its line to the
// chat.
func (cs *calls) end(c *call, why string, line bool) {
	if c.phase == CallEnded {
		return
	}
	wasActive := !c.since.IsZero()
	c.phase = CallEnded
	if why == "" {
		why = "The call ended"
	}
	c.ended = why
	for _, d := range c.devs {
		if d.peer != nil {
			d.peer.Close()
		}
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.audio.Close != nil {
		c.audio.Close()
	}
	if k := cs.known[c.chat]; k != nil && bytes.Equal(k.id, c.id) {
		delete(k.in, string(cs.e.c.DeviceKey()))
	}
	if !c.group {
		delete(cs.known, c.chat) // a direct call ends for both
		cs.over[wire.EncodeB64(c.id)] = true
	}
	cs.missed[wire.EncodeB64(c.id)] = true
	if line {
		if wasActive {
			cs.systemLine(c.chat, "Call, "+duration(time.Since(c.since)))
		} else {
			cs.systemLine(c.chat, why)
		}
	}
	cs.emitCall()
}

func duration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// who names an identity in a line: its handle, with @.
func (cs *calls) who(id string) string {
	if h := cs.e.Handle(id); h != "" {
		return "@" + h
	}
	return "someone"
}

func (cs *calls) systemLine(chat, text string) {
	e := cs.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch := e.st.Chats[chat]; ch != nil {
		e.systemLine(ch, text)
		e.save()
	}
}

// tick ends rings and calls that ran out of time.
func (cs *calls) tick() {
	now := time.Now()
	changed := false
	cs.mu.Lock()
	for k, r := range cs.rings {
		if now.Sub(r.At) > ringFor {
			delete(cs.rings, k)
			r := *r
			changed = true
			if !cs.missed[r.Call] {
				cs.missed[r.Call] = true
				defer cs.systemLine(r.Chat, "Missed call from "+cs.who(r.From))
			}
		}
	}
	c := cs.active
	cs.mu.Unlock()
	if c != nil && c.phase == CallEnded && cs.hasLast && cs.last.Phase == CallEnded {
		cs.setActive(nil) // shown as ended for a second, then gone
		c, changed = nil, true
	}
	if c != nil {
		switch {
		case c.phase == CallRinging && now.Sub(c.ringAt) > ringFor:
			cs.setActive(nil)
			changed = true
		case c.phase == CallCalling && now.Sub(c.ringAt) > ringFor:
			// Nobody answered: the caller hangs up (§15.2).
			cs.hangup(c, "No answer")
		case c.phase == CallConnecting && now.Sub(c.ringAt) > ringFor && len(c.devs) == 0:
			cs.hangup(c, "The call couldn't connect")
		case c.phase == CallActive || c.phase == CallConnecting:
			c.keys.refresh()
			c.media.SetSender(c.keys.sender())
			cs.checkPeers(c)
			if c.relay.due(now) {
				cs.e.renewRelay(cs.do, func(r *client.Relay, err error) { cs.renewed(c, r, err) })
			}
			changed = true
		}
	}
	for chat, k := range cs.known {
		if len(k.in) == 0 && now.Sub(k.at) > ringFor && (cs.active == nil || cs.active.chat != chat || cs.active.phase == CallEnded) {
			delete(cs.known, chat)
		}
	}
	if changed {
		cs.emitCall()
	}
}

// renewed takes new relay credentials during a long call (client API,
// "Call relays"). Connections to devices that join later use them, and so
// does any connection that gathers candidates again.
func (cs *calls) renewed(c *call, r *client.Relay, err error) {
	if cs.active != c || c.phase == CallEnded {
		return
	}
	if err != nil {
		cs.e.log.Info("chat: renewing the call relay", "err", err)
		c.relay.failed()
		return
	}
	c.rtcOpts.ICEServers = iceServers(r)
	c.relay.set(r)
	for _, d := range c.devs {
		if d.peer != nil {
			if err := d.peer.PC.SetConfiguration(c.rtcOpts.Config()); err != nil {
				cs.e.log.Info("chat: renewing the call relay", "err", err)
			}
		}
	}
}

// checkPeers drops devices whose connections failed, and ends a call
// with nobody left.
func (cs *calls) checkPeers(c *call) {
	connected := false
	for key, d := range c.devs {
		if d.peer == nil {
			continue
		}
		switch d.peer.PC.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			connected = true
			if c.since.IsZero() {
				c.since = time.Now()
			}
			c.phase = CallActive
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			d.peer.Close()
			delete(c.devs, key)
		}
	}
	if c.phase == CallActive && !connected && len(c.devs) == 0 {
		cs.hangup(c, "")
	}
}

// handleCall takes call content from the chat's group. dev is the
// sending device, and leaf its leaf index. The caller holds e.mu, so this
// only queues the work.
func (e *Engine) handleCall(ch *Chat, from string, dev []byte, am AppMessage) error {
	c, err := parseCall(am.C)
	if err != nil {
		return err
	}
	chat, group := ch.ID, ch.Kind == KindGroup
	ts := time.Unix(am.TS, 0)
	e.calls.do(func() { e.calls.handle(chat, group, from, dev, ts, c) })
	return nil
}

func (cs *calls) handle(chat string, group bool, from string, dev []byte, ts time.Time, m CallContent) {
	e := cs.e
	me, myDev := e.c.ID(), e.c.DeviceKey()
	mine := from == me
	key := string(dev)
	idStr := wire.EncodeB64(m.Call)
	c := cs.active
	inCall := c != nil && c.phase != CallEnded && c.phase != CallRinging && c.chat == chat && bytes.Equal(c.id, m.Call)
	k := cs.known[chat]
	if k != nil && !bytes.Equal(k.id, m.Call) && len(k.in) == 0 {
		delete(cs.known, chat) // the call it knew of is over
		k = nil
	}
	if k != nil && !bytes.Equal(k.id, m.Call) {
		if m.Op == opRing || m.Op == opAccept {
			// Two calls in one chat: everyone converges on the one whose
			// ID is lower (§15.2).
			if bytes.Compare(m.Call, k.id) < 0 {
				old := k.id
				k = nil
				delete(cs.known, chat)
				if c != nil && c.chat == chat && bytes.Equal(c.id, old) && c.phase != CallEnded {
					if c.phase == CallCalling || c.phase == CallActive || c.phase == CallConnecting {
						cs.hangup(c, "Joined the other call")
						defer func() { cs.join(chat, m.Call, from, group) }()
					} else {
						cs.setActive(nil)
					}
				}
				cs.mu.Lock()
				delete(cs.rings, wire.EncodeB64(old))
				cs.mu.Unlock()
			} else {
				return // the other call wins
			}
		} else {
			return
		}
	}
	if k == nil {
		if m.Op != opRing && m.Op != opAccept || cs.over[idStr] {
			return
		}
		k = &knownCall{id: m.Call, from: from, at: ts, in: map[string]string{}}
		cs.known[chat] = k
	}
	k.at = time.Now()
	defer cs.emitCall()

	switch m.Op {
	case opRing:
		k.in[key] = from
		k.from = from
		if mine {
			return // another device of this identity is calling
		}
		if time.Since(ts) > ringFor || cs.missed[idStr] {
			if !cs.missed[idStr] {
				cs.missed[idStr] = true
				cs.systemLine(chat, "Missed call from "+cs.who(from))
			}
			return
		}
		k.rang = true
		r := &CallRing{Chat: chat, Call: idStr, From: from, At: ts}
		cs.mu.Lock()
		cs.rings[idStr] = r
		cs.mu.Unlock()
		if c == nil || c.phase == CallEnded {
			cs.setActive(&call{chat: chat, id: m.Call, group: group, incoming: true, from: from, phase: CallRinging, ringAt: ts, devs: map[string]*callDev{}})
		}
		// Otherwise it's call waiting: shown, and nothing is sent.
	case opAccept:
		k.in[key] = from
		if mine && !bytes.Equal(dev, myDev) {
			cs.stopRinging(chat, idStr, "Answered on another device")
		}
		if !inCall {
			return
		}
		if c.phase == CallCalling {
			c.phase = CallConnecting
		}
		if _, ok := c.devs[key]; ok {
			return
		}
		if !c.group && len(c.devs) > 0 {
			return // a direct call connects to the first device that accepts
		}
		d := &callDev{id: from, dev: append([]byte(nil), dev...)}
		c.devs[key] = d
		cs.offer(c, d)
		if c.media.Muted() || c.deafened {
			cs.sendState(c) // a device that joins assumes everyone is unmuted
		}
	case opDecline:
		if mine {
			cs.stopRinging(chat, idStr, "Declined on another device")
			return
		}
		if inCall && !c.group && c.phase == CallCalling {
			cs.hangup(c, cs.who(from)+" declined the call")
		}
	case opHangup:
		delete(k.in, key)
		if !group {
			k.in = map[string]string{} // a direct call ends with either side
			cs.over[idStr] = true
		}
		cs.mu.Lock()
		r := cs.rings[idStr]
		cs.mu.Unlock()
		if r != nil && r.From == from && !group {
			cs.stopRinging(chat, idStr, "")
			if !cs.missed[idStr] {
				cs.missed[idStr] = true
				cs.systemLine(chat, "Missed call from "+cs.who(from))
			}
		}
		if group && len(k.in) == 0 {
			cs.stopRinging(chat, idStr, "")
		}
		if !inCall {
			return
		}
		if d := c.devs[key]; d != nil {
			if d.peer != nil {
				d.peer.Close()
			}
			delete(c.devs, key)
		}
		if len(c.devs) == 0 && (c.phase == CallActive || !c.group) && c.phase != CallCalling {
			// The last other participant hung up (§15.2).
			cs.end(c, "", true)
		}
	case opSDP, opICE:
		if m.To != me {
			return
		}
		if !bytes.Equal(m.Dev, myDev) {
			// Signaling for another device of this identity: it took the
			// call (§15.2).
			cs.stopRinging(chat, idStr, "Answered on another device")
			return
		}
		if !inCall {
			return
		}
		k.in[key] = from
		d := c.devs[key]
		if d == nil {
			if !c.group && len(c.devs) > 0 {
				return
			}
			d = &callDev{id: from, dev: append([]byte(nil), dev...)}
			c.devs[key] = d
		}
		if m.Op == opICE {
			if d.peer == nil {
				d.ice = append(d.ice, m.Data)
				return
			}
			if err := d.peer.AddCandidate(m.Data); err != nil {
				e.log.Info("chat: a call's ICE candidate", "err", err)
			}
			return
		}
		cs.remote(c, d, m.Data)
	case opState:
		k.in[key] = from
		if !inCall {
			return // a device that isn't in the call ignores state
		}
		if d := c.devs[key]; d != nil {
			if m.Muted != nil {
				d.muted = *m.Muted
			}
			d.deafened = m.Deafened
			if m.Cast != nil {
				d.cast = *m.Cast
			}
		}
	}
}

// stopRinging stops this device ringing for a call.
func (cs *calls) stopRinging(chat, id, why string) {
	cs.mu.Lock()
	_, rang := cs.rings[id]
	delete(cs.rings, id)
	c := cs.active
	if c != nil && c.phase == CallRinging && wire.EncodeB64(c.id) == id {
		cs.active = nil
	}
	cs.mu.Unlock()
	if rang && why != "" && !cs.missed[id] {
		cs.missed[id] = true
		cs.systemLine(chat, why)
	}
}

// newPeer makes the connection to one device in the call.
func (cs *calls) newPeer(c *call, d *callDev) error {
	p, err := rtc.NewPeer(c.api, c.rtcOpts, nil, func(t *webrtc.TrackRemote, kind string) {
		go c.media.Receive(t, kind)
	})
	if err != nil {
		return err
	}
	d.peer = p
	return nil
}

// offer sends an offer to a device that accepted.
func (cs *calls) offer(c *call, d *callDev) {
	if err := cs.newPeer(c, d); err != nil {
		cs.e.log.Info("chat: a call connection", "err", err)
		return
	}
	if err := d.peer.AddSections(c.media.Track); err != nil {
		cs.e.log.Info("chat: a call connection", "err", err)
		return
	}
	offer, err := d.peer.Offer()
	if err != nil {
		cs.e.log.Info("chat: a call offer", "err", err)
		return
	}
	d.offered = true
	cs.sendTo(c, d, opSDP, offer)
	cs.sendTo(c, d, opICE, `{"candidate":""}`)
}

// remote applies an offer or answer from a device in the call.
func (cs *calls) remote(c *call, d *callDev, data string) {
	var desc rtc.Description
	if err := wire.Unmarshal([]byte(data), &desc); err != nil {
		return
	}
	if desc.Type == "offer" && d.offered && d.peer != nil && d.peer.PC.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		// Crossing offers: the device with the lower key keeps its own
		// (§15.2).
		if bytes.Compare(cs.e.c.DeviceKey(), d.dev) < 0 {
			return
		}
		d.peer.Close()
		d.peer, d.offered = nil, false
	}
	if d.peer == nil {
		if desc.Type != "offer" {
			return
		}
		if err := cs.newPeer(c, d); err != nil {
			cs.e.log.Info("chat: a call connection", "err", err)
			return
		}
	}
	answer, err := d.peer.Remote(data, c.media.Track)
	if err != nil {
		cs.e.log.Info("chat: a call's session description", "err", err)
		return
	}
	for _, cand := range d.ice {
		d.peer.AddCandidate(cand)
	}
	d.ice = nil
	if desc.Type == "answer" {
		d.offered = false
		return
	}
	cs.sendTo(c, d, opSDP, answer)
	cs.sendTo(c, d, opICE, `{"candidate":""}`)
	if c.media.Muted() || c.deafened {
		cs.sendState(c)
	}
}

// callKeys are a call's SFrame keys, from the chat's MLS exporter
// (§15.2).
type callKeys struct {
	e    *Engine
	chat string
	call []byte

	mu     sync.Mutex
	epoch  uint64
	leaf   uint32
	base   []byte
	snd    *sframe.Sender
	old    map[uint64]oldKey // past epochs' base keys, by epoch % 16
	cache  map[uint64]*sframe.Key
	who    map[uint32]string // leaf index to identity
	loaded bool
}

type oldKey struct {
	base  []byte
	until time.Time
	epoch uint64
}

// refresh exports the base key for the group's current epoch, if it
// changed. The sender switches at once, and the previous epoch's key is
// kept for 10 seconds (§15.2).
func (k *callKeys) refresh() error {
	e := k.e
	e.mu.Lock()
	g := e.dev.mls.Group(mustB64(k.chat))
	st, err := g.State()
	var base []byte
	if err == nil && (!k.loaded || st.Epoch != k.epoch) {
		base, err = g.ExportSecret(sframe.ExporterLabel, k.call, 16)
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	for low, o := range k.old {
		if now.After(o.until) {
			delete(k.old, low)
			k.cache = nil
		}
	}
	who := map[uint32]string{}
	for _, m := range st.Members {
		if id, _, err := parseCredential(m.Identity); err == nil {
			who[m.LeafIndex] = id
		}
	}
	k.who = who
	if base == nil {
		return nil
	}
	if k.loaded {
		k.old[k.epoch%16] = oldKey{base: k.base, until: now.Add(oldKeysFor), epoch: k.epoch}
	}
	// A newer epoch with the same low 4 bits replaces the old key at once.
	delete(k.old, st.Epoch%16)
	k.epoch, k.leaf, k.base, k.loaded, k.cache = st.Epoch, st.OwnLeafIndex, base, true, nil
	s, err := sframe.NewSender(base, sframe.CallKID(st.OwnLeafIndex, st.Epoch))
	if err != nil {
		return err
	}
	k.snd = s
	return nil
}

func (k *callKeys) sender() *sframe.Sender {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.snd
}

// Key gives the key for a KID, and the identity of the leaf it names.
func (k *callKeys) Key(kid uint64) (string, *sframe.Key, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if key, ok := k.cache[kid]; ok {
		return k.who[uint32(kid>>4)], key, key != nil
	}
	low := kid & 15
	var base []byte
	switch {
	case k.loaded && k.epoch%16 == low:
		base = k.base
	default:
		if o, ok := k.old[low]; ok {
			base = o.base
		}
	}
	if k.cache == nil {
		k.cache = map[uint64]*sframe.Key{}
	}
	if base == nil {
		return "", nil, false
	}
	key, err := sframe.Derive(base, kid)
	if err != nil {
		return "", nil, false
	}
	k.cache[kid] = key
	return k.who[uint32(kid>>4)], key, true
}

// DirectCalls says whether calls may connect directly, which shows each
// side's IP address to the other. Off by default: calls are relayed
// (§15.2).
func (e *Engine) DirectCalls() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.DirectCalls
}

// SetDirectCalls allows or stops direct connections for calls.
func (e *Engine) SetDirectCalls(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.DirectCalls = on
	return e.save()
}
