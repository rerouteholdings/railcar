package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestRemoveContact asks before removing a contact, and Esc keeps them.
func TestRemoveContact(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "ravi", false))
	runCommand(a, b, "block")
	if a.main.overlay != overlayConfirm || !strings.Contains(plain(a), "Block @ravi.chat.example.com?") {
		t.Fatalf("no question:\n%s", plain(a))
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	if c, _ := a.main.activeChat(); c.Stopped != "" {
		t.Fatal("Esc blocked them")
	}
	runCommand(a, b, "remove-contact")
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if c, _ := a.main.activeChat(); !strings.HasPrefix(c.Stopped, "You removed @ravi.chat.example.com") {
		t.Fatalf("after removing: %+v", c)
	}
}
