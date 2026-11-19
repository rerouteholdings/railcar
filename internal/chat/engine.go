// Package chat is Railcar's chat layer: contacts (§9), direct chats as MLS
// groups (§11), and the messages in them (§12, §13.3). It sits between the
// protocol client, which talks to the homeserver, and the interface. Every
// Tieroute rule about groups and messages that Railcar follows lives here;
// the MLS library only does MLS.
//
// An Engine processes the inbox in order, one item at a time, and keeps
// contacts, chats, the MLS state, and each chat's history in the profile,
// encrypted at rest.
package chat

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
)

// EventKind says what an Event reports.
type EventKind int

// The kinds of event.
const (
	// EventStream: the live stream's state changed. Stream holds the
	// client's event kind, and Err why it dropped.
	EventStream EventKind = iota
	// EventContact: a contact or request changed.
	EventContact
	// EventChat: a chat was created or changed.
	EventChat
	// EventChatRemoved: a chat is gone from this device, such as a direct
	// chat replaced by a new one with the same person (§11.11). ChatID
	// names it.
	EventChatRemoved
	// EventMessage: a message was added or changed.
	EventMessage
	// EventTyping: someone started or stopped typing in a chat.
	EventTyping
	// EventStopped: the stream stopped for good, such as when this device
	// was removed from the identity (§7.6).
	EventStopped
)

// Event is something that happened.
type Event struct {
	Kind    EventKind
	Stream  client.EventKind
	Err     error
	Contact *Contact
	Chat    *Chat
	Message *Message
	ChatID  string
	// New is true for a message that wasn't there before.
	New bool
	// Who and On describe a typing indicator.
	Who string
	On  bool
}

// Options adjust an Engine.
type Options struct {
	// Logger receives debug lines. Message text and keys never go in it.
	Logger *slog.Logger
}

// Engine is the chat layer for one signed-in identity on this device.
type Engine struct {
	c   *client.Client
	dev *Device
	log *slog.Logger
	key ed25519.PrivateKey // the message key, which signs app-messages (§3.2.3)

	// mu serializes everything that touches the MLS state or the records:
	// processing the inbox, sending, and creating chats.
	mu     sync.Mutex
	st     *state
	leaves map[string]bool // leaves whose msg-key checked out, by signature key and certificate
	seen   map[string]bool // deliveries processed since start, by ID
	// syncUntil is when the sync rule stops retrying; zero when it's done
	// (§11.9).
	syncUntil time.Time
	// fresh is when the sync rule last fetched each identity's chain from
	// its home.
	fresh map[string]time.Time

	// cchains are the communities' checked chains, by community server.
	cchains map[string]*community.Chain
	// runCtx is Run's context, for streams of communities joined later.
	runCtx context.Context
	// joinCode is the code a join is using, by community server, for
	// code access before this identity is a member.
	joinCode map[string][]byte
	crunMu   sync.Mutex
	cruns    map[string]bool

	evMu   sync.Mutex
	events chan Event
	closed bool
}

// New starts the chat layer for a client: it opens the MLS state and the
// chat records from the client's profile.
func New(c *client.Client, opts Options) (*Engine, error) {
	info, err := c.MLSInfo()
	if err != nil {
		return nil, err
	}
	dev, err := OpenDevice(c.Profile(), info)
	if err != nil {
		return nil, err
	}
	st, err := loadState(c.Profile())
	if err != nil {
		dev.Close()
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		c: c, dev: dev, log: log, st: st,
		key:      ed25519.NewKeyFromSeed(info.MsgKeySeed),
		leaves:   map[string]bool{},
		seen:     map[string]bool{},
		fresh:    map[string]time.Time{},
		cchains:  map[string]*community.Chain{},
		cruns:    map[string]bool{},
		joinCode: map[string][]byte{},
		events:   make(chan Event, 256),
	}, nil
}

// Client returns the protocol client.
func (e *Engine) Client() *client.Client { return e.c }

