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

	// SignOut removes this device's identity from the profile. Afterwards,
	// Open returns ErrNoIdentity.
	SignOut(ctx context.Context) error

	// Close stops the stream and closes Events.
	Close() error
}

// ErrNoIdentity is what Open returns for a profile without an identity.
var ErrNoIdentity = errors.New("this profile doesn't have an identity yet")

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

// The kinds of chat.
const (
	DirectChat ChatKind = iota
	GroupChat
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
}

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
	Status MessageStatus
}

// Draft is a message the user is about to send.
type Draft struct {
	Text string
	// ReplyTo is the ID of the message being answered, or empty.
	ReplyTo string
}

// Event is something that happened in the backend. It's one of
// StreamChanged, ChatChanged, MessageAdded, MessageChanged, or SignedOut.
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

// MessageAdded reports a new message in a chat, from anyone.
type MessageAdded struct{ Message Message }

// MessageChanged reports an edit, or a change of status, for a message the
// interface already has.
type MessageChanged struct{ Message Message }

// SignedOut reports that the identity is no longer signed in on this device,
// such as when another device removes it (§7.6).
type SignedOut struct{ Reason string }

func (StreamChanged) event()  {}
func (ChatChanged) event()    {}
func (MessageAdded) event()   {}
func (MessageChanged) event() {}
func (SignedOut) event()      {}
