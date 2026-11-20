package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// CallBar is this device's call or voice channel, as the call bar shows
// it (§15.2, §16.4).
type CallBar struct {
	// Voice is true for a voice channel, false for a call.
	Voice bool
	Chat  string
	// Where names the chat or channel, such as "@ravi.example.org" or
	// "#Lounge in Night shift".
	Where string
	// Phase is "ringing", "calling", "connecting", "active", or "ended".
	Phase    string
	From     Member // who's calling, while ringing
	Since    time.Time
	Muted    bool
	Deafened bool
	Level    float64 // this device's microphone, from 0 to 1
	People   []CallMember
	// Waiting names people calling from other chats.
	Waiting []string
	Ended   string
	// Direct is true when the call may connect directly, which shows IP
	// addresses.
	Direct bool
}

// CallMember is someone else in the call or voice channel.
type CallMember struct {
	Member
	Connected bool
	Muted     bool
	Deafened  bool
	Speaking  bool
	Cast      []string
}

// CallChanged reports a change to the call bar. Bar is nil when there's
// no call and no voice channel.
type CallChanged struct{ Bar *CallBar }

func (CallChanged) event() {}

// callChanged takes a new call bar, and notifies about a new ring.
func (m *mainView) callChanged(a *App, bar *CallBar) tea.Cmd {
	was := m.call
	m.call = bar
	m.resize(a)
	if bar == nil || a.opts.Demo {
		return nil
	}
	ringing := bar.Phase == "ringing" && (was == nil || was.Phase != "ringing" || was.Chat != bar.Chat)
	waiting := len(bar.Waiting) > 0 && (was == nil || len(was.Waiting) < len(bar.Waiting))
	switch {
	case ringing:
		m.notice = "Call from " + who(bar.From) + ". Press Ctrl+Y to answer, or Ctrl+X to decline"
		return notifyCmd(m.notify, "Railcar", "Call from "+who(bar.From))
	case waiting:
		m.notice = bar.Waiting[len(bar.Waiting)-1] + " is calling. Press Ctrl+Y to end this call and answer"
		return notifyCmd(m.notify, "Railcar", bar.Waiting[len(bar.Waiting)-1]+" is calling")
	}
	return nil
}

func who(mb Member) string {
	if mb.Name != "" {
		return mb.Name
	}
	return "@" + mb.Handle
}

// callBarView is the line above the status bar while there's a call or
// a voice channel.
func (m *mainView) callBarView(a *App) string {
	c := m.call
	if c == nil {
		return ""
	}
	t := a.theme
	st := t.status
	seg := func(s lipgloss.Style, text string) string { return s.Background(t.p.panel).Render(text) }
	var parts []string
	switch c.Phase {
	case "ringing":
		parts = append(parts, seg(t.accent.Bold(true), " ☎ Call from "+who(c.From)), seg(st, " in "+c.Where),
			seg(t.statusKey, "   ctrl+y answer  ctrl+x decline"))
	case "calling":
		parts = append(parts, seg(t.accent, " ☎ Calling "+c.Where), seg(t.statusKey, "   ctrl+x hang up"))
	case "ended":
		parts = append(parts, seg(st, " "+c.Ended))
	default:
		label := " ☎ "
		if c.Voice {
			label = " 🔊 "
		}
		parts = append(parts, seg(t.ok, label)+seg(t.statusValue, c.Where))
		if c.Phase == "connecting" {
			parts = append(parts, seg(st, "  Connecting"))
		} else if !c.Since.IsZero() {
			parts = append(parts, seg(st, "  "+clock(a.opts.Now().Sub(c.Since))))
		}
		switch {
		case c.Deafened:
			parts = append(parts, seg(t.danger, "  Deafened"))
		case c.Muted:
			parts = append(parts, seg(t.danger, "  Muted"))
		default:
			parts = append(parts, seg(st, "  "+meter(c.Level)))
		}
		for _, p := range c.People {
			name := "@" + p.Handle
			s := st
			switch {
			case p.Speaking:
				s, name = t.ok, "▶"+name
			case !p.Connected && !c.Voice:
				name += " (connecting)"
			}
			if p.Deafened {
				name += " (deafened)"
			} else if p.Muted {
				name += " (muted)"
			}
			if len(p.Cast) > 0 {
				name += " (casting " + strings.Join(p.Cast, " and ") + ")"
			}
			parts = append(parts, seg(s, "  "+name))
		}
		if c.Direct {
			parts = append(parts, seg(t.statusKey, "  direct"))
		}
		mute, hang := "mute", "hang up"
		if c.Muted || c.Deafened {
			mute = "unmute"
		}
		if c.Voice {
			hang = "leave"
		}
		parts = append(parts, seg(t.statusKey, "   ctrl+r "+mute+"  ctrl+x "+hang))
	}
	line := strings.Join(parts, "")
	w := a.width
	if lw := lipgloss.Width(line); lw < w {
		line += seg(st, strings.Repeat(" ", w-lw))
	} else {
		line = ansi.Truncate(line, w, "…")
	}
	return line
}

