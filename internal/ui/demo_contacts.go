package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// demoInviteCode is the code the demo hands out. It isn't a real code.
const demoInviteCode = "Kx82mPq1Za"

// changeMessage changes one of a chat's messages, and reports it. The
// caller holds d.mu.
func (d *DemoBackend) changeMessage(chat, id string, f func(*Message) error) error {
	for i := range d.messages[chat] {
		if d.messages[chat][i].ID == id {
			if err := f(&d.messages[chat][i]); err != nil {
				return err
			}
			d.emit(MessageChanged{Message: d.messages[chat][i]})
			return nil
		}
	}
	return fmt.Errorf("There's no message %q", id)
}

// Edit implements Backend.
func (d *DemoBackend) Edit(_ context.Context, chat, id, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changeMessage(chat, id, func(m *Message) error {
		if !m.Mine {
			return errors.New("You can only edit your own messages")
		}
		m.Text, m.Edited = text, true
		return nil
	})
}

// Delete implements Backend.
func (d *DemoBackend) Delete(_ context.Context, chat, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changeMessage(chat, id, func(m *Message) error {
		if !m.Mine {
			return errors.New("You can only delete your own messages")
		}
		m.Text, m.Deleted, m.Reactions = "", true, nil
		return nil
	})
}

// React implements Backend.
func (d *DemoBackend) React(_ context.Context, chat, id, emoji string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.changeMessage(chat, id, func(m *Message) error {
		m.Reactions = toggleReaction(m.Reactions, emoji)
		return nil
	})
}

// toggleReaction adds the identity's reaction with emoji, or takes it back.
func toggleReaction(rs []Reaction, emoji string) []Reaction {
	for i, r := range rs {
		if r.Emoji != emoji {
			continue
		}
		if r.Mine {
			rs[i].Count--
			rs[i].Mine = false
			if rs[i].Count == 0 {
				return append(rs[:i], rs[i+1:]...)
			}
			return rs
		}
		rs[i].Count++
		rs[i].Mine = true
		return rs
	}
	return append(rs, Reaction{Emoji: emoji, Count: 1, Mine: true})
}

// SetTyping implements Backend. Nobody in the demo is listening.
func (d *DemoBackend) SetTyping(context.Context, string, bool) error { return nil }

// MakeInvite implements Backend.
func (d *DemoBackend) MakeInvite(context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return "@" + d.handle() + "::" + demoInviteCode, nil
}

// AddContact implements Backend. With Live set, the request is accepted a
// moment later, and becomes a direct chat.
func (d *DemoBackend) AddContact(_ context.Context, code string) error {
	handle, inv, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(code), "@"), "::")
	if !ok || !strings.HasPrefix(strings.TrimSpace(code), "@") || len(inv) != 10 || checkHandle(handle) != nil {
		return errors.New("That isn't a contact code. It looks like @ravi.example.com::Kx82mPq1Za")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := "request:" + handle
	c := &Chat{ID: id, Kind: SentRequest, Title: "@" + handle, Members: []Member{{Handle: handle}},
		Preview: "Request sent", LastActivity: d.opts.Now()}
	d.chats[id] = c
	d.emit(ChatChanged{Chat: *c})
	if d.opts.Live {
		d.later(3*time.Second, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			delete(d.chats, id)
			d.emit(ChatRemoved{Chat: id})
			chat := &Chat{ID: "direct:" + handle, Kind: DirectChat, Title: "@" + handle, Members: []Member{{Handle: handle}},
				Preview: "", LastActivity: d.opts.Now()}
			d.chats[chat.ID] = chat
			d.emit(ChatChanged{Chat: *chat})
		})
	}
	return nil
}

// AcceptRequest implements Backend.
func (d *DemoBackend) AcceptRequest(_ context.Context, chat string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.chats[chat]
	if !ok || c.Kind != ContactRequest {
		return fmt.Errorf("There's no request %q", chat)
	}
	delete(d.chats, chat)
	d.emit(ChatRemoved{Chat: chat})
	n := &Chat{ID: "direct:" + c.Members[0].Handle, Kind: DirectChat, Title: c.Title, Members: c.Members, LastActivity: d.opts.Now()}
	d.chats[n.ID] = n
	d.emit(ChatChanged{Chat: *n})
	return nil
}

// DeclineRequest implements Backend.
func (d *DemoBackend) DeclineRequest(_ context.Context, chat string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.chats[chat]; !ok || c.Kind != ContactRequest {
		return fmt.Errorf("There's no request %q", chat)
	}
	delete(d.chats, chat)
	d.emit(ChatRemoved{Chat: chat})
	return nil
}
