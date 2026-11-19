package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDetectNotify(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want notifyKind
	}{
		{map[string]string{"TERM_PROGRAM": "iTerm.app"}, notifyOSC9},
		{map[string]string{"WT_SESSION": "1"}, notifyOSC9},
		{map[string]string{"TERM": "foot"}, notifyOSC777},
		{map[string]string{"VTE_VERSION": "7600"}, notifyOSC777},
		{map[string]string{"TERM": "xterm-256color"}, notifyBell},
	} {
		if got := detectNotify(func(k string) string { return c.env[k] }); got != c.want {
			t.Errorf("%v: %v, want %v", c.env, got, c.want)
		}
	}
}

func TestNotifySequence(t *testing.T) {
	msg := notifyCmd(notifyOSC9, "Railcar", "Kat mentioned you\x07; in #general")()
	raw, ok := msg.(tea.RawMsg)
	if !ok {
		t.Fatalf("%T", msg)
	}
	s := raw.Msg.(string)
	// The bell, then the notification; the text can't end it early.
	if !strings.HasPrefix(s, "\a\x1b]9;Railcar: ") || strings.Count(s, "\a") != 2 || !strings.Contains(s, "you   in #general") {
		t.Fatalf("%q", s)
	}
}
