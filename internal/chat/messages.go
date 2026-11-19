package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// processApp decrypts an application message, checks it (§12), and
// applies its content. The caller holds e.mu.
func (e *Engine) processApp(ctx context.Context, ch *Chat, b buffered) error {
	g := e.dev.mls.Group(mustB64(ch.ID))
	p, err := g.Process(b.Data)
	if err != nil {
		e.dev.Revert()
		return fmt.Errorf("chat: decrypting a message: %w", err)
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	if p.Kind == mls.KindOwn {
		return nil
	}
	if p.Kind != mls.KindApplication {
		return fmt.Errorf("chat: an app delivery holding a %s", p.Kind)
	}
	st, err := g.State()
	if err != nil {
		return err
	}
	leaf, ok := st.Member(p.Sender.Index)
	if !ok || !bytes.Equal(leaf.Identity, p.Identity) {
		// The sender's leaf changed since the message's epoch. Checking
		// against past epochs' leaves comes with group chats.
		return errors.New("chat: the sender's leaf is gone")
	}
	// The checks on every message (§12): the signature, the message key
	// against the sender's msg-key and chain, and the identity and group.
	id, _, err := e.checkLeaf(&leaf.LeafInfo, e.chainGetter(ctx, false), leafCurrent)
	if err != nil {
		return err
	}
	am, t, err := openAppMessage(p.Plaintext, leaf.SignatureKey)
	if err != nil {
		return err
	}
	if am.ID != id || !bytes.Equal(am.Group, mustB64(ch.ID)) {
		return errors.New("chat: an app-message whose id or group doesn't match")
	}
	if am.Channel != nil {
		return errors.New("chat: a channel outside a community")
	}
	if b.From != "" && b.From != id {
		return errors.New("chat: an app-message delivered by someone else")
	}
	return e.applyContent(ch, id, am, t, b.Received)
}

// applyContent applies one message's content to the chat's history.
func (e *Engine) applyContent(ch *Chat, from string, am AppMessage, t string, received int64) error {
	c, err := parseContent(t, am.C)
	if err != nil {
		return err
	}
	if !knownContent[t] {
		// Unknown content is shown as a placeholder, not dropped (§12).
		var u struct {
			ID string `json:"id"`
		}
		wire.Unmarshal(am.C, &u)
		if !validMsgID(u.ID) {
			u.ID = newMsgID()
		}
		return e.addMessage(ch, Message{ID: u.ID, From: from, TS: am.TS, Received: received, Unsupported: t, Status: Sent})
	}
	switch c := c.(type) {
	case Text:
		return e.addMessage(ch, Message{ID: c.ID, From: from, TS: am.TS, Received: received, Body: *c.Body, Reply: c.Reply, AskedReceipt: c.Receipt, Status: Sent})
	case Edit:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			if m.From != from {
				return errors.New("chat: an edit from someone other than the author") // §12
			}
			m.Body, m.Edited = *c.Body, true
			return nil
		})
	case Delete:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			if m.From != from {
				return errors.New("chat: a delete from someone other than the author") // §12
			}
			m.Body, m.Deleted, m.Reactions = "", true, nil
			return nil
		})
	case React:
		return e.changeMessage(ch, c.Target, func(m *Message) error {
			react(m, from, c.Emoji, c.Remove)
			return nil
		})
	case Receipt:
		return e.receipt(ch, from, c.Upto)
	case Typing:
		e.emit(Event{Kind: EventTyping, ChatID: ch.ID, Who: from, On: *c.On})
		return nil
	}
	// Known content this version doesn't act on, such as files and calls,
	// shows as a placeholder when it's a message of its own.
	var withID struct {
		ID string `json:"id"`
	}
	wire.Unmarshal(am.C, &withID)
	if t == CFile && validMsgID(withID.ID) {
		return e.addMessage(ch, Message{ID: withID.ID, From: from, TS: am.TS, Received: received, Unsupported: t, Status: Sent})
	}
	return nil
}

func react(m *Message, from, emoji string, remove bool) {
	if m.Reactions == nil {
		m.Reactions = map[string][]string{}
	}
	who := slices.DeleteFunc(m.Reactions[emoji], func(s string) bool { return s == from })
	if !remove {
		who = append(who, from)
	}
	if len(who) == 0 {
		delete(m.Reactions, emoji)
	} else {
		m.Reactions[emoji] = who
	}
}

