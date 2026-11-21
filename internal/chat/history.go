package chat

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"encoding/json/jsontext"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// History for new devices (§7.2, §7.3). Every direct chat and group chat
// keeps the signed app-messages it applied, with the msg-key that signed
// each, so this device can pass its history to a device it links. A
// linked device verifies each message it gets, and keeps the ones for a
// chat until it joins that chat.

// Records for history.
const (
	recHistLog  = "hlog."     // followed by the chat's ID in hex: its signed messages
	recMsgKeys  = "msgkeys"   // every msg-key a kept message names, by hash
	recImported = "himported" // messages from a link bundle, waiting for their chats
)

// histContent is the content a history export holds: messages, and the
// edits, deletes, and reactions applied to them (§7.3). Typing, read
// receipts, and protocol content stay out.
var histContent = map[string]bool{CText: true, CFile: true, CEdit: true, CDelete: true, CReact: true}

// histEntry is one kept app-message.
type histEntry struct {
	Msg wire.Signed `json:"msg"`
	MK  wire.Bytes  `json:"mk"` // the hash of the msg-key that signed it
	TS  int64       `json:"ts"`
}

// History lines (§7.3); history-key is community.HistoryKey.
type (
	historyHeader struct {
		T    string `json:"t"`
		V    int    `json:"v"`
		ID   string `json:"id"`
		Made int64  `json:"made"`
	}
	historyChat struct {
		T    string `json:"t"`
		Chat string `json:"chat"`
		Kind string `json:"kind"`
		Name string `json:"name,omitempty"`
	}
	historyMsg struct {
		T    string      `json:"t"`
		Chat string      `json:"chat"`
		Msg  wire.Signed `json:"msg"`
		MK   wire.Bytes  `json:"mk"`
	}
)

// imported is a verified message from a link bundle's history, for the
// chat Key names: the other person for a direct chat, or the group.
type imported struct {
	Kind string      `json:"kind"`
	Key  string      `json:"key"`
	Msg  wire.Signed `json:"msg"`
	MK   wire.Signed `json:"mk"` // the msg-key that signed it
	// Replaced is set when the export holds a newer msg-key for the same
	// device and composite key (§3.2.3).
	Replaced bool `json:"replaced,omitempty"`
}

// replacedKeys marks the items signed with a message key that a msg-key
// with a higher n, for the same identity and device and certified by the
// same composite key, replaced (§3.2.3).
func replacedKeys(items []imported, keys map[string]wire.Signed) {
	newest := map[string]uint64{}
	of := func(s wire.Signed) (string, uint64, bool) {
		var b identity.MsgKeyBody
		if s.Body(&b) != nil {
			return "", 0, false
		}
		return b.ID + "\x00" + string(b.Dev) + "\x00" + string(s.K), b.N, true
	}
	for _, s := range keys {
		if k, n, ok := of(s); ok && n > newest[k] {
			newest[k] = n
		}
	}
	for i := range items {
		if k, n, ok := of(items[i].MK); ok && n < newest[k] {
			items[i].Replaced = true
		}
	}
}

func histRecord(chat string) string { return recHistLog + recordID(chat) }

// keepHistory keeps a signed app-message a chat applied or sent, when
// it's content an export holds. mkData is the signed msg-key that signed
// it. The caller holds e.mu.
func (e *Engine) keepHistory(ch *Chat, signed []byte, mkData []byte) {
	if ch.Kind == KindChannel {
		return
	}
	s, err := wire.ParseSigned(signed)
	if err != nil {
		return
	}
	var am AppMessage
	if s.Body(&am) != nil {
		return
	}
	var head struct {
		T string `json:"t"`
	}
	if wire.Unmarshal(am.C, &head) != nil || !histContent[head.T] {
		return
	}
	mk, err := wire.ParseSigned(mkData)
	if err != nil {
		return
	}
	h := community.MsgKeyHash(mk)
	keys := e.msgKeys()
	if _, ok := keys[hex.EncodeToString(h)]; !ok {
		keys[hex.EncodeToString(h)] = mk
		if err := e.profile().Save(recMsgKeys, keys); err != nil {
			e.log.Info("chat: keeping a msg-key", "err", err)
		}
	}
	l := e.histLog(ch.ID)
	if slices.ContainsFunc(l, func(x histEntry) bool { return bytes.Equal(x.Msg.S, s.S) }) {
		return // already kept, such as a message restored again
	}
	l = append(l, histEntry{Msg: s, MK: h, TS: am.TS})
	e.keepForBackup(ch, s, h)
	if len(l) > maxHistory {
		l = l[len(l)-maxHistory:]
	}
	if err := e.profile().Save(histRecord(ch.ID), l); err != nil {
		e.log.Info("chat: keeping history", "err", err)
	}
}