// Events delivers what happens. The channel closes on Close. If nobody
// reads it, events are dropped once 256 are waiting.
func (e *Engine) Events() <-chan Event { return e.events }

func (e *Engine) emit(ev Event) {
	e.evMu.Lock()
	defer e.evMu.Unlock()
	if e.closed {
		return
	}
	select {
	case e.events <- ev:
	default:
		e.log.Debug("chat: an event was dropped; nobody is reading")
	}
}

// Close closes Events and frees the MLS state. Call it after Run returns.
func (e *Engine) Close() {
	e.evMu.Lock()
	if !e.closed {
		e.closed = true
		close(e.events)
	}
	e.evMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dev.Close()
}

// save writes the chats record. The caller holds e.mu.
func (e *Engine) save() error {
	return e.c.Profile().Save(recState, e.st)
}

// EnsureKeyPackages tops up this device's key packages on the homeserver
// to 50, with a last-resort one (§11.4).
func (e *Engine) EnsureKeyPackages(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.topUp(ctx)
}

// topUp is EnsureKeyPackages for a caller that holds e.mu. Making key
// packages saves the MLS state, which sets the point Revert returns to, so
// it never runs while another change is in progress.
func (e *Engine) topUp(ctx context.Context) error {
	n, lastResort, err := e.c.KeyPackageCount(ctx)
	if err != nil {
		return err
	}
	if n >= client.DefaultKeyPackages/2 && lastResort {
		return nil
	}
	_, err = e.c.UploadKeyPackages(ctx, e.dev, client.DefaultKeyPackages-n, client.UploadOptions{LastResort: !lastResort})
	return err
}

// Run keeps the live stream open and processes every inbox item until ctx
// ends or the stream stops for good. It tops up key packages first.
func (e *Engine) Run(ctx context.Context) error {
	if err := e.EnsureKeyPackages(ctx); err != nil {
		e.log.Debug("chat: key packages", "err", err)
	}
	e.mu.Lock()
	e.wantSync()
	e.mu.Unlock()
	e.runCommunities(ctx)
	events := e.c.Stream(ctx, client.StreamOptions{})
	tick := time.NewTicker(syncEvery)
	defer tick.Stop()
	for {
		var ev client.Event
		select {
		case <-tick.C:
			e.retrySync(ctx)
			continue
		case got, ok := <-events:
			if !ok {
				return nil
			}
			ev = got
		}
		switch ev.Kind {
		case client.EventConnected:
			e.emit(Event{Kind: EventStream, Stream: ev.Kind})
			go e.catchUpGroups(ctx)
		case client.EventDisconnected, client.EventError:
			e.emit(Event{Kind: EventStream, Stream: ev.Kind, Err: ev.Err})
		case client.EventItem:
			e.handle(ctx, ev.Item)
		case client.EventStopped:
			e.emit(Event{Kind: EventStopped, Err: ev.Err})
			if errors.Is(ev.Err, context.Canceled) || errors.Is(ev.Err, context.DeadlineExceeded) {
				return nil
			}
			return ev.Err
		}
	}
}

// Sync fetches and processes every inbox item waiting, without the
// stream. It's for tests and one-shot tools.
func (e *Engine) Sync(ctx context.Context) error {
	if err := e.syncHome(ctx); err != nil {
		return err
	}
	return e.syncCommunities(ctx)
}

func (e *Engine) syncHome(ctx context.Context) error {
	for range 1000 {
		page, err := e.c.Inbox(ctx, e.c.Cursor(), 0)
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return nil
		}
		for i := range page.Items {
			if err := e.handle(ctx, &page.Items[i]); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
	}
	return nil
}

// retryable marks a failure that may pass, such as an unreachable server,
// as opposed to a verdict on the item.
type retryable struct{ err error }

func (r retryable) Error() string { return r.err.Error() }
func (r retryable) Unwrap() error { return r.err }

