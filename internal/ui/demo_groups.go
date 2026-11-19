package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	if line, ok := d.actPeople(a); ok {
		return line, nil
	}
	if c != nil && (a.Kind == "attach" || a.Kind == "save") {
		return d.actFiles(c, a)
	}
	switch a.Kind {
	case "ask-receipts":
		return "Your messages ask for read receipts now", nil
	case "report":
		return "Reported. The demo sends nothing", nil
	case "backup-on", "backup-now", "backup-off", "restore-backup", "backup-status":
		return "The demo has no backup", nil
	case "read-receipts":
		if c == nil {
			return "", errors.New("Open a chat first")
		}
		c.ReceiptAsk, c.ReadReceipts = nil, a.Arg
		d.emit(ChatChanged{*c})
		return "Done", nil
	}
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

// actPeople runs the profile and presence actions in the demo (§13), and
// reports whether a.Kind was one of them. The caller holds d.mu.
func (d *DemoBackend) actPeople(a Action) (string, bool) {
	id := d.identity
	if id == nil {
		return "", false
	}
	changed := func(line string) (string, bool) {
		d.emit(IdentityChanged{Identity: *id})
		return line, true
	}
	switch a.Kind {
	case "set-presence":
		id.Presence, id.Away = strings.ToLower(strings.TrimSpace(a.Arg)), false
		if s := strings.TrimSpace(a.Arg2); s != "" {
			id.Status = s
		}
		return changed("You're " + id.Presence + " to your contacts")
	case "set-status":
		id.Status = strings.TrimSpace(a.Arg)
		return changed("Your contacts see your status message")
	case "auto-away":
		id.Away = a.Arg == "on" && (id.Presence == "online" || id.Presence == "available")
		return changed("")
	case "set-name":
		id.DisplayName = strings.TrimSpace(a.Arg)
		return changed("Anyone can see your display name now")
	case "set-bio", "set-links", "set-avatar":
		return "The demo doesn't publish profiles", true
	case "show-profile":
		return "Name: " + id.DisplayName + ", presence: " + id.Presence, true
	}
	return "", false
}

// actFiles sends and saves files in the demo (§14). Nothing is read or
// written: a sent file is named after the path, and saving only marks it.
// The caller holds d.mu.
func (d *DemoBackend) actFiles(c *Chat, a Action) (string, error) {
	if a.Kind == "attach" {
		name := filepath.Base(strings.TrimSpace(a.Arg))
		if name == "." || name == "" {
			return "", errors.New("Name a file, as :attach ~/Pictures/map.png")
		}
		d.nextID++
		m := Message{
			ID: fmt.Sprintf("m%d", d.nextID), Chat: c.ID, Sender: Member{Name: d.displayName(), Handle: d.handle()},
			Mine: true, Sent: d.opts.Now(), Files: []Attachment{{Name: name, MIME: "application/octet-stream", Size: 52000}},
		}
		d.messages[c.ID] = append(d.messages[c.ID], m)
		c.LastActivity, c.Preview = m.Sent, "File: "+name
		d.emit(MessageAdded{Message: m})
		d.emit(ChatChanged{Chat: *c})
		return "Sent " + name + " (the demo sends nothing)", nil
	}
	msgs := d.messages[c.ID]
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].Files) > 0 {
			dir := strings.TrimSpace(a.Arg)
			if dir == "" {
				dir = "~/Downloads"
			}
			path := filepath.Join(dir, msgs[i].Files[0].Name)
			msgs[i].Files[0].Saved = path
			d.emit(MessageChanged{Message: msgs[i]})
			return "Saved to " + path + " (the demo writes nothing)", nil
		}
	}
	return "", errors.New("There's no file in this chat to save")
}