func (e *Engine) histLog(chat string) []histEntry {
	var l []histEntry
	if err := e.profile().Load(histRecord(chat), &l); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: a chat's history", "err", err)
	}
	return l
}

func (e *Engine) msgKeys() map[string]wire.Signed {
	keys := map[string]wire.Signed{}
	if err := e.profile().Load(recMsgKeys, &keys); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: the msg-keys", "err", err)
	}
	return keys
}

// moveHistory puts an old chat's kept history before a new one's, when a
// direct chat replaces another (§11.11). The caller holds e.mu.
func (e *Engine) moveHistory(oldID, newID string) {
	old := e.histLog(oldID)
	if len(old) == 0 {
		return
	}
	l := append(old, e.histLog(newID)...)
	if len(l) > maxHistory {
		l = l[len(l)-maxHistory:]
	}
	if err := e.profile().Save(histRecord(newID), l); err == nil {
		e.profile().Delete(histRecord(oldID))
	}
}

// exportHistory writes this device's history of direct chats and group
// chats as one version 2 part, JSON Lines in time order (§7.3). The
// caller holds e.mu.
func (e *Engine) exportHistory() ([]byte, error) {
	var b bytes.Buffer
	line := func(v any) error {
		j, err := wire.JCS(v)
		if err != nil {
			return err
		}
		b.Write(j)
		b.WriteByte('\n')
		return nil
	}
	if err := line(historyHeader{T: "history", V: 2, ID: e.c.ID(), Made: e.c.Clock().Now().Unix()}); err != nil {
		return nil, err
	}
	type entry struct {
		chat string
		histEntry
	}
	var all []entry
	named := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(e.st.Chats)) {
		ch := e.st.Chats[id]
		if ch.Kind == KindChannel {
			continue
		}
		l := e.histLog(id)
		if len(l) == 0 {
			continue
		}
		key, item := id, historyChat{T: "chat", Chat: id, Kind: ch.Kind, Name: ch.Name}
		if ch.Kind == KindDirect {
			key, item = ch.Peer, historyChat{T: "chat", Chat: ch.Peer, Kind: KindDirect}
		}
		if !named[key] {
			named[key] = true
			if err := line(item); err != nil {
				return nil, err
			}
		}
		for _, h := range l {
			all = append(all, entry{key, h})
		}
	}
	slices.SortStableFunc(all, func(a, b entry) int { return int(a.TS - b.TS) })
	keys := e.msgKeys()
	seen := map[string]bool{}
	for _, it := range all {
		k := hex.EncodeToString(it.MK)
		mk, ok := keys[k]
		if !ok {
			continue
		}
		if !seen[k] {
			seen[k] = true
			if err := line(community.HistoryKey{T: community.ItemKey, Key: mk}); err != nil {
				return nil, err
			}
		}
		if err := line(historyMsg{T: community.ItemMsg, Chat: it.chat, Msg: it.Msg, MK: it.MK}); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// uploadLinkHistory exports this device's history and uploads it as files of
// at most 64 MiB, split between lines, for a link bundle's history
// (§7.2, §7.3). An export with no messages gives nothing. The caller
// holds e.mu.
func (e *Engine) uploadLinkHistory(ctx context.Context) ([]jsontext.Value, error) {
	data, err := e.exportHistory()
	if err != nil {
		return nil, err
	}
	if bytes.Count(data, []byte{'\n'}) <= 1 {
		return nil, nil
	}
	var out []jsontext.Value
	for len(data) > 0 {
		n := len(data)
		if n > client.MaxFile {
			n = bytes.LastIndexByte(data[:client.MaxFile], '\n') + 1
			if n <= 0 {
				return nil, errors.New("chat: a history line over 64 MiB")
			}
		}
		ref, err := e.c.UploadFile(ctx, data[:n], "history.jsonl", "application/jsonl")
		if err != nil {
			return nil, err
		}
		j, err := wire.Marshal(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
		data = data[n:]
	}
	return out, nil
}

// importHistory downloads a link bundle's history, verifies each message
// (§7.3, "Importing"), and keeps the ones that pass until this device
// joins their chats. The caller holds e.mu.
func (e *Engine) importHistory(ctx context.Context, refs []jsontext.Value) error {
	var data []byte
	for _, raw := range refs {
		var ref client.MediaRef
		if err := wire.Unmarshal(raw, &ref); err != nil {
			return err
		}
		if err := ref.Check(); err != nil {
			return err
		}
		part, err := e.c.DownloadFile(ctx, ref)
		if err != nil {
			return err
		}
		data = append(data, part...)
	}
	items := e.readExport(ctx, data)
	pending := e.loadImported()
	pending = append(pending, items...)
	if err := e.profile().Save(recImported, pending); err != nil {
		return err
	}
	for _, ch := range e.st.Chats {
		e.applyImported(ctx, ch)
	}
	return nil
}

// readExport checks an export's lines, and returns its messages that
// pass. Only a version 2 part for this identity counts: Railcar never
// had a version 1 identity, so it skips version 1 parts (§7.3).
func (e *Engine) readExport(ctx context.Context, data []byte) []imported {
	if !utf8.Valid(data) {
		e.log.Info("chat: a history export that isn't UTF-8")
		return nil
	}
	var out []imported
	inPart := false
	kinds := map[string]string{}
	keys := map[string]wire.Signed{}
	chains := map[string]*identity.Chain{}
	dropped := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var head struct {
			T string `json:"t"`
		}
		if wire.Unmarshal([]byte(line), &head) != nil {
			continue
		}
		if head.T == "history" {
			var h historyHeader
			inPart = wire.Unmarshal([]byte(line), &h) == nil && h.V == 2 && h.ID == e.c.ID()
			continue
		}
		if !inPart {
			continue
		}
		switch head.T {
		case "chat":
			var c historyChat
			if wire.Unmarshal([]byte(line), &c) == nil && (c.Kind == KindDirect || c.Kind == KindGroup) {
				kinds[c.Chat] = c.Kind
			}
		case community.ItemKey:
			var k community.HistoryKey
			if wire.Unmarshal([]byte(line), &k) == nil && k.Key.T == identity.TypeMsgKey {
				keys[string(community.MsgKeyHash(k.Key))] = k.Key
			}
		case community.ItemMsg:
			var m historyMsg
			if wire.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			kind, ok := kinds[m.Chat]
			mk, kok := keys[string(m.MK)]
			if !ok || !kok {
				continue // a community channel's, or a line without its key
			}
			if err := e.verifyHistoryMsg(ctx, kind, m.Chat, m.Msg, mk, chains); err != nil {
				dropped++
				continue
			}
			out = append(out, imported{Kind: kind, Key: m.Chat, Msg: m.Msg, MK: mk})
		}
	}
	if dropped > 0 {
		e.log.Info("chat: dropped messages from imported history", "count", dropped)
	}
	replacedKeys(out, keys)
	e.log.Debug("chat: imported history", "messages", len(out), "chats", len(kinds), "keys", len(keys))
	return out
}

// verifyHistoryMsg checks one imported message (§7.3): its signature
// under its msg-key, the msg-key signed by a device that appears in the
// sender's chain, and its group: the chat's for a group, and any group
// for a direct chat, whose sender is one of its two people.
func (e *Engine) verifyHistoryMsg(ctx context.Context, kind, chat string, msg, mkSigned wire.Signed, chains map[string]*identity.Chain) error {
	var mk identity.MsgKeyBody
	if err := mkSigned.Body(&mk); err != nil {
		return err
	}
	if msg.T != TypeAppMessage {
		return errors.New("chat: not an app-message")
	}
	if err := sig.VerifyMessageKey(msg, mk.Key); err != nil {
		return err
	}
	var am AppMessage
	if err := msg.Body(&am); err != nil {
		return err
	}
	if am.ID != mk.ID || am.Channel != nil {
		return errors.New("chat: a history message for another identity, or a channel")
	}
	switch kind {
	case KindGroup:
		if wire.EncodeB64(am.Group) != chat {
			return errors.New("chat: a history message for another group")
		}
	case KindDirect:
		if am.ID != e.c.ID() && am.ID != chat {
			return errors.New("chat: a direct chat's history message from someone else")
		}
	}
	c, ok := chains[am.ID]
	if !ok {
		var err error
		if am.ID == e.c.ID() {
			c = e.c.Chain()
		} else if c, err = e.fetchChain(ctx, am.ID, false); err != nil {
			return err
		}
		chains[am.ID] = c
	}
	for _, info := range c.Info {
		for _, op := range info.Body.Ops {
			if op.Op == identity.OpAddDevice {
				if _, err := identity.VerifyMsgKey(mkSigned, am.ID, op.Pub); err == nil {
					return nil
				}
			}
		}
	}
	return errors.New("chat: a msg-key no device of the sender's signed")
}

func (e *Engine) loadImported() []imported {
	var l []imported
	if err := e.profile().Load(recImported, &l); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: imported history", "err", err)
	}
	return l
}