// addMessage adds a message to a chat's history, and counts it as unread
// when someone else sent it.
func (e *Engine) addMessage(ch *Chat, m Message) error {
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	if findMessage(msgs, m.ID) >= 0 {
		return nil
	}
	if m.Received == 0 {
		m.Received = e.c.Clock().Now().Unix()
	}
	msgs = append(msgs, m)
	if err := saveMessages(e.profile(), recordID(ch.ID), msgs); err != nil {
		return err
	}
	ch.LastActivity, ch.Preview = max(ch.LastActivity, m.Received), preview(m)
	if m.From != e.c.ID() && !m.System {
		ch.Unread++
	}
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventMessage, ChatID: ch.ID, Message: &m, New: true})
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	return nil
}

// changeMessage changes a message in a chat's history.
func (e *Engine) changeMessage(ch *Chat, id string, f func(*Message) error) error {
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	i := findMessage(msgs, id)
	if i < 0 {
		return fmt.Errorf("chat: a change to a message this device doesn't have")
	}
	if err := f(&msgs[i]); err != nil {
		return err
	}
	if err := saveMessages(e.profile(), recordID(ch.ID), msgs); err != nil {
		return err
	}
	if i == len(msgs)-1 {
		ch.Preview = preview(msgs[i])
		if err := e.save(); err != nil {
			return err
		}
		e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	}
	m := msgs[i]
	e.emit(Event{Kind: EventMessage, ChatID: ch.ID, Message: &m})
	return nil
}

// receipt marks every message up to upto that asked for a receipt as read
// by from (§13.3).
func (e *Engine) receipt(ch *Chat, from, upto string) error {
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	end := findMessage(msgs, upto)
	if end < 0 {
		return nil
	}
	var changed []int
	for i := 0; i <= end; i++ {
		if msgs[i].From == e.c.ID() && msgs[i].AskedReceipt && !slices.Contains(msgs[i].ReadBy, from) {
			msgs[i].ReadBy = append(msgs[i].ReadBy, from)
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := saveMessages(e.profile(), recordID(ch.ID), msgs); err != nil {
		return err
	}
	for _, i := range changed {
		m := msgs[i]
		e.emit(Event{Kind: EventMessage, ChatID: ch.ID, Message: &m})
	}
	return nil
}

func newMsgID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return wire.EncodeB64(b)
}

// MaxBody bounds a message's text, so it fits in one delivery with room
// for its signature and encryption.
const MaxBody = 64 << 10

// SendText sends a text message, optionally a reply (§12).
func (e *Engine) SendText(ctx context.Context, chat, body, reply string) (Message, error) {
	body = strings.TrimRight(body, " \n")
	if body == "" {
		return Message{}, errors.New("chat: an empty message")
	}
	if len(body) > MaxBody || !utf8.ValidString(body) {
		return Message{}, errors.New("chat: the message is too long")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return Message{}, err
	}
	now := e.c.Clock().Now().Unix()
	m := Message{ID: newMsgID(), From: e.c.ID(), TS: now, Received: now, Body: body, Reply: reply, Status: Sending}
	if err := e.addMessage(ch, m); err != nil {
		return Message{}, err
	}
	c := Text{T: CText, ID: m.ID, TS: now, Body: &body, Reply: reply}
	err = e.sendContent(ctx, ch, c, false)
	m.Status = Sent
	if err != nil {
		m.Status = Failed
	}
	e.changeMessage(ch, m.ID, func(x *Message) error { x.Status = m.Status; return nil })
	return m, err
}

// Edit replaces the text of one of this identity's messages (§12).
func (e *Engine) Edit(ctx context.Context, chat, target, body string) error {
	if body == "" || len(body) > MaxBody {
		return errors.New("chat: an edit needs text")
	}
	return e.changeOwn(ctx, chat, target, func(now int64) any {
		return Edit{T: CEdit, ID: newMsgID(), TS: now, Target: target, Body: &body}
	}, func(m *Message) { m.Body, m.Edited = body, true })
}

// Delete deletes one of this identity's messages for everyone (§12).
func (e *Engine) Delete(ctx context.Context, chat, target string) error {
	return e.changeOwn(ctx, chat, target, func(now int64) any {
		return Delete{T: CDelete, ID: newMsgID(), TS: now, Target: target}
	}, func(m *Message) { m.Body, m.Deleted, m.Reactions = "", true, nil })
}

func (e *Engine) changeOwn(ctx context.Context, chat, target string, content func(int64) any, apply func(*Message)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return err
	}
	msgs, err := e.messages(chat)
	if err != nil {
		return err
	}
	i := findMessage(msgs, target)
	if i < 0 || msgs[i].From != e.c.ID() || msgs[i].Deleted {
		return errors.New("chat: you can only change your own messages")
	}
	if err := e.sendContent(ctx, ch, content(e.c.Clock().Now().Unix()), false); err != nil {
		return err
	}
	return e.changeMessage(ch, target, func(m *Message) error { apply(m); return nil })
}

