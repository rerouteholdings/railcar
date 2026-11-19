// Package ui is Railcar's terminal interface, built with Bubble Tea, Bubbles,
// Lip Gloss, and Huh. It talks to the protocol only through the Backend
// interface, so it runs against the demo backend before the protocol is done.
package ui

import (
	"context"
	"errors"
	"time"
)

// Backend is everything the interface needs from the protocol side. The
// interface calls each method from a Bubble Tea command, never from Update, so
// a method may block on the network. Methods must be safe to call from more
// than one goroutine.
type Backend interface {
	// Open loads the identity saved in the profile. It returns ErrNoIdentity
	// when the profile doesn't have one yet, and the interface starts
	// onboarding.
	Open(ctx context.Context) (Identity, error)

	// SignUp creates an identity on a homeserver with a registration code. It
	// returns the identity and its new recovery code as 12 words (§6.1). The
	// interface shows the words once, asks for some of them back, and then
	// calls FinishSignUp. It never stores the words.
	SignUp(ctx context.Context, req SignUpRequest) (Identity, []string, error)

	// FinishSignUp tells the backend the user has written down the recovery
	// words. The backend forgets them, as §6.1 requires, and the identity is
	// ready to use.
	FinishSignUp(ctx context.Context) error

	// Recover signs in to an existing identity on this device with its
	// handle and recovery code (§6.1).
	Recover(ctx context.Context, handle string, words []string) (Identity, error)

	// StartLink starts signing in to an existing identity with another
	// device's help (§7.2): this device makes its keys, leaves its key on
	// the homeserver, and returns the link code to show as a QR code and as
	// text. The other device approves it.
	StartLink(ctx context.Context, req LinkRequest) (string, error)

	// StartLinkWithCode starts signing in with an approval code that
	// another device shows (§7.2.1). There's no confirmation code to type.
	StartLinkWithCode(ctx context.Context, code string) error

	// ConfirmLink sends the confirmation code the other device shows back
	// to it (§7.2 step 2).
	ConfirmLink(ctx context.Context, code string) error

	// FinishLink tries to sign in once the other device has added this
	// one. It returns ErrNotLinkedYet until then; the interface calls it
	// again every couple of seconds.
	FinishLink(ctx context.Context) (Identity, error)

	// CancelLink gives up on signing in with another device.
	CancelLink()

	// Connect starts the live stream and keeps it open, reconnecting on its
	// own. It returns once the first attempt has started. Every change of
	// state arrives as a StreamChanged event.
	Connect(ctx context.Context) error

	// Events delivers what happens while the interface runs. The channel stays
	// open until Close.
	Events() <-chan Event

	// Chats returns every direct chat and group chat, in any order. The
	// interface sorts them by LastActivity.
	Chats(ctx context.Context) ([]Chat, error)

	// Messages returns up to limit messages in a chat, oldest first. With an
	// empty before, they're the latest ones. Otherwise they're the ones just
	// before the message with that ID.
	Messages(ctx context.Context, chat string, before string, limit int) ([]Message, error)

	// Send queues a message in a chat. It returns the message with
	// StatusSending, then a MessageChanged event once it's sent or has failed.
	Send(ctx context.Context, chat string, draft Draft) (Message, error)

	// MarkRead marks every message in a chat as read, which clears its unread
	// count.
	MarkRead(ctx context.Context, chat string) error

	// Edit replaces the text of one of the identity's own messages (§12).
	Edit(ctx context.Context, chat, message, text string) error

	// Delete deletes one of the identity's own messages for everyone (§12).
	Delete(ctx context.Context, chat, message string) error

	// React adds a reaction to a message, or takes it back when the
	// identity already reacted with that emoji (§12).
	React(ctx context.Context, chat, message, emoji string) error

	// SetTyping tells the chat's other members whether the user is typing
	// (§13.3).
	SetTyping(ctx context.Context, chat string, on bool) error

	// MakeInvite makes an invite code others can send a contact request
	// with, and returns it as it's shared: @handle::code (§9.1, §9.4).
	MakeInvite(ctx context.Context) (string, error)

	// AddContact sends a contact request with someone's code, @handle::code
	// or its link (§9.2, §9.4). The request shows in the chat list as a
	// SentRequest until they accept.
	AddContact(ctx context.Context, code string) error

	// AcceptRequest accepts the contact request shown as a ContactRequest
	// chat, which starts a direct chat with them (§9.3).
	AcceptRequest(ctx context.Context, chat string) error

	// DeclineRequest declines the contact request shown as a
	// ContactRequest chat. The requester is told (§9.3).
	DeclineRequest(ctx context.Context, chat string) error

	// ApproveLinkCode starts adding the device that shows a link code
	// (§7.2 step 2), and returns the confirmation code to show, as
	// XXXX-XXXX. name is what the new device is called in the identity's
	// chain.
	ApproveLinkCode(ctx context.Context, code, name string) (string, error)

	// ShowApprovalCode makes an approval code for a new device to read
	// (§7.2.1), as text. It works for 10 minutes and for one device.
	ShowApprovalCode(ctx context.Context, name string) (ApprovalCode, error)

	// FinishApproval looks once for the new device's answer to the code
	// ApproveLinkCode or ShowApprovalCode showed. Once it's there, it adds
	// the device to the identity and to every chat (§7.2, §11.9), and
	// returns true. ErrWrongConfirmation means the new device sent back
	// another confirmation code; keep waiting.
	FinishApproval(ctx context.Context) (bool, error)

	// CancelApproval stops adding a device, and forgets its code.
	CancelApproval()

	// Devices lists the identity's devices, from its chain.
	Devices(ctx context.Context) ([]Device, error)

	// RemoveDevice removes another of the identity's devices (§7.6). Its
	// chats follow (§11.9).
	RemoveDevice(ctx context.Context, key string) error

	// SignOut removes this device's identity from the profile. Afterwards,
	// Open returns ErrNoIdentity.
	SignOut(ctx context.Context) error

	// Close stops the stream and closes Events.
	Close() error
}

