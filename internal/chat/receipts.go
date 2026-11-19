package chat

import (
	"context"
	"errors"
)

// Read receipts (§13.3): the sender asks, with receipt on a text, and the
// reader decides. Railcar never sends a receipt for a message that didn't
// ask, or before the user agreed for that chat. The first time someone
// asks in a chat, the interface asks the user once, and the answer is
// kept per chat.

// ReceiptQuestion returns who asked for a read receipt in a chat whose
// user hasn't decided yet, or "" when there's nothing to ask.
func (e *Engine) ReceiptQuestion(chat string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.st.Chats[chat]
	if !ok || ch.ReadReceipts != nil {
		return ""
	}
	return ch.ReceiptAsker
}

// SetReadReceipts records whether the user agrees to send read receipts in
// a chat, and when they do, sends one for what they've read.
func (e *Engine) SetReadReceipts(ctx context.Context, chat string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.st.Chats[chat]
	if !ok {
		return errors.New("chat: no such chat")
	}
	if ch.Kind == KindChannel {
		return errors.New("chat: channels don't have read receipts")
	}
	ch.ReadReceipts, ch.ReceiptAsker = &on, ""
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
	if on && ch.Unread == 0 {
		return e.sendReceipt(ctx, ch)
	}
	return nil
}

// SetAskReceipts turns asking for read receipts on this identity's
// messages on or off. It's off by default (§13.3).
func (e *Engine) SetAskReceipts(on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.AskReceipts = on
	return e.save()
}

// AskReceipts reports whether this identity's messages ask for read
// receipts.
func (e *Engine) AskReceipts() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.st.AskReceipts
}

// sendReceipt sends a receipt up to the newest message from someone else
// that asked for one, if there's a newer one than the last receipt, as an
// ephemeral delivery (§13.3). The caller holds e.mu, and checked that the
// user agreed.
func (e *Engine) sendReceipt(ctx context.Context, ch *Chat) error {
	msgs, err := e.messages(ch.ID)
	if err != nil {
		return err
	}
	upto := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].ID == ch.ReceiptUpto {
			break
		}
		if msgs[i].AskedReceipt && msgs[i].From != e.c.ID() && !msgs[i].Deleted {
			upto = msgs[i].ID
			break
		}
	}
	if upto == "" {
		return nil
	}
	if _, err := e.sendable(ch.ID); err != nil {
		return err
	}
	if err := e.sendContent(ctx, ch, Receipt{T: CReceipt, TS: e.c.Clock().Now().Unix(), Upto: upto}, true); err != nil {
		return err
	}
	ch.ReceiptUpto = upto
	return e.save()
}
