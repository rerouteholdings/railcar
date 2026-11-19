package chat

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Reports (§17.2, §17.3): a report carries each message as its sender
// signed it, with the msg-key that certifies the key, so whoever reads
// the report can check that the sender sent exactly that.

// MaxReason bounds a report's reason, in bytes.
const MaxReason = 1000

// Report reports messages from one person in a chat. A direct chat's or
// group chat's report goes to the homeserver's admins, and with forward
// to the reported identity's homeserver too; a channel's goes to the
// community's moderators (§17.3). Only messages this device has signed
// copies of can be reported.
func (e *Engine) Report(ctx context.Context, chat string, ids []string, reason string, forward bool) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > MaxReason {
		return errors.New("chat: a report needs a reason of at most 1,000 bytes")
	}
	if len(ids) == 0 {
		return errors.New("chat: name a message to report")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.st.Chats[chat]
	if !ok {
		return errors.New("chat: no such chat")
	}
	r := client.Report{Reason: reason}
	var at []client.ReportAt
	add := func(msg, mk wire.Signed) error {
		var am AppMessage
		if err := msg.Body(&am); err != nil {
			return err
		}
		if am.ID == e.c.ID() {
			return errors.New("chat: you can't report your own message")
		}
		if r.Reported != "" && r.Reported != am.ID {
			return errors.New("chat: a report covers one person's messages")
		}
		r.Reported = am.ID
		r.Evidence = append(r.Evidence, client.Evidence{Msg: msg, MK: mk})
		return nil
	}
	if ch.Kind == KindChannel {
		cm, _, err := e.communityOf(ch.Community)
		if err != nil {
			return err
		}
		msgs, err := e.messages(ch.ID)
		if err != nil {
			return err
		}
		log := e.loadLog(cm, ch.Channel)
		for _, id := range ids {
			i := findMessage(msgs, id)
			if i < 0 || msgs[i].Seq == 0 {
				return errors.New("chat: no such message")
			}
			j := slices.IndexFunc(log, func(le logEntry) bool { return le.Seq == msgs[i].Seq && le.Msg != nil && le.MsgKey != nil })
			if j < 0 {
				return errors.New("chat: this device doesn't have that message as its sender signed it")
			}
			if err := add(*log[j].Msg, *log[j].MsgKey); err != nil {
				return err
			}
			at = append(at, client.ReportAt{Channel: ch.Channel, Seq: msgs[i].Seq})
		}
		return e.c.Community(cm.Server).Report(ctx, r, at)
	}
	log := e.histLog(ch.ID)
	keys := e.msgKeys()
	for _, id := range ids {
		found := false
		for _, h := range log {
			var am AppMessage
			var c struct {
				ID string `json:"id"`
			}
			if h.Msg.Body(&am) != nil || wire.Unmarshal(am.C, &c) != nil || c.ID != id {
				continue
			}
			mk, ok := keys[hex.EncodeToString(h.MK)]
			if !ok {
				break
			}
			if err := add(h.Msg, mk); err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return errors.New("chat: this device doesn't have that message as its sender signed it")
		}
	}
	return e.c.Report(ctx, r, forward)
}
