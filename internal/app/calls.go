package app

import (
	"context"
	"errors"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// speaking is the level above which someone shows as speaking.
const speaking = 0.02

// actCalls runs the actions for calls and voice channels (§15.2, §16.4).
func actCalls(ctx context.Context, eng *chat.Engine, chatID string, a ui.Action) (string, bool, error) {
	inVoice := func() bool { _, ok := eng.Voice(); return ok }
	switch a.Kind {
	case "call":
		if chatID == "" {
			return "", true, errors.New("open the chat to call first")
		}
		if c, ok := eng.Chat(chatID); ok && c.Voice {
			return "", true, eng.JoinVoice(ctx, chatID)
		}
		if inVoice() {
			return "", true, errors.New("leave the voice channel first")
		}
		return "Calling", true, eng.StartCall(ctx, chatID)
	case "answer":
		if inVoice() {
			if err := eng.LeaveVoice(ctx); err != nil {
				return "", true, err
			}
		}
		target := ""
		if c, ok := eng.Call(); ok && c.Phase == chat.CallRinging {
			target = c.Chat
		} else if rings := eng.CallRings(); len(rings) > 0 {
			target = rings[0].Chat
		} else {
			target = chatID // a group call going on in the open chat
		}
		return "Answering", true, eng.AnswerCall(ctx, target)
	case "decline-call":
		target := ""
		if c, ok := eng.Call(); ok && c.Phase == chat.CallRinging {
			target = c.Chat
		} else if rings := eng.CallRings(); len(rings) > 0 {
			target = rings[0].Chat
		}
		return "Declined", true, eng.DeclineCall(ctx, target)
	case "hangup":
		if inVoice() {
			return "You left the voice channel", true, eng.LeaveVoice(ctx)
		}
		return "", true, eng.HangUp(ctx)
	case "mute", "unmute":
		on := a.Kind == "mute"
		msg := map[bool]string{true: "Muted", false: "Unmuted"}[on]
		if inVoice() {
			return msg, true, eng.SetVoiceMuted(on)
		}
		return msg, true, eng.SetCallMuted(on)
	case "deafen", "undeafen":
		on := a.Kind == "deafen"
		msg := map[bool]string{true: "Deafened: you hear nothing, and you're muted", false: "Undeafened"}[on]
		if inVoice() {
			return msg, true, eng.SetVoiceDeafened(on)
		}
		return msg, true, eng.SetCallDeafened(on)
	case "join-voice":
		if c, ok := eng.Call(); ok && c.Phase != chat.CallEnded && c.Phase != chat.CallRinging {
			return "", true, errors.New("hang up the call first")
		}
		return "Joining", true, eng.JoinVoice(ctx, chatID)
	case "leave-voice":
		return "You left the voice channel", true, eng.LeaveVoice(ctx)
	case "direct-calls":
		switch a.Arg {
		case "on":
			return "Calls may connect directly now, which shows your IP address to the people you call and to community servers", true, eng.SetDirectCalls(true)
		case "off":
			return "Calls go through your homeserver's relay, which hides your IP address", true, eng.SetDirectCalls(false)
		}
		return "", true, errors.New("type :direct-calls on or :direct-calls off")
	}
	return "", false, nil
}

// callBar is the call bar for the interface: the voice channel this
// device is in, or else its call.
func callBar(eng *chat.Engine) *ui.CallBar {
	if v, ok := eng.Voice(); ok {
		b := &ui.CallBar{Voice: true, Chat: v.Chat, Phase: "active", Muted: v.Muted, Deafened: v.Deafened, Level: v.Level}
		if !v.Joined {
			b.Phase = "connecting"
		}
		if v.Ended != "" {
			b.Phase, b.Ended = "ended", v.Ended
		}
		if c, ok := eng.Chat(v.Chat); ok {
			b.Where = "#" + c.Name + " in " + c.CommunityName
		}
		for _, p := range v.People {
			if p.Self {
				continue
			}
			b.People = append(b.People, ui.CallMember{Member: member(eng, p.ID), Connected: p.Keyed, Muted: p.Muted, Deafened: p.Deafened, Speaking: p.Level > speaking, Cast: p.Cast})
		}
		return b
	}
	c, ok := eng.Call()
	if !ok {
		return nil
	}
	b := &ui.CallBar{Chat: c.Chat, Phase: string(c.Phase), From: member(eng, c.From), Since: c.Since, Muted: c.Muted,
		Deafened: c.Deafened, Level: c.Level, Ended: c.Ended, Direct: !c.Relay}
	if ch, ok := eng.Chat(c.Chat); ok {
		if ch.Kind == chat.KindGroup {
			b.Where = ch.Name
		} else {
			b.Where = "@" + eng.Handle(ch.Peer)
		}
	}
	for _, p := range c.People {
		b.People = append(b.People, ui.CallMember{Member: member(eng, p.ID), Connected: p.Connected, Muted: p.Muted, Deafened: p.Deafened, Speaking: p.Level > speaking, Cast: p.Cast})
	}
	for _, r := range c.Waiting {
		b.Waiting = append(b.Waiting, "@"+eng.Handle(r.From))
	}
	return b
}

// AudioDevices implements ui.Backend.
func (b *Backend) AudioDevices(context.Context) (ui.AudioSetup, error) {
	eng, err := b.engine()
	if err != nil {
		return ui.AudioSetup{}, err
	}
	var out ui.AudioSetup
	out.Microphone, out.Speaker = eng.AudioDevices()
	ds, err := audio.List()
	if err != nil {
		return out, errors.New("Railcar couldn't list the audio devices: " + err.Error())
	}
	out.Microphones, out.Speakers = ds.Microphones, ds.Speakers
	return out, nil
}

// SetAudioDevices implements ui.Backend.
func (b *Backend) SetAudioDevices(_ context.Context, mic, speakers string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	return eng.SetAudioDevices(mic, speakers)
}