// applyImported applies the imported messages that belong to a chat, in
// order, before what the chat already shows, and keeps them for the next
// device. Imported messages don't count as unread. The caller holds
// e.mu.
func (e *Engine) applyImported(ctx context.Context, ch *Chat) {
	if ch.Kind == KindChannel {
		return
	}
	pending := e.loadImported()
	if len(pending) == 0 {
		return
	}
	mine := func(it imported) bool {
		return it.Kind == ch.Kind && (ch.Kind == KindDirect && it.Key == ch.Peer || ch.Kind == KindGroup && it.Key == ch.ID)
	}
	if !slices.ContainsFunc(pending, mine) {
		return
	}
	unread, asker := ch.Unread, ch.ReceiptAsker
	replaced := map[string]bool{}
	for _, it := range pending {
		if !mine(it) {
			continue
		}
		var am AppMessage
		if it.Msg.Body(&am) != nil {
			continue
		}
		var head struct {
			T string `json:"t"`
		}
		if wire.Unmarshal(am.C, &head) != nil || !histContent[head.T] {
			continue
		}
		if err := e.applyContent(ctx, ch, am.ID, am, head.T, am.TS); err != nil {
			e.log.Debug("chat: an imported message", "err", err)
			continue
		}
		if it.Replaced {
			var c struct {
				ID string `json:"id"`
			}
			if wire.Unmarshal(am.C, &c) == nil && c.ID != "" {
				replaced[c.ID] = true
			}
		}
		e.keepHistory(ch, it.Msg.Canonical(), it.MK.Canonical())
	}
	ch.Unread, ch.ReceiptAsker = unread, asker
	if msgs, err := e.messages(ch.ID); err == nil {
		slices.SortStableFunc(msgs, func(a, b Message) int { return int(a.TS - b.TS) })
		for i := range msgs {
			if replaced[msgs[i].ID] {
				msgs[i].ReplacedKey = true
			}
		}
		saveMessages(e.profile(), recordID(ch.ID), msgs)
	}
	pending = slices.DeleteFunc(pending, mine)
	if err := e.profile().Save(recImported, pending); err != nil {
		e.log.Info("chat: imported history", "err", err)
	}
	e.save()
	e.emit(Event{Kind: EventChat, Chat: ptr(ch.public())})
}
