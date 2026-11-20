package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestCallBar calls Ravi in the demo: the call bar shows the call, its
// mute state, and goes away after hanging up, with a line in the chat.
func TestCallBar(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "ravi", false))
	runCommand(a, b, "call")
	out := plain(a)
	if !strings.Contains(out, "@ravi.chat.example.com") || !strings.Contains(out, "0:00") || !strings.Contains(out, "ctrl+x hang up") {
		t.Fatalf("the call bar:\n%s", out)
	}
	runCommand(a, b, "mute")
	if !strings.Contains(plain(a), "Muted") {
		t.Fatal("the call bar doesn't say muted")
	}
	runCommand(a, b, "hangup")
	if a.main.call != nil {
		t.Fatal("the call bar is still there")
	}
	if !strings.Contains(plain(a), "Call, 0:00") {
		t.Fatalf("no call line:\n%s", plain(a))
	}
}

// TestVoiceBar joins the demo's voice channel.
func TestVoiceBar(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "c-lounge", false))
	runCommand(a, b, "join-voice")
	out := plain(a)
	if !strings.Contains(out, "#Lounge in Night shift") || !strings.Contains(out, "You're in this voice channel.") || !strings.Contains(out, "ctrl+x leave") {
		t.Fatalf("the voice bar:\n%s", out)
	}
	runCommand(a, b, "leave-voice")
	if a.main.call != nil {
		t.Fatal("the voice bar is still there")
	}
}

// TestCallKeys answers, mutes, and hangs up with the call keys, from the
// composer.
func TestCallKeys(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "ravi", true))
	ctrl := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }
	send(a, b, ctrl('y'))
	if a.main.notice != "Nothing is ringing" {
		t.Fatalf("Ctrl+Y with nothing ringing: %q", a.main.notice)
	}
	runCommand(a, b, "call")
	if a.main.call == nil {
		t.Fatal("no call")
	}
	send(a, b, ctrl('r'))
	if !a.main.call.Muted || !strings.Contains(plain(a), "ctrl+r unmute") {
		t.Fatalf("Ctrl+R didn't mute:\n%s", plain(a))
	}
	send(a, b, ctrl('r'))
	if a.main.call.Muted {
		t.Fatal("Ctrl+R didn't unmute")
	}
	if a.main.composer.Value() != "" {
		t.Fatalf("the keys reached the composer: %q", a.main.composer.Value())
	}
	send(a, b, ctrl('x'))
	if a.main.call != nil {
		t.Fatal("Ctrl+X didn't hang up")
	}
}

// TestAudioDevices chooses a microphone in the demo.
func TestAudioDevices(t *testing.T) {
	a, b := started(t, 120, 40)
	runCommand(a, b, "audio-devices")
	if a.main.overlay != overlayAudio || !strings.Contains(plain(a), "USB headset") || !strings.Contains(plain(a), "System default  (chosen)") {
		t.Fatalf("the audio devices:\n%s", plain(a))
	}
	send(a, b, press('j'), tea.KeyPressMsg{Code: tea.KeyEnter})
	if s, _ := b.AudioDevices(t.Context()); s.Microphone != "Built-in microphone" {
		t.Fatalf("the microphone is %q", s.Microphone)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.main.overlay != overlayNone {
		t.Fatal("Esc didn't close it")
	}
}
