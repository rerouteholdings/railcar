package app

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// actContacts runs the actions that remove, block, and unblock people
// (§9.6). Each takes a handle, or else means the person in the open
// direct chat or contact request.
func actContacts(ctx context.Context, eng *chat.Engine, chatID string, a ui.Action) (string, bool, error) {
	switch a.Kind {
	case "remove-contact", "block", "unblock", "blocked":
	default:
		return "", false, nil
	}
	if a.Kind == "blocked" {
		var hs []string
		for _, c := range eng.Contacts() {
			if c.State == chat.StateBlocked {
				hs = append(hs, "@"+c.Handle)
			}
		}
		if len(hs) == 0 {
			return "You haven't blocked anyone", true, nil
		}
		slices.Sort(hs)
		return "Blocked: " + strings.Join(hs, ", "), true, nil
	}
	id, err := personOf(eng, chatID, a.Arg)
	if err != nil {
		return "", true, err
	}
	who := "@" + eng.Handle(id)
	switch a.Kind {
	case "remove-contact":
		return "You removed " + who + " as a contact", true, eng.RemoveContact(ctx, id)
	case "block":
		return "You blocked " + who + ". They aren't told", true, eng.Block(ctx, id)
	default:
		return "You unblocked " + who, true, eng.Unblock(ctx, id)
	}
}

// personOf is the identity a handle names, or else the other person in the
// open direct chat or contact request.
func personOf(eng *chat.Engine, chatID, handle string) (string, error) {
	if strings.TrimSpace(handle) != "" {
		return idOf(eng, handle)
	}
	if id, ok := strings.CutPrefix(chatID, requestPrefix); ok {
		return id, nil
	}
	if c, ok := eng.Chat(chatID); ok && c.Kind == chat.KindDirect {
		return c.Peer, nil
	}
	return "", errors.New("name someone, as @handle, or open your direct chat with them")
}
