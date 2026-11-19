package chat

import (
	"context"
	"errors"
	"os"

	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Group pictures (§12): a group-meta carries the picture inline, at most
// 256 KiB of PNG, JPEG, WebP, or GIF, never as a media-ref, since files
// expire (§14).

// recGroupPicture is followed by the chat's ID in hex.
const recGroupPicture = "gpic."

var pictureTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// ErrBadGroupPicture is what SetGroupPicture returns for a file that isn't
// a PNG, JPEG, WebP, or GIF of at most 256 KiB.
var ErrBadGroupPicture = errors.New("chat: a group's picture is a PNG, JPEG, WebP, or GIF of at most 256 KiB")

func checkGroupPicture(p GroupPicture) error {
	if !pictureTypes[p.MIME] || len(p.Data) == 0 || len(p.Data) > MaxGroupPicture {
		return ErrBadGroupPicture
	}
	if detectMIME("", p.Data) != p.MIME {
		return ErrBadGroupPicture
	}
	return nil
}

// SetGroupPicture sets a group chat's picture, or with nil data removes
// it, and tells the members (§12).
func (e *Engine) SetGroupPicture(ctx context.Context, chat string, data []byte) error {
	var pic *GroupPicture
	if data != nil {
		pic = &GroupPicture{MIME: detectMIME("", data), Data: data}
		if err := checkGroupPicture(*pic); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.groupChat(chat)
	if err != nil {
		return err
	}
	if err := e.keepGroupPicture(ch, pic); err != nil {
		return err
	}
	ch.PictureRemoved = pic == nil
	err = e.sendMeta(ctx, ch)
	ch.PictureRemoved = false
	if err != nil {
		return err
	}
	if pic == nil {
		e.systemLine(ch, "You removed the group's picture")
	} else {
		e.systemLine(ch, "You changed the group's picture")
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return e.save()
}

// GroupPicture returns a group chat's picture, if it has one.
func (e *Engine) GroupPicture(chat string) (GroupPicture, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.groupPicture(chat)
}

func (e *Engine) groupPicture(chat string) (GroupPicture, bool) {
	var p GroupPicture
	if err := e.profile().Load(recGroupPicture+recordID(chat), &p); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.log.Info("chat: a group's picture", "err", err)
		}
		return p, false
	}
	return p, true
}

// keepGroupPicture stores a group's picture, or deletes it with nil. The
// caller holds e.mu.
func (e *Engine) keepGroupPicture(ch *Chat, p *GroupPicture) error {
	ch.Picture = p != nil
	if p == nil {
		return e.profile().Delete(recGroupPicture + recordID(ch.ID))
	}
	return e.profile().Save(recGroupPicture+recordID(ch.ID), p)
}

// metaPicture is the avatar member of a group-meta for this chat: the
// picture, or nothing when it has none.
func (e *Engine) metaPicture(ch *Chat) []byte {
	p, ok := e.groupPicture(ch.ID)
	if !ok {
		return nil
	}
	j, err := wire.Marshal(p)
	if err != nil {
		return nil
	}
	return j
}

// SaveGroupPicture saves a group chat's picture in dir, under a name that
// doesn't overwrite another file, and returns the path.
func (e *Engine) SaveGroupPicture(chat, dir string) (string, error) {
	p, ok := e.GroupPicture(chat)
	if !ok {
		return "", errors.New("chat: this group has no picture")
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[p.MIME]
	path, err := freePath(dir, "group-picture"+ext)
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, p.Data, 0o600)
}
