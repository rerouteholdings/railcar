package ui

import (
	"strings"
	"testing"
)

// TestCallBar calls Ravi in the demo: the call bar shows the call, its
// mute state, and goes away after hanging up, with a line in the chat.
func TestCallBar(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "ravi", false))
	runCommand(a, b, "call")
	out := plain(a)
	if !strings.Contains(out, "@ravi.chat.example.com") || !strings.Contains(out, "0:00") || !strings.Contains(out, ":hangup") {
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
	if !strings.Contains(out, "#Lounge in Night shift") || !strings.Contains(out, "You're in this voice channel.") || !strings.Contains(out, ":leave-voice") {
		t.Fatalf("the voice bar:\n%s", out)
	}
	runCommand(a, b, "leave-voice")
	if a.main.call != nil {
		t.Fatal("the voice bar is still there")
	}
}