// React adds a reaction to a message, or takes it back (§12).
func (e *Engine) React(ctx context.Context, chat, target, emoji string, remove bool) error {
	if emoji == "" || len(emoji) > 64 {
		return errors.New("chat: a reaction is one emoji")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return err
	}
	msgs, err := e.messages(chat)
	if err != nil {
		return err
	}
	if findMessage(msgs, target) < 0 {
		return errors.New("chat: no such message")
	}
	c := React{T: CReact, ID: newMsgID(), TS: e.c.Clock().Now().Unix(), Target: target, Emoji: emoji, Remove: remove}
	if err := e.sendContent(ctx, ch, c, false); err != nil {
		return err
	}
	me := e.c.ID()
	return e.changeMessage(ch, target, func(m *Message) error { react(m, me, emoji, remove); return nil })
}

// SetTyping sends a typing indicator, as an ephemeral delivery (§13.3).
func (e *Engine) SetTyping(ctx context.Context, chat string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, err := e.sendable(chat)
	if err != nil {
		return err
	}
	return e.sendContent(ctx, ch, Typing{T: CTyping, TS: e.c.Clock().Now().Unix(), On: &on}, true)
}

// sendable returns a chat this device may send to now.
func (e *Engine) sendable(chat string) (*Chat, error) {
	ch, ok := e.st.Chats[chat]
	switch {
	case !ok:
		return nil, errors.New("chat: no such chat")
	case ch.Stopped != "":
		return nil, fmt.Errorf("chat: this chat is stopped: %s", ch.Stopped)
	case ch.Pending:
		// Nothing goes out while a commit waits for a verdict (§7.1).
		return nil, errors.New("chat: this chat is waiting on a change it can't check yet")
	case ch.Hold:
		// A removed device still has a leaf here (§11.9).
		return nil, errors.New("chat: this chat is waiting for a removed device to be taken out")
	}
	return ch, nil
}

// sendContent signs content as an app-message, encrypts it in the chat's
// group, and sends it to every member (§10.2, §12). The caller holds
// e.mu.
func (e *Engine) sendContent(ctx context.Context, ch *Chat, content any, ephemeral bool) error {
	gid := mustB64(ch.ID)
	plain, err := signAppMessage(e.key, e.c.ID(), gid, e.c.Clock().Now().Unix(), content)
	if err != nil {
		return err
	}
	g := e.dev.mls.Group(gid)
	ct, err := g.Encrypt(e.dev.signer, plain, nil)
	if err != nil {
		return err
	}
	// The ratchet moved: save before the message leaves, so a key is
	// never used twice.
	if err := e.dev.Save(); err != nil {
		return err
	}
	epoch := ch.Epoch
	res, err := e.c.Send(ctx, client.Outgoing{
		ID: client.NewMsgID(), Kind: "mls", To: ch.Members, Group: ch.ID, Epoch: &epoch, MLS: "app",
		Ephemeral: ephemeral, Data: ct,
	})
	if err != nil {
		return err
	}
	if res[0].Status != "sent" {
		return &wire.Error{Code: res[0].Error, Msg: "the homeserver refused the message"}
	}
	return nil
}

// deliveryFailed handles a delivery-failed notice (§10.5). Deliveries
// aren't tied to messages yet, so it's logged.
func (e *Engine) deliveryFailed(n *client.Notice) {
	e.log.Info("chat: a delivery failed", "reason", n.Reason, "recipients", len(n.To))
}
