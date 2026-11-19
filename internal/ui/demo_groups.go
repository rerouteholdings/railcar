package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Contacts implements Backend: the demo's direct chats.
func (d *DemoBackend) Contacts(context.Context) ([]Member, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Member
	for _, c := range d.chats {
		if c.Kind == DirectChat && len(c.Members) == 1 {
			out = append(out, c.Members[0])
		}
	}
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(a.Handle, b.Handle) })
	return out, nil
}

// CreateGroup implements Backend.
func (d *DemoBackend) CreateGroup(_ context.Context, name string, handles []string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(handles) == 0 {
		return "", errors.New("A group needs a name and at least one other person")
	}
	var members []Member
	for _, h := range handles {
		members = append(members, d.demoMember(h))
	}
	d.nextID++
	id := fmt.Sprintf("group%d", d.nextID)
	c := &Chat{ID: id, Kind: GroupChat, Title: name, Members: members, Admin: true, LastActivity: d.opts.Now(), Preview: "You created the group"}
	d.chats[id] = c
	d.systemLine(id, "You created the group")
	d.emit(ChatChanged{*c})
	return id, nil
}

func (d *DemoBackend) demoMember(handle string) Member {
	h := strings.TrimPrefix(strings.TrimSpace(handle), "@")
	for _, c := range d.chats {
		for _, m := range c.Members {
			if m.Handle == h {
				m.Admin, m.Roles = false, ""
				return m
			}
		}
	}
	return Member{Handle: h}
}

// systemLine adds a centered line to a demo chat. The caller holds d.mu.
func (d *DemoBackend) systemLine(chat, text string) {
	d.nextID++
	m := Message{ID: fmt.Sprintf("m%d", d.nextID), Chat: chat, Sent: d.opts.Now(), Text: text, System: true}
	d.messages[chat] = append(d.messages[chat], m)
	d.emit(MessageAdded{m})
}

// Act implements Backend. The demo changes its own data and sends
// nothing.
func (d *DemoBackend) Act(_ context.Context, chat string, a Action) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.chats[chat]
	switch a.Kind {
	case "create-community", "join-community":
		return "", errors.New("The demo has one community. Communities need a community server")
	}
	if c == nil {
		return "", errors.New("Open a chat first")
	}
	line := ""
	switch a.Kind {
	case "add-member":
		c.Members = append(c.Members, d.demoMember(a.Arg))
		line = "You added @" + strings.TrimPrefix(a.Arg, "@")
	case "remove-member", "kick", "ban":
		h := strings.TrimPrefix(a.Arg, "@")
		c.Members = slices.DeleteFunc(c.Members, func(m Member) bool { return m.Handle == h })
		line = "You removed @" + h
	case "make-admin", "revoke-admin":
		h := strings.TrimPrefix(a.Arg, "@")
		for i := range c.Members {
			if c.Members[i].Handle == h {
				c.Members[i].Admin = a.Kind == "make-admin"
			}
		}
		line = "@" + h + " is an admin now"
		if a.Kind == "revoke-admin" {
			line = "@" + h + " is no longer an admin"
		}
	case "rename":
		c.Title = a.Arg
		line = "You renamed the group to " + a.Arg
	case "topic":
		c.Topic = a.Arg
		line = "You changed the topic"
	case "leave", "leave-community":
		c.Stopped = "You left"
		line = "You left"
	case "make-code":
		return "community.example.com::Kx82mPq1ZaR4vT9b", nil
	case "history":
		return "Read 0 messages from history", nil
	default:
		return "Done", nil
	}
	d.systemLine(chat, line)
	d.emit(ChatChanged{*c})
	return line, nil
}