// clock shows a call's length, as 4:05 or 1:02:03.
func clock(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// meter draws a microphone level as five bars.
func meter(level float64) string {
	n := min(5, int(level*25+0.5))
	return "mic " + strings.Repeat("▮", n) + strings.Repeat("▯", 5-n)
}

// callKey runs a call key: Ctrl+Y answers, Ctrl+X hangs up, declines a
// ring, or leaves a voice channel, and Ctrl+R mutes or unmutes.
func (m *mainView) callKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	c := m.call
	live := c != nil && c.Phase != "ringing" && c.Phase != "ended"
	switch {
	case key.Matches(msg, a.keys.Answer):
		if c != nil && (c.Phase == "ringing" || len(c.Waiting) > 0) {
			return m.act(a, "answer", "", "", false)
		}
		m.notice = "Nothing is ringing"
	case key.Matches(msg, a.keys.HangUp):
		switch {
		case c != nil && c.Phase == "ringing":
			return m.act(a, "decline-call", "", "", false)
		case live && c.Voice:
			return m.act(a, "leave-voice", "", "", false)
		case live:
			return m.act(a, "hangup", "", "", false)
		}
		m.notice = "You're not in a call"
	case key.Matches(msg, a.keys.Mute):
		if live {
			if c.Muted || c.Deafened {
				return m.act(a, "unmute", "", "", false)
			}
			return m.act(a, "mute", "", "", false)
		}
		m.notice = "You're not in a call"
	}
	return nil
}

// callCommands are the palette's commands for calls and voice channels.
func callCommands(m *mainView) []command {
	act := func(kind string) func(a *App) tea.Cmd {
		return func(a *App) tea.Cmd { return a.main.act(a, kind, "", "", false) }
	}
	cmds := []command{}
	c := m.call
	if c != nil && c.Phase == "ringing" || c != nil && len(c.Waiting) > 0 {
		cmds = append(cmds,
			command{label: "Answer the call", names: []string{"answer"}, run: act("answer")},
			command{label: "Decline the call", names: []string{"decline-call"}, run: act("decline-call")})
	}
	if c != nil && c.Phase != "ringing" && c.Phase != "ended" {
		mute, unmute := "mute", "unmute"
		cmds = append(cmds,
			command{label: "Mute your microphone", names: []string{mute}, run: act(mute)},
			command{label: "Unmute your microphone", names: []string{unmute}, run: act(unmute)},
			command{label: "Deafen: hear nothing, and mute", names: []string{"deafen"}, run: act("deafen")},
			command{label: "Undeafen", names: []string{"undeafen"}, run: act("undeafen")})
		if c.Voice {
			cmds = append(cmds, command{label: "Leave the voice channel", names: []string{"leave-voice", "hangup"}, run: act("leave-voice")})
		} else {
			cmds = append(cmds, command{label: "Hang up", names: []string{"hangup", "hang-up"}, run: act("hangup")})
		}
	}
	if ch, ok := m.activeChat(); ok {
		switch {
		case ch.Kind == DirectChat || ch.Kind == GroupChat:
			cmds = append(cmds, command{label: "Call " + ch.Title, names: []string{"call"}, run: act("call")})
		case ch.Kind == VoiceChannel:
			cmds = append(cmds, command{label: "Join " + ch.Title, names: []string{"join-voice"}, run: act("join-voice")})
		}
	}
	cmds = append(cmds, command{label: "Choose the microphone and speakers", names: []string{"audio-devices", "audio"}, run: func(a *App) tea.Cmd { return a.main.openAudio(a) }})
	cmds = append(cmds, argCommand("Allow direct calls, which show your IP address: on or off", "direct-calls", "direct-calls", "on or off", false))
	return cmds
}
