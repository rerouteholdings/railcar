package chat

import (
	"errors"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Profile records this package keeps, next to the client's own.
const (
	recState    = "chats"
	recMessages = "messages." // followed by the chat's ID
)

// Contact states as this device keeps them. The homeserver's list has the
// first four (§9); declined is what a decline notice leaves (§9.3).
const (
	StateContact    = "contact"
	StatePendingIn  = "pending-in"
	StatePendingOut = "pending-out"
	StateBlocked    = "blocked"
	StateDeclined   = "declined"
)

// Contact is someone in the contact list, or a request in or out.
type Contact struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
	State  string `json:"state"`
	Since  int64  `json:"since"`
	// Request is the signed request, for pending-in.
	Request *wire.Signed `json:"request,omitempty"`
	// Chat is the direct chat's ID, once there is one.
	Chat string `json:"chat,omitempty"`
}

// Chat is a direct chat or group chat this device is in.
type Chat struct {
	ID      string   `json:"id"` // the group ID, base64url
	Kind    string   `json:"kind"`
	Hub     string   `json:"hub"`
	Peer    string   `json:"peer,omitempty"` // a direct chat's other identity
	Members []string `json:"members"`        // every member identity, this one included
	Epoch   uint64   `json:"epoch"`
	// Applied is the hash of the commit applied in each recent epoch, to
	// notice a hub that sends members different commits (§11.6).
	Applied map[uint64]wire.Bytes `json:"applied,omitempty"`
	// Buffered holds deliveries for an epoch this device hasn't reached
	// (§11.6).
	Buffered []buffered `json:"buffered,omitempty"`
	// Stopped is why this device stopped following the group, such as a
	// refused commit. It sends nothing to the group again (§7.1).
	Stopped string `json:"stopped,omitempty"`
	// Pending is set while a commit can't be judged yet: nothing is sent
	// until it's decided (§7.1).
	Pending bool `json:"pending,omitempty"`

	Unread       int    `json:"unread"`
	LastActivity int64  `json:"last_activity"`
	Preview      string `json:"preview"`
	// ReadReceipts is whether the user agreed to send read receipts in
	// this chat (§13.3); nil means they haven't been asked.
	ReadReceipts *bool `json:"read_receipts,omitempty"`
}

// buffered is a delivery kept for a later epoch.
type buffered struct {
	Epoch    uint64       `json:"epoch"`
	MLS      string       `json:"mls"`
	From     string       `json:"from"`
	Data     wire.Bytes   `json:"data"`
	Receipt  *wire.Signed `json:"receipt,omitempty"`
	Received int64        `json:"received"`
}

// maxBuffered bounds the deliveries kept for later epochs in one chat.
const maxBuffered = 500

// Message statuses.
const (
	Sent    = "sent"
	Sending = "sending"
	Failed  = "failed"
)

// Message is one message in a chat's local history.
type Message struct {
	ID       string `json:"id"`   // its msg-id
	From     string `json:"from"` // the sender's identifier
	TS       int64  `json:"ts"`   // the sender's ts
	Received int64  `json:"received"`
	Body     string `json:"body"`
	Reply    string `json:"reply,omitempty"`
	Edited   bool   `json:"edited,omitempty"`
	Deleted  bool   `json:"deleted,omitempty"`
	// Unsupported is the content type of a message this client can't show
	// (§12).
	Unsupported string `json:"unsupported,omitempty"`
	// Reactions maps each emoji to the identities that reacted with it.
	Reactions map[string][]string `json:"reactions,omitempty"`
	Status    string              `json:"status"`
	// AskedReceipt is whether the sender asked for a read receipt (§13.3).
	AskedReceipt bool `json:"asked_receipt,omitempty"`
	// ReadBy lists who sent a read receipt covering this message.
	ReadBy []string `json:"read_by,omitempty"`
}

// state is the record that holds contacts and chats.
type state struct {
	Contacts map[string]*Contact `json:"contacts"`
	Chats    map[string]*Chat    `json:"chats"`
	// Handles caches each identity's handle, from its chain.
	Handles map[string]string `json:"handles"`
}

func loadState(p *store.Profile) (*state, error) {
	st := &state{}
	if err := p.Load(recState, st); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if st.Contacts == nil {
		st.Contacts = map[string]*Contact{}
	}
	if st.Chats == nil {
		st.Chats = map[string]*Chat{}
	}
	if st.Handles == nil {
		st.Handles = map[string]string{}
	}
	return st, nil
}

func messagesRecord(chat string) string { return recMessages + chat }

// maxHistory is how many messages a chat keeps on this device.
const maxHistory = 5000

func loadMessages(p *store.Profile, chat string) ([]Message, error) {
	var ms []Message
	err := p.Load(messagesRecord(chat), &ms)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return ms, err
}

func saveMessages(p *store.Profile, chat string, ms []Message) error {
	if len(ms) > maxHistory {
		ms = ms[len(ms)-maxHistory:]
	}
	return p.Save(messagesRecord(chat), ms)
}

func findMessage(ms []Message, id string) int {
	return slices.IndexFunc(ms, func(m Message) bool { return m.ID == id })
}

// preview is a chat list line for a message.
func preview(m Message) string {
	switch {
	case m.Deleted:
		return "Message deleted"
	case m.Unsupported != "":
		return "Unsupported message"
	}
	line, _, _ := strings.Cut(m.Body, "\n")
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80]) + "…"
	}
	return line
}
