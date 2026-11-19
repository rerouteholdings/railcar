package chat

import (
	"errors"
	"slices"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
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
	// Admins are a group chat's admins, from tieroute_group (§11.8).
	Admins []string `json:"admins,omitempty"`
	// Name and Topic are a group chat's, from the latest group-meta
	// (§12). Picture is whether it has a picture.
	Name    string `json:"name,omitempty"`
	Topic   string `json:"topic,omitempty"`
	Picture bool   `json:"picture,omitempty"`
	// Replaces is a group chat this one re-creates, offered until the
	// user switches (§11.11).
	Replaces string `json:"replaces,omitempty"`
	// PictureRemoved is set only while a group-meta that removes the
	// picture goes out.
	PictureRemoved bool `json:"-"`
	// Requests are add-requests in a group chat that no admin has
	// answered yet (§11.8).
	Requests []AddPending `json:"requests,omitempty"`
	// Left is set once this identity left the group chat, or was removed
	// from it. Its history stays.
	Left bool `json:"left,omitempty"`
	// For a community channel (KindChannel): its community server, the
	// community's name, the channel's ID and place in the order, whether
	// it's a voice channel, who's in it, the newest sequence number seen,
	// and how far this member has read it (§16.4, §16.5).
	Community     string   `json:"community,omitempty"`
	CommunityName string   `json:"community_name,omitempty"`
	Channel       uint64   `json:"channel,omitempty"`
	Order         int      `json:"order,omitempty"`
	Voice         bool     `json:"voice,omitempty"`
	InVoice       []string `json:"-"`
	NewestSeq     uint64   `json:"newest_seq,omitempty"`
	ReadSeq       uint64   `json:"read_seq,omitempty"`
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
	// Hold is set while a removed device still has a leaf here: nothing
	// but the Remove is sent until it lands (§11.9).
	Hold bool `json:"hold,omitempty"`

	Unread       int    `json:"unread"`
	LastActivity int64  `json:"last_activity"`
	Preview      string `json:"preview"`
	// ReadReceipts is whether the user agreed to send read receipts in
	// this chat (§13.3); nil means they haven't been asked.
	ReadReceipts *bool `json:"read_receipts,omitempty"`
	// ReceiptUpto is the newest message this device sent a read receipt
	// for.
	ReceiptUpto string `json:"receipt_upto,omitempty"`
	// ReceiptAsker is who asked for a read receipt first, while the user
	// hasn't decided; the interface asks the user then (§13.3).
	ReceiptAsker string `json:"receipt_asker,omitempty"`
}

// AddPending is an add-request no admin has answered yet (§11.8).
type AddPending struct {
	ID   string `json:"id"`   // the add-request's msg-id
	From string `json:"from"` // the member who asked
	Who  string `json:"who"`  // the person they want to add
	TS   int64  `json:"ts"`
}

// buffered is a delivery kept for a later epoch.
type buffered struct {
	Epoch    uint64       `json:"epoch"`
	MLS      string       `json:"mls"`
	From     string       `json:"from"`
	Data     wire.Bytes   `json:"data"`
	Receipt  *wire.Signed `json:"receipt,omitempty"`
	Received int64        `json:"received"`
	// Community deliveries also carry a channel, its sequence number, and
	// a code join's proof (§10.1).
	Channel *uint64      `json:"channel,omitempty"`
	Seq     *uint64      `json:"seq,omitempty"`
	Proof   *wire.Signed `json:"proof,omitempty"`
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
	// System marks a line the client shows on its own, such as a new
	// device in the chat (§11.9). It has no sender.
	System bool `json:"system,omitempty"`
	// Seq is a community channel message's sequence number (§16.5).
	Seq uint64 `json:"seq,omitempty"`
	// Gap marks a placeholder for sequence numbers this device lacks,
	// from Seq to GapEnd (§16.5, "Gaps in history").
	Gap    bool   `json:"gap,omitempty"`
	GapEnd uint64 `json:"gap_end,omitempty"`
	// Everyone marks an @everyone that counts (§12, §16.3).
	Everyone bool `json:"everyone,omitempty"`
	// Mentioned is set when the message's mentions name this identity
	// (§12).
	Mentioned bool `json:"mentioned,omitempty"`
	// Files are the message's attachments (§14).
	Files []Attachment `json:"files,omitempty"`
}

// Attachment is a file in a message: its media-ref, and what this device
// learned about it, such as an image's size in pixels once it had the
// bytes, or where it saved it (§14).
type Attachment struct {
	Ref    client.MediaRef `json:"ref"`
	Width  int             `json:"width,omitempty"`
	Height int             `json:"height,omitempty"`
	Saved  string          `json:"saved,omitempty"`
	// Gone is "expired" or "removed" once the homeserver said so (§14,
	// §17.2).
	Gone string `json:"gone,omitempty"`
}

// state is the record that holds contacts and chats.
type state struct {
	Contacts map[string]*Contact `json:"contacts"`
	Chats    map[string]*Chat    `json:"chats"`
	// Handles caches each identity's handle, from its chain.
	Handles map[string]string `json:"handles"`
	// Expected lists the chats a link bundle said this device would be
	// added to (§7.2).
	Expected []string `json:"expected,omitempty"`
	// Communities are the communities this identity is in, by community
	// server name (§16).
	Communities map[string]*Community `json:"communities,omitempty"`
	// AskReceipts is whether this identity's messages ask for read
	// receipts. It's off unless the user turns it on (§13.3).
	AskReceipts bool `json:"ask_receipts,omitempty"`
	// Backup is the message history backup, once this device turned it
	// on (§7.5).
	Backup *BackupState `json:"backup,omitempty"`
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
	if st.Communities == nil {
		st.Communities = map[string]*Community{}
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
	case m.Body == "" && len(m.Files) > 0:
		return fileLabel(m.Files[0].Ref)
	}
	line, _, _ := strings.Cut(m.Body, "\n")
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80]) + "…"
	}
	return line
}
