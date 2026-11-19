package chat

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // image sizes
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// fileLabel is a short line for a file, for the chat list.
func fileLabel(r client.MediaRef) string {
	switch {
	case strings.HasPrefix(r.MIME, "image/"):
		return "Image"
	case strings.HasPrefix(r.MIME, "audio/"):
		return "Voice message"
	case strings.HasPrefix(r.MIME, "video/"):
		return "Video"
	}
	if r.Name != "" {
		return "File: " + r.Name
	}
	return "File"
}

// imageSize reads an image's size in pixels from its header, for PNG,
// JPEG, GIF, and WebP.
func imageSize(data []byte) (int, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// attachments turns a message's media-refs into attachments.
func attachments(raw []jsontext.Value) []Attachment {
	var out []Attachment
	for _, f := range raw {
		var r client.MediaRef
		if wire.Unmarshal(f, &r) == nil && r.Check() == nil {
			out = append(out, Attachment{Ref: r})
		}
	}
	return out
}

// detectMIME names a file's type from its first bytes, or its extension.
func detectMIME(name string, data []byte) string {
	m := http.DetectContentType(data)
	if strings.HasPrefix(m, "application/ogg") {
		return "audio/ogg" // Ogg Opus, as voice messages are
	}
	if m == "application/octet-stream" || strings.HasPrefix(m, "text/plain") {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".opus", ".ogg", ".oga":
			return "audio/ogg"
		case ".m4a":
			return "audio/mp4"
		case ".mp3":
			return "audio/mpeg"
		case ".pdf":
			return "application/pdf"
		case ".md":
			return "text/markdown"
		}
	}
	if i := strings.Index(m, ";"); i > 0 {
		m = m[:i]
	}
	return m
}

// ErrStorageFull reports a homeserver whose file storage for this
// identity is full (§14).
var ErrStorageFull = errors.New("chat: your homeserver's storage for files is full. Files you sent expire after 30 days, which makes room")

// SendFile encrypts a file, uploads it to the homeserver, and sends it to
// a chat as a file message (§14, §12). The message shows as sending
// while the upload runs.
func (e *Engine) SendFile(ctx context.Context, chat, name string, data []byte) (Message, error) {
	if len(data) == 0 {
		return Message{}, errors.New("chat: the file is empty")
	}
	if len(data) > client.MaxFile {
		return Message{}, client.ErrFileTooLarge
	}
	name = filepath.Base(name)
	mime := detectMIME(name, data)
	ref, err := e.c.UploadFile(ctx, data, name, mime)
	switch {
	case wire.IsCode(err, wire.CodeOverQuota):
		return Message{}, ErrStorageFull
	case wire.IsCode(err, wire.CodeTooLarge):
		return Message{}, client.ErrFileTooLarge
	case err != nil:
		return Message{}, err
	}
	att := Attachment{Ref: ref}
	if strings.HasPrefix(mime, "image/") {
		att.Width, att.Height = imageSize(data)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return Message{}, err
	}
	now := e.c.Clock().Now().Unix()
	m := Message{ID: newMsgID(), From: e.c.ID(), TS: now, Received: now, Files: []Attachment{att}, Status: Sending}
	if ch.Kind == KindChannel {
		if err := e.addChannelMessage(ch, m, true); err != nil {
			return Message{}, err
		}
	} else if err := e.addMessage(ch, m); err != nil {
		return Message{}, err
	}
	c := File{T: CFile, ID: m.ID, TS: now, Ref: &ref}
	if ch.Kind == KindChannel {
		var seq uint64
		seq, err = e.sendChannel(ctx, ch, c)
		if err == nil {
			m.Seq = seq
		}
	} else {
		err = e.sendContent(ctx, ch, c, false)
	}
	m.Status = Sent
	if err != nil {
		m.Status = Failed
	}
	e.changeMessage(ch, m.ID, func(x *Message) error { x.Status, x.Seq = m.Status, m.Seq; return nil })
	return m, err
}

// SaveFile downloads attachment i of a message, decrypts it, and saves it
// in dir under its name, or a name that doesn't overwrite another file.
// It returns the path. A file past its 30 days is client.ErrFileExpired
// (§14).
func (e *Engine) SaveFile(ctx context.Context, chat, msgID string, i int, dir string) (string, error) {
	e.mu.Lock()
	msgs, err := e.messages(chat)
	e.mu.Unlock()
	if err != nil {
		return "", err
	}
	k := findMessage(msgs, msgID)
	if k < 0 || i < 0 || i >= len(msgs[k].Files) {
		return "", errors.New("chat: no such file")
	}
	att := msgs[k].Files[i]
	data, err := e.c.DownloadFile(ctx, att.Ref)
	gone := ""
	switch {
	case errors.Is(err, client.ErrFileExpired):
		gone = "expired"
	case errors.Is(err, client.ErrFileRemoved):
		gone = "removed"
	case err != nil:
		return "", err
	}
	path := ""
	if gone == "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		path, err = freePath(dir, att.Ref.Name)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch := e.st.Chats[chat]; ch != nil {
		e.changeMessage(ch, msgID, func(m *Message) error {
			if i < len(m.Files) {
				f := &m.Files[i]
				f.Saved, f.Gone = path, gone
				if gone == "" && strings.HasPrefix(f.Ref.MIME, "image/") {
					f.Width, f.Height = imageSize(data)
				}
			}
			return nil
		})
	}
	if gone == "expired" {
		return "", client.ErrFileExpired
	}
	if gone == "removed" {
		return "", client.ErrFileRemoved
	}
	return path, nil
}

// freePath is dir/name, or dir/name with a number before the extension
// when that's taken. The name loses any directories, so a sender can't
// pick where a file goes.
func freePath(dir, name string) (string, error) {
	name = filepath.Base(filepath.Clean("/" + strings.ReplaceAll(name, "\\", "/")))
	if name == "/" || name == "." || name == "" || strings.HasPrefix(name, ".") {
		name = "file" + name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 0; n < 1000; n++ {
		p := filepath.Join(dir, name)
		if n > 0 {
			p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		}
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
	}
	return "", errors.New("chat: too many files with that name")
}
