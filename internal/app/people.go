package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// member is someone as the interface shows them: their handle, and their
// display name, presence, and profile as this device knows them (§13).
func member(eng *chat.Engine, id string) ui.Member {
	m := ui.Member{Handle: eng.Handle(id)}
	if p, ok := eng.ProfileOf(id); ok {
		m.Name, m.Bio, m.Links, m.Picture = p.Name, p.Bio, p.Links, p.Avatar != nil
	}
	if p, ok := eng.Presence(id); ok {
		m.Presence, m.Status = p.State, p.Status
	}
	return m
}

// identityOf is the signed-in identity as the interface shows it.
func identityOf(eng *chat.Engine, c *client.Client) ui.Identity {
	id := ui.Identity{Handle: c.Handle(), Homeserver: c.Server()}
	if d, ok := c.Chain().State.Device(c.DeviceKey()); ok {
		id.Device = d.Name
	}
	if eng != nil {
		if p, ok := eng.ProfileOf(c.ID()); ok {
			id.DisplayName = p.Name
		}
		own := eng.OwnPresence()
		id.Presence, id.Status, id.Away = own.State, own.Status, eng.Away()
	}
	return id
}

// presenceEvent shows a change of someone's presence or profile: in every
// chat they're in, or in the status bar when it's this identity's.
func (b *Backend) presenceEvent(eng *chat.Engine, who string) {
	c := eng.Client()
	if who == c.ID() {
		b.emit(ui.IdentityChanged{Identity: identityOf(eng, c)})
		return
	}
	for _, ch := range eng.Chats() {
		for _, m := range ch.Members {
			if m == who {
				b.emit(ui.ChatChanged{Chat: b.uiChat(eng, ch)})
				break
			}
		}
	}
	for _, ct := range eng.Contacts() {
		if ct.ID == who {
			if rc, ok := requestChat(ct); ok {
				b.emit(ui.ChatChanged{Chat: rc})
			}
		}
	}
}

// actPeople runs the profile and presence actions (§13), and reports
// whether a.Kind was one of them.
func actPeople(ctx context.Context, eng *chat.Engine, a ui.Action) (string, bool, error) {
	switch a.Kind {
	case "set-presence":
		state := strings.ToLower(strings.TrimSpace(a.Arg))
		if !client.ValidPresence(state) {
			return "", true, errors.New("presence is available, online, away, busy, or offline")
		}
		status := strings.TrimSpace(a.Arg2)
		if status == "" && state != client.PresenceOffline {
			status = eng.OwnPresence().Status
		}
		if err := eng.SetPresence(ctx, state, status); err != nil {
			return "", true, err
		}
		if state == client.PresenceOffline {
			return "You appear offline to your contacts", true, nil
		}
		return "You're " + state + " to your contacts", true, nil
	case "set-status":
		own := eng.OwnPresence()
		status := strings.TrimSpace(a.Arg)
		if err := eng.SetPresence(ctx, own.State, status); err != nil {
			return "", true, err
		}
		if status == "" {
			return "Status message cleared", true, nil
		}
		return "Your contacts see your status message", true, nil
	case "auto-away":
		return "", true, eng.AutoAway(ctx, a.Arg == "on")
	case "set-name":
		name := strings.TrimSpace(a.Arg)
		_, err := eng.EditProfile(ctx, chat.ProfileEdit{Name: &name})
		return "Anyone can see your display name now", true, err
	case "set-bio":
		bio := strings.TrimSpace(a.Arg)
		_, err := eng.EditProfile(ctx, chat.ProfileEdit{Bio: &bio})
		return "Your bio is on your profile", true, err
	case "set-links":
		links := strings.Fields(a.Arg)
		if links == nil {
			links = []string{}
		}
		_, err := eng.EditProfile(ctx, chat.ProfileEdit{Links: links})
		return fmt.Sprintf("Your profile has %d links", len(links)), true, err
	case "set-avatar":
		path := strings.TrimSpace(a.Arg)
		if path == "" || strings.EqualFold(path, "none") {
			_, err := eng.EditProfile(ctx, chat.ProfileEdit{NoAvatar: true})
			return "Removed your profile picture", true, err
		}
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			return "", true, fmt.Errorf("Railcar can't read %s", path)
		}
		mime := http.DetectContentType(data)
		m, err := eng.Client().UploadPublic(ctx, data, mime)
		if err != nil {
			return "", true, err
		}
		_, err = eng.EditProfile(ctx, chat.ProfileEdit{Avatar: &m})
		return "Anyone can see your profile picture now", true, err
	case "show-profile":
		p, err := eng.RefreshProfile(ctx, eng.Client().ID())
		if errors.Is(err, client.ErrNoProfile) {
			return "You haven't published a profile. Use :name, :bio, :links, and :avatar", true, nil
		}
		if err != nil {
			return "", true, err
		}
		parts := []string{"Name: " + orNone(p.Name), "bio: " + orNone(p.Bio)}
		if len(p.Links) > 0 {
			parts = append(parts, "links: "+strings.Join(p.Links, " "))
		}
		if p.Avatar != nil {
			parts = append(parts, "with a picture")
		}
		own := eng.OwnPresence()
		parts = append(parts, "presence: "+own.State)
		if own.Status != "" {
			parts = append(parts, "status: "+own.Status)
		}
		return strings.Join(parts, ", "), true, nil
	}
	return "", false, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// expandHome turns a leading ~/ into the home directory.
func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return h + "/" + rest
		}
	}
	return p
}
