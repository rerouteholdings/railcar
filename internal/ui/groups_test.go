package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNewGroupChat(t *testing.T) {
	a, b := started(t, 120, 40)
	runCommand(a, b, "new-group")
	if a.main.overlay != overlayNewGroup || !strings.Contains(plain(a), "New group chat") {
		t.Fatal("the create-group box didn't open")
	}
	send(a, b, typeText("Signal box")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyTab}, tea.KeyPressMsg{Code: tea.KeySpace}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayNone {
		t.Fatalf("the box is still open: %q", a.main.grp.err)
	}
	c, ok := a.main.activeChat()
	if !ok || c.Kind != GroupChat || c.Title != "Signal box" || len(c.Members) != 1 {
		t.Fatalf("the new group: %+v", c)
	}
	runCommand(a, b, "rename Points")
	if c, _ := a.main.activeChat(); c.Title != "Points" {
		t.Fatalf("after renaming: %q", c.Title)
	}
}

func TestGroupDetailsShowAdmins(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "trail", false))
	a.main.toggleDetails(a)
	out := plain(a)
	if !strings.Contains(out, "Mia Lund (admin)") || !strings.Contains(out, "You (admin)") || !strings.Contains(out, "Saturday's ridge trail") {
		t.Fatalf("the details pane:\n%s", out)
	}
}

func TestCommunityChannels(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "c-general", false))
	out := plain(a)
	if !strings.Contains(out, "Anything goes") || !strings.Contains(out, "Coffee's on in the signal box.") {
		t.Fatalf("#general:\n%s", out)
	}
	drive(a, b, a.main.openChat(a, "c-lounge", false))
	if out := plain(a); !strings.Contains(out, "Railcar can't join voice channels.") || !strings.Contains(out, "@ravi.chat.example.com") {
		t.Fatalf("the voice channel:\n%s", out)
	}
	// Folding the community hides its channels.
	a.main.setFocus(a, focusChats)
	for i, it := range a.main.list.VisibleItems() {
		if _, ok := it.(communityItem); ok {
			a.main.list.Select(i)
		}
	}
	before := len(a.main.list.VisibleItems())
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if after := len(a.main.list.VisibleItems()); after >= before {
		t.Fatalf("folding: %d items, then %d", before, after)
	}
}