// ErrNoIdentity is what Open returns for a profile without an identity.
var ErrNoIdentity = errors.New("this profile doesn't have an identity yet")

// ErrSignUpRefused marks a FinishSignUp error for a handle or registration
// code the homeserver refused. The interface goes back to the form to fix
// them. A backend may return the same recovery words from the next
// SignUp, and the interface then doesn't ask for them back again.
var ErrSignUpRefused = errors.New("the homeserver refused the handle or registration code")

// ErrNotLinkedYet is what FinishLink returns until the other device has
// added this one.
var ErrNotLinkedYet = errors.New("the other device hasn't added this one yet")

// ErrWrongConfirmation is what FinishApproval returns when the new device
// sent back a confirmation code other than the one shown.
var ErrWrongConfirmation = errors.New("the new device sent back a different confirmation code. Check the code, and enter it there again")

// ErrApprovalUsedTwice is what FinishApproval returns when more than one
// device used an approval code (§7.2.1 step 3). None of them is added.
var ErrApprovalUsedTwice = errors.New("more than one device used the code, so Railcar added none of them. Someone else may have seen it")

// ErrApprovalExpired is what FinishApproval returns once an approval code
// has stopped working, after 10 minutes.
var ErrApprovalExpired = errors.New("the code expired. Make a new one to add a device")

// LinkRequest names the identity a new device signs in to.
type LinkRequest struct {
	Handle     string
	Homeserver string
}

// ApprovalCode is an approval code to show, and when it stops working.
type ApprovalCode struct {
	Text    string
	Expires time.Time
}

// Device is one of the identity's devices.
type Device struct {
	// Key identifies the device, for RemoveDevice.
	Key string
	// Name is what it's called in the identity's chain, such as "Kat's
	// laptop". It may be empty.
	Name string
	// Added is when it was added.
	Added time.Time
	// This is true for the device Railcar is running on.
	This bool
}

// Identity is the signed-in identity, as the interface shows it.
type Identity struct {
	// Handle is the identity's handle, without the leading @, such as
	// kat.chat.example.com (§5.2).
	Handle string
	// Homeserver is the homeserver's name, such as chat.example.com.
	Homeserver string
	// DisplayName is the name on the identity's profile. It may be empty.
	DisplayName string
	// Device is this device's name, such as "Railcar on kat-laptop".
	Device string
}

// SignUpRequest is what the user enters to create an identity.
type SignUpRequest struct {
	Homeserver       string
	Handle           string
	RegistrationCode string
}

// StreamState is the state of the live stream to the homeserver.
type StreamState int

