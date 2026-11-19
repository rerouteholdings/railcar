package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func runCommand(a *App, b *DemoBackend, line string) {
	send(a, b, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	send(a, b, typeText(line)...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestAddContact(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	if a.main.overlay != overlayAddContact || !strings.Contains(plain(a), "Add a contact") {
		t.Fatal("the add-contact box didn't open")
	}
	// A code in the wrong form is refused, and the box stays.
	send(a, b, typeText("ravi.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayAddContact || !strings.Contains(plain(a), "isn't a contact code") {
		t.Fatalf("a bad code: overlay %v, error %q", a.main.overlay, a.main.addErr)
	}
	a.main.addInput.SetValue("")
	send(a, b, typeText("@lee.example.org::Kx82mPq1Za")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayNone {
		t.Fatalf("the box is still open: %q", a.main.addErr)
	}
	found := false
	for _, c := range a.main.chats {
		if c.Kind == SentRequest && c.Title == "@lee.example.org" {
			found = true
		}
	}
	if !found {
		t.Fatal("the sent request isn't in the chat list")
	}
	// The palette takes the code too.
	runCommand(a, b, "add @noa.example.net::Kx82mPq1Zb")
	if a.main.overlay != overlayAddContact || a.main.addInput.Value() != "@noa.example.net::Kx82mPq1Zb" {
		t.Fatalf("overlay %v, input %q", a.main.overlay, a.main.addInput.Value())
	}
}

func TestInvite(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	runCommand(a, b, "invite")
	if a.main.overlay != overlayInvite || !strings.Contains(plain(a), "@kat.chat.example.com::"+demoInviteCode) {
		t.Fatal("the invite code isn't showing")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.main.overlay != overlayNone {
		t.Error("Esc didn't close it")
	}
}

func TestContactRequest(t *testing.T) {
	a, b := started(t, 120, 40)
	b.mu.Lock()
	req := &Chat{ID: "request:lee", Kind: ContactRequest, Title: "@lee.example.org", Members: []Member{{Handle: "lee.example.org"}},
		Preview: "Wants to be your contact", LastActivity: b.opts.Now()}
	b.chats[req.ID] = req
	b.emit(ChatChanged{Chat: *req})
	b.mu.Unlock()
	drive(a, b, nil)
	_, cmd := a.Update(tea.KeyPressMsg{}) // nothing; just redraw
	drive(a, b, cmd)
	drive(a, b, a.main.openChat(a, req.ID, true))
	if a.main.focus == focusComposer {
		t.Error("a request has no composer")
	}
	if !strings.Contains(plain(a), "@lee.example.org wants to be your contact.") {
		t.Fatal("the request isn't showing")
	}
	send(a, b, tea.KeyPressMsg{Code: 'a', Text: "a"})
	for _, c := range a.main.chats {
		if c.ID == req.ID {
			t.Fatal("the request is still listed after accepting")
		}
	}
	found := false
	for _, c := range a.main.chats {
		if c.Kind == DirectChat && c.Title == "@lee.example.org" {
			found = true
		}
	}
	if !found {
		t.Fatal("no direct chat after accepting")
	}
}

func TestEditReactDeleteAndReply(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter}) // into Ravi's composer
	send(a, b, typeText("frist")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape}) // to the messages
	send(a, b, tea.KeyPressMsg{Code: 'e', Text: "e"})
	if a.main.editing == "" || a.main.composer.Value() != "frist" {
		t.Fatalf("edit mode: %q, composer %q", a.main.editing, a.main.composer.Value())
	}
	a.main.composer.SetValue("first")
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := a.main.messages["ravi"]
	last := msgs[len(msgs)-1]
	if last.Text != "first" || !last.Edited {
		t.Fatalf("after the edit: %+v", last)
	}
	if !strings.Contains(plain(a), "(edited)") {
		t.Error("the edit isn't marked")
	}

	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	runCommand(a, b, "react 🎉")
	theirs, _ := a.main.lastMessage(false)
	if len(theirs.Reactions) != 1 || theirs.Reactions[0].Emoji != "🎉" || !theirs.Reactions[0].Mine {
		t.Fatalf("the reaction: %+v", theirs.Reactions)
	}

	send(a, b, tea.KeyPressMsg{Code: 'r', Text: "r"})
	if a.main.replyTo != theirs.ID || !strings.Contains(a.main.composer.Placeholder, "Reply to Ravi") {
		t.Fatalf("reply mode: %q, %q", a.main.replyTo, a.main.composer.Placeholder)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape}) // cancels the reply first
	if a.main.replyTo != "" || a.main.focus != focusComposer {
		t.Fatal("Esc didn't cancel the reply")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	runCommand(a, b, "delete")
	msgs = a.main.messages["ravi"]
	if !msgs[len(msgs)-1].Deleted || !strings.Contains(plain(a), "Message deleted") {
		t.Fatal("the message isn't deleted")
	}
}

func TestTypingShows(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.handleEvent(Typing{Chat: "ravi", Who: demoRavi, On: true}))
	if !strings.Contains(plain(a), "Ravi is typing…") {
		t.Fatal("the typing indicator isn't showing")
	}
	drive(a, b, a.handleEvent(Typing{Chat: "ravi", Who: demoRavi, On: false}))
	if strings.Contains(plain(a), "is typing") {
		t.Fatal("the typing indicator stayed")
	}
}

func TestReadReceiptQuestion(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "noor", false))
	if a.main.overlay != overlayReceipts || !strings.Contains(plain(a), "Send read receipts to Noor") {
		t.Fatalf("the question didn't show:\n%s", plain(a))
	}
	send(a, b, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if a.main.overlay != overlayNone {
		t.Fatal("the question stayed")
	}
	if c, _ := a.main.activeChat(); c.ReceiptAsk != nil || c.ReadReceipts != "on" {
		t.Fatalf("after answering: %+v", c)
	}
	// It's asked once.
	drive(a, b, a.main.openChat(a, "ravi", false))
	drive(a, b, a.main.openChat(a, "noor", false))
	if a.main.overlay != overlayNone {
		t.Fatal("the question came back")
	}
}

func TestReportShowsWhatGoes(t *testing.T) {
	a, b := started(t, 120, 40)
	drive(a, b, a.main.openChat(a, "ravi", false))
	runCommand(a, b, "report spam")
	if a.main.overlay != overlayReport {
		t.Fatal("the report box didn't open")
	}
	out := plain(a)
	if !strings.Contains(out, "Report this message from Ravi") || !strings.Contains(out, "Can you check that the trailhead is open?") || !strings.Contains(out, "Reason: spam") {
		t.Fatalf("the report box:\n%s", out)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayNone {
		t.Fatal("the box stayed")
	}
}