// handle processes one inbox item and acks it. An item whose processing
// failed for a reason that may pass is tried again a few times first.
func (e *Engine) handle(ctx context.Context, it *client.Item) error {
	if it.Delivery != nil && it.Delivery.ID != "" && e.seen[it.Delivery.ID] {
		return e.ack(ctx, it)
	}
	wait := 2 * time.Second
	for attempt := 0; ; attempt++ {
		err := e.process(ctx, it)
		var r retryable
		if err == nil || !errors.As(err, &r) || attempt >= 4 {
			if err != nil {
				e.log.Info("chat: dropped an inbox item", "err", err)
			}
			break
		}
		e.log.Debug("chat: will retry an inbox item", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	if it.Delivery != nil && it.Delivery.ID != "" {
		e.seen[it.Delivery.ID] = true
	}
	return e.ack(ctx, it)
}

func (e *Engine) ack(ctx context.Context, it *client.Item) error {
	if it.Cursor == "" {
		return nil // ephemeral: nothing to ack
	}
	if err := e.c.Ack(ctx, it.Cursor); err != nil {
		e.log.Debug("chat: ack", "err", err)
		return err
	}
	return nil
}

// process handles one item. The caller acks it.
func (e *Engine) process(ctx context.Context, it *client.Item) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := it.Notice; n != nil {
		return e.notice(ctx, n)
	}
	d := it.Delivery
	switch d.Kind {
	case "link":
		return e.linkBundle(ctx, d)
	case "mls":
	default:
		return nil
	}
	switch d.MLS {
	case "welcome":
		return e.welcome(ctx, d)
	case "commit", "app", "proposal":
		return e.groupMessage(ctx, d, it.TS)
	}
	return fmt.Errorf("chat: a delivery of mls kind %q", d.MLS)
}

// Contacts returns the contact list and requests, sorted by handle.
func (e *Engine) Contacts() []Contact {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Contact, 0, len(e.st.Contacts))
	for _, c := range e.st.Contacts {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// Chats returns every chat this device follows.
func (e *Engine) Chats() []Chat {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Chat, 0, len(e.st.Chats))
	for _, c := range e.st.Chats {
		out = append(out, c.public())
	}
	return out
}

// Chat returns one chat.
func (e *Engine) Chat(id string) (Chat, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.st.Chats[id]
	if !ok {
		return Chat{}, false
	}
	return c.public(), true
}

// public is a copy of a chat to hand out.
func (c *Chat) public() Chat {
	p := *c
	p.Members = slices.Clone(c.Members)
	p.Applied = maps.Clone(c.Applied)
	p.Admins = slices.Clone(c.Admins)
	p.Requests = slices.Clone(c.Requests)
	p.InVoice = slices.Clone(c.InVoice)
	p.Buffered = nil
	return p
}

// Messages returns a chat's local history, oldest first.
func (e *Engine) Messages(chat string) ([]Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.messages(chat)
}

func (e *Engine) messages(chat string) ([]Message, error) {
	return loadMessages(e.c.Profile(), recordID(chat))
}

// Handle returns an identity's handle as this device knows it, from its
// chain, or "" if it doesn't.
func (e *Engine) Handle(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.handleOf(id)
}

func (e *Engine) handleOf(id string) string {
	if id == e.c.ID() {
		return e.c.Handle()
	}
	return e.st.Handles[id]
}

// MarkRead clears a chat's unread count.
func (e *Engine) MarkRead(chat string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.st.Chats[chat]
	if ok && c.Kind == KindChannel {
		e.markChannelRead(context.Background(), c)
	}
	if !ok || c.Unread == 0 {
		return nil
	}
	c.Unread = 0
	if err := e.save(); err != nil {
		return err
	}
	e.emit(Event{Kind: EventChat, Chat: ptr(c.public())})
	return nil
}

func ptr[T any](v T) *T { return &v }

// profile returns the profile records live in.
func (e *Engine) profile() *store.Profile { return e.c.Profile() }
