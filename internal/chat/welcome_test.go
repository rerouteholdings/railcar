package chat

import (
	"slices"
	"testing"
)

// TestWelcomeFrom checks who a Welcome may come from, also for a device
// coming back as a new device: its own identity or a contact (§10.3,
// §11.9).
func TestWelcomeFrom(t *testing.T) {
	for _, c := range []struct {
		from string
		c    *Contact
		ok   bool
	}{
		{"me", nil, true},
		{"kat", &Contact{State: StateContact}, true},
		{"kat", &Contact{State: StatePendingOut}, true},
		{"kat", &Contact{State: StatePendingIn}, false},
		{"kat", &Contact{State: StateBlocked}, false},
		{"lee", nil, false},
	} {
		if got := welcomeFrom("me", c.from, c.c); got != c.ok {
			t.Errorf("from %s with %+v: %v", c.from, c.c, got)
		}
	}
}

// TestNewerBound checks which bound histories become this device's copy
// (§11.9): only other identities', and only when longer than the copy.
func TestNewerBound(t *testing.T) {
	got := newerBound("me", map[string]int{"me": 9, "kat": 3, "lee": 2, "ravi": 4}, map[string]int{"kat": 2, "lee": 2, "me": 1})
	if !slices.Equal(got, []string{"kat", "ravi"}) {
		t.Fatalf("kept %v", got)
	}
}

// TestWelcomeFromMember checks who may add a device to a chat it waits
// for (§7.1): its own identity or a member it knows of, or, with no list,
// anyone the usual rule takes.
func TestWelcomeFromMember(t *testing.T) {
	members := []string{"me", "kat"}
	for _, c := range []struct {
		from string
		list []string
		ok   bool
	}{
		{"me", members, true},
		{"kat", members, true},
		{"lee", members, false},
		{"lee", nil, true},
		{"me", []string{}, true},
		{"kat", []string{}, false},
	} {
		if got := welcomeFromMember("me", c.from, c.list); got != c.ok {
			t.Errorf("from %s with %v: %v", c.from, c.list, got)
		}
	}
}