// The stream states.
const (
	StreamOffline StreamState = iota
	StreamConnecting
	StreamConnected
	StreamReconnecting
)

// String returns the state as the status bar shows it.
func (s StreamState) String() string {
	switch s {
	case StreamConnecting:
		return "Connecting"
	case StreamConnected:
		return "Connected"
	case StreamReconnecting:
		return "Reconnecting"
	default:
		return "Offline"
	}
}

// ChatKind tells direct chats from group chats.
type ChatKind int

// The kinds of chat. Contact requests show in the chat list too, until
// they're accepted or declined.
const (
	DirectChat ChatKind = iota
	GroupChat
	// ContactRequest is someone asking to be a contact (§9.2).
	ContactRequest
	// SentRequest is the identity's own request, waiting for an answer.
	SentRequest
)

// Member is one person in a chat.
type Member struct {
	Name   string
	Handle string
}

// Chat is one entry in the chat list.
type Chat struct {
	ID    string
	Kind  ChatKind
	Title string
	// Members lists everyone in the chat except the signed-in identity.
	Members []Member
	Unread  int
	// Preview is a short plain-text line from the latest message.
	Preview      string
	LastActivity time.Time
	// Stopped says why nothing can be sent in the chat anymore, such as a
	// change from another member that broke the rules (§7.1). It's empty
	// for a chat that works.
	Stopped string
}

// IsRequest reports whether the entry is a contact request, not a chat.
func (c Chat) IsRequest() bool { return c.Kind == ContactRequest || c.Kind == SentRequest }

// MessageStatus tracks a message the signed-in identity sent.
type MessageStatus int

// The message statuses. Received messages are always StatusSent.
const (
	StatusSent MessageStatus = iota
	StatusSending
	StatusFailed
)

// Message is one message in a chat.
type Message struct {
	ID     string
	Chat   string
	Sender Member
	// Mine is true for messages the signed-in identity sent, from any of
	// its devices.
	Mine bool
	Sent time.Time
	Text string
	// ReplyTo is the ID of the message this one answers, or empty.
	ReplyTo string
	// Edited is true when the sender has changed the text since sending.
	Edited bool
	// Deleted is true when the sender deleted the message for everyone.
	Deleted bool
	// Unsupported is true for content this version of Railcar can't show
	// (§12).
	Unsupported bool
	Reactions   []Reaction
	// Read is true for the identity's own message once the other side sent
	// a read receipt for it (§13.3).
	Read bool
	// System marks a line Railcar shows on its own, such as "@kat has a new
	// device" (§11.9). It has no sender.
	System bool
	Status MessageStatus
}

// Reaction is one emoji on a message, and how many people reacted with it.
type Reaction struct {
	Emoji string
	Count int
	// Mine is true when the signed-in identity is one of them.
	Mine bool
}

// Draft is a message the user is about to send.
type Draft struct {
	Text string
	// ReplyTo is the ID of the message being answered, or empty.
	ReplyTo string
}

// Event is something that happened in the backend. It's one of
// StreamChanged, ChatChanged, ChatRemoved, MessageAdded, MessageChanged,
// Typing, or SignedOut.
type Event interface{ event() }

// StreamChanged reports a new stream state. Err is why the stream is offline
// or reconnecting, when that's known.
type StreamChanged struct {
	State StreamState
	Err   error
}

// ChatChanged reports a new chat, or a change to one: its title, members,
// unread count, or preview.
type ChatChanged struct{ Chat Chat }

// ChatRemoved reports that an entry left the chat list, such as a
// declined request, or a direct chat replaced by a new one with the same
// person (§11.11).
type ChatRemoved struct{ Chat string }

// MessageAdded reports a new message in a chat, from anyone.
type MessageAdded struct{ Message Message }

// MessageChanged reports an edit, or a change of status, for a message the
// interface already has.
type MessageChanged struct{ Message Message }

// Typing reports that someone in a chat started or stopped typing
// (§13.3).
type Typing struct {
	Chat string
	Who  Member
	On   bool
}

// SignedOut reports that the identity is no longer signed in on this device,
// such as when another device removes it (§7.6).
type SignedOut struct{ Reason string }

func (StreamChanged) event()  {}
func (ChatChanged) event()    {}
func (ChatRemoved) event()    {}
func (MessageAdded) event()   {}
func (MessageChanged) event() {}
func (SignedOut) event()      {}
func (Typing) event()         {}
