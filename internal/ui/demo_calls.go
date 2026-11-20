package ui

import (
	"errors"
	"math/rand/v2"
	"time"
)

// demoCall is the demo's call or voice channel. The caller holds d.mu.
type demoCall struct {
	bar  CallBar
	stop chan struct{}
}

// actCalls runs the call and voice channel actions in the demo. Nothing
// leaves the machine; the people in a call take turns speaking.
func (d *DemoBackend) actCalls(chat string, a Action) (string, bool, error) {
	c := d.chats[chat]
	cur := d.call
	inCall := cur != nil && cur.bar.Phase != "ended" && cur.bar.Phase != "ringing"
	switch a.Kind {
	case "call", "join-voice":
		if c == nil {
			return "", true, errors.New("Open a chat first")
		}
		if inCall {
			return "", true, errors.New("Hang up first")
		}
		voice := c.Kind == VoiceChannel
		if a.Kind == "join-voice" && !voice {
			return "", true, errors.New("That isn't a voice channel")
		}
		if !voice && c.Kind != DirectChat && c.Kind != GroupChat {
			return "", true, errors.New("Calls are for direct chats and group chats")
		}
		bar := CallBar{Voice: voice, Chat: chat, Where: c.Title, Phase: "calling"}
		people := c.Members
		if voice {
			bar.Where, bar.Phase, people = "#"+c.Title+" in "+c.Community, "connecting", c.InVoice
		} else if c.Kind == DirectChat && len(c.Members) == 1 {
			bar.Where = "@" + c.Members[0].Handle
		}
		d.startCall(bar, people, 2*time.Second)
		if voice {
			return "Joining " + c.Title, true, nil
		}
		return "Calling", true, nil
	case "answer":
		if cur == nil || cur.bar.Phase != "ringing" {
			return "", true, errors.New("No call is ringing")
		}
		cur.bar.Phase = "connecting"
		d.startCall(cur.bar, []Member{cur.bar.From}, 500*time.Millisecond)
		return "Answering", true, nil
	case "decline-call":
		if cur == nil || cur.bar.Phase != "ringing" {
			return "", true, errors.New("No call is ringing")
		}
		d.endCall("You declined a call from " + who(cur.bar.From))
		return "Declined", true, nil
	case "hangup", "leave-voice":
		if !inCall {
			return "", true, errors.New("You're not in a call")
		}
		line := "Call, " + clock(d.opts.Now().Sub(cur.bar.Since))
		if cur.bar.Voice {
			line = ""
		}
		d.endCall(line)
		return "", true, nil
	case "mute", "unmute", "deafen", "undeafen":
		if !inCall {
			return "", true, errors.New("You're not in a call")
		}
		switch a.Kind {
		case "mute":
			cur.bar.Muted = true
		case "unmute":
			cur.bar.Muted, cur.bar.Deafened = false, false
		case "deafen":
			cur.bar.Muted, cur.bar.Deafened = true, true
		case "undeafen":
			cur.bar.Deafened = false
		}
		d.emitCall()
		return "Done", true, nil
	case "direct-calls":
		return "The demo's calls go nowhere", true, nil
	}
	return "", false, nil
}

// startCall connects the demo's call after a moment.
func (d *DemoBackend) startCall(bar CallBar, people []Member, after time.Duration) {
	if d.call != nil && d.call.stop != nil {
		close(d.call.stop)
	}
	call := &demoCall{bar: bar, stop: make(chan struct{})}
	d.call = call
	d.emitCall()
	connect := func() {
		if d.call != call {
			return
		}
		call.bar.Phase, call.bar.Since = "active", d.opts.Now()
		for _, p := range people {
			call.bar.People = append(call.bar.People, CallMember{Member: p, Connected: true})
		}
		d.emitCall()
		if !d.opts.Live {
			return
		}
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-call.stop:
					return
				case <-d.stop:
					return
				case <-t.C:
				}
				d.mu.Lock()
				talker := rand.IntN(len(call.bar.People) + 1)
				for i := range call.bar.People {
					call.bar.People[i].Speaking = i == talker
				}
				call.bar.Level = 0
				if !call.bar.Muted {
					call.bar.Level = rand.Float64() * 0.15
				}
				d.emitCall()
				d.mu.Unlock()
			}
		}()
	}
	if !d.opts.Live {
		connect() // the caller holds d.mu
		return
	}
	d.later(after, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		connect()
	})
}

// endCall ends the demo's call, with a line in its chat.
func (d *DemoBackend) endCall(line string) {
	call := d.call
	if call == nil {
		return
	}
	if call.stop != nil {
		close(call.stop)
		call.stop = nil
	}
	if line != "" {
		d.systemLine(call.bar.Chat, line)
	}
	d.call = nil
	d.emit(CallChanged{})
}

func (d *DemoBackend) emitCall() {
	if d.call == nil {
		d.emit(CallChanged{})
		return
	}
	b := d.call.bar
	b.People = append([]CallMember(nil), b.People...)
	d.emit(CallChanged{Bar: &b})
}

// ringLater has Ravi call a little after the demo connects, so the call
// bar shows without a command. Unanswered, it's a missed call.
func (d *DemoBackend) ringLater() {
	d.later(8*time.Second, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.call != nil || d.chats["ravi"] == nil {
			return
		}
		call := &demoCall{bar: CallBar{Chat: "ravi", Where: "@" + demoRavi.Handle, Phase: "ringing", From: demoRavi}}
		d.call = call
		d.emitCall()
		d.later(60*time.Second, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.call == call && call.bar.Phase == "ringing" {
				d.endCall("Missed call from " + who(demoRavi))
			}
		})
	})
}
