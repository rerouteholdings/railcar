// Package app connects Railcar's interface to the protocol: Backend
// implements ui.Backend with the Tieroute client and the chat layer, for
// one profile directory.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// Options configure a Backend.
type Options struct {
	// ProfileDir is the profile's directory.
	ProfileDir string
	// Client configures the protocol client: the device's name, local test
	// networks, and how the profile's key is kept.
	Client client.Options
	// Logger receives debug lines. Nil means none.
	Logger *slog.Logger
}

// Backend is the real ui.Backend.
type Backend struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	c      *client.Client
	eng    *chat.Engine
	signup *client.Signup
	req    ui.SignUpRequest // what the pending signup was started with

	// Linking: this device signing in with another's help, or this device
	// adding a new one, by its link code or by an approval code (§7.2).
	link           *client.Link
	approval       *client.LinkApproval
	approvalSecret *client.ApprovalSecret
	approvalName   string

	runCancel context.CancelFunc
	runDone   chan struct{}
	pumpDone  chan struct{}

	evMu   sync.Mutex
	events chan ui.Event
	closed bool
}

var _ ui.Backend = (*Backend)(nil)

// New returns a backend for a profile directory. Nothing is opened until
// the interface calls Open.
func New(opts Options) *Backend {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.Client.Logger == nil {
		opts.Client.Logger = opts.Logger
	}
	return &Backend{opts: opts, log: log, events: make(chan ui.Event, 256)}
}

func (b *Backend) emit(e ui.Event) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if b.closed {
		return
	}
	select {
	case b.events <- e:
	default:
		b.log.Debug("app: an interface event was dropped")
	}
}

// Events implements ui.Backend.
func (b *Backend) Events() <-chan ui.Event { return b.events }

// Open implements ui.Backend.
func (b *Backend) Open(ctx context.Context) (ui.Identity, error) {
	if !store.Exists(b.opts.ProfileDir) {
		return ui.Identity{}, ui.ErrNoIdentity
	}
	c, err := client.Open(b.opts.ProfileDir, b.opts.Client)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoProfile) {
		// A signup that never finished registering leaves a profile
		// without an identity.
		return ui.Identity{}, ui.ErrNoIdentity
	}
	if err != nil {
		return ui.Identity{}, err
	}
	return b.start(c)
}

// start runs the chat layer for a client and returns the identity.
func (b *Backend) start(c *client.Client) (ui.Identity, error) {
	eng, err := chat.New(c, chat.Options{Logger: b.opts.Logger})
	if err != nil {
		return ui.Identity{}, err
	}
	b.mu.Lock()
	b.c, b.eng = c, eng
	b.pumpDone = make(chan struct{})
	b.mu.Unlock()
	go b.pump(eng, b.pumpDone)
	return identityOf(eng, c), nil
}

// engine returns the running chat layer, or an error when signed out.
func (b *Backend) engine() (*chat.Engine, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eng == nil {
		return nil, errors.New("You're not signed in")
	}
	return b.eng, nil
}

// errRefused is a registration the homeserver refused: a taken handle, or
// a registration code it didn't take. The interface goes back to the
// form, and the recovery words stay the same.
type errRefused struct{ msg string }

func (e errRefused) Error() string        { return e.msg }
func (e errRefused) Is(target error) bool { return target == ui.ErrSignUpRefused }

// SignUp implements ui.Backend.
//
// The protocol client signs up in two steps: NewSignup makes the device
// key and the recovery words, and Register registers them. The interface
// shows the words and asks for some back between SignUp and FinishSignUp,
// so SignUp runs NewSignup and FinishSignUp runs Register.
//
// The trade-off: a taken handle or a wrong registration code shows up only
// after the user has typed the words back. Registering in SignUp would
// catch those first, but it would leave a registered identity whose words
// the user may never have written down, if they quit at the words screen;
// §6.1 asks clients to have the words typed back, and Railcar does that
// before anything is registered. NewSignup still checks the homeserver
// early: that it's reachable, speaks wire version 2, and isn't closed.
// After a refusal, SignUp with the same homeserver keeps the same words,
// so the user doesn't write down a second set.
func (b *Backend) SignUp(ctx context.Context, req ui.SignUpRequest) (ui.Identity, []string, error) {
	b.mu.Lock()
	s, prev := b.signup, b.req
	b.mu.Unlock()
	if s == nil || prev.Homeserver != req.Homeserver {
		if s != nil {
			s.Abort()
		}
		var err error
		s, err = client.NewSignup(ctx, req.Homeserver, b.opts.Client)
		if err != nil {
			return ui.Identity{}, nil, friendly(err)
		}
	}
	b.mu.Lock()
	b.signup, b.req = s, req
	b.mu.Unlock()
	handle := req.Handle
	if !strings.Contains(handle, ".") {
		handle += "." + req.Homeserver
	}
	return ui.Identity{Handle: handle, Homeserver: req.Homeserver}, s.Words().Words(), nil
}

// FinishSignUp implements ui.Backend: it registers the identity (§8.2),
// then starts the chat layer and uploads key packages.
func (b *Backend) FinishSignUp(ctx context.Context) error {
	b.mu.Lock()
	s, req := b.signup, b.req
	b.mu.Unlock()
	if s == nil {
		return errors.New("There's no signup to finish")
	}
	c, err := s.Register(ctx, b.opts.ProfileDir, req.Handle, req.RegistrationCode)
	switch {
	case wire.IsCode(err, wire.CodeHandleTaken):
		return errRefused{"That handle is taken. Pick another one."}
	case wire.IsCode(err, wire.CodeBadInvite):
		return errRefused{"The homeserver didn't take that registration code."}
	case c == nil && err != nil:
		return friendly(err)
	case err != nil:
		// Registered; only storing the recovery wrap failed. The client
		// stores it at the next chance.
		b.log.Info("app: after registering", "err", err)
	}
	b.mu.Lock()
	b.signup = nil
	b.mu.Unlock()
	if _, err := b.start(c); err != nil {
		return err
	}
	return nil
}

// Recover implements ui.Backend: it signs in with the recovery code, which
// adds this device and removes every other one with a recovery-signed
// entry (§6.4.1, client API, Recover an identity). Then it takes the
// contact list from the homeserver, and uploads key packages, so contacts
// add this device to their chats with them (§11.9).
func (b *Backend) Recover(ctx context.Context, req ui.RecoverRequest) (ui.Identity, error) {
	b.clearLeftover()
	c, err := client.Recover(ctx, req.Homeserver, req.Handle, req.Words, b.opts.ProfileDir, b.opts.Client)
	switch {
	case errors.Is(err, identity.ErrNotCurrentCode):
		return ui.Identity{}, errors.New("Those words aren't this identity's current recovery code")
	case errors.Is(err, identity.ErrWords):
		return ui.Identity{}, errors.New("Those aren't recovery words: check each word and their order")
	case errors.Is(err, client.ErrNoRecoveryWrap):
		return ui.Identity{}, errors.New("You're signed in, but the homeserver has no copy of your account key for these words")
	case wire.IsCode(err, wire.CodeNotFound):
		return ui.Identity{}, errors.New("Nobody has that handle on that homeserver")
	case err != nil:
		return ui.Identity{}, friendly(err)
	}
	id, err := b.start(c)
	if err != nil {
		return ui.Identity{}, err
	}
	if eng, err := b.engine(); err == nil {
		if err := eng.ImportContacts(ctx); err != nil {
			b.log.Info("app: the contact list", "err", err)
		}
		if err := eng.EnsureKeyPackages(ctx); err != nil {
			b.log.Info("app: key packages", "err", err)
		}
	}
	return id, nil
}

// Connect implements ui.Backend: it runs the chat layer's live stream
// until Close or SignOut.
func (b *Backend) Connect(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.eng == nil {
		return errors.New("You're not signed in")
	}
	if b.runCancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.runCancel, b.runDone = cancel, done
	eng := b.eng
	b.emit(ui.StreamChanged{State: ui.StreamConnecting})
	go func() {
		defer close(done)
		if err := eng.Run(ctx); err != nil {
			b.log.Info("app: the stream stopped", "err", err)
		}
	}()
	return nil
}

// stop ends the stream and the chat layer. The caller doesn't hold b.mu.
func (b *Backend) stop() {
	b.mu.Lock()
	cancel, done, pump, eng := b.runCancel, b.runDone, b.pumpDone, b.eng
	b.runCancel, b.runDone, b.eng, b.c = nil, nil, nil, nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	if eng != nil {
		eng.Close()
		<-pump
	}
}

// pump turns the chat layer's events into the interface's.
func (b *Backend) pump(eng *chat.Engine, done chan struct{}) {
	defer close(done)
	me := eng.Client().ID()
	for ev := range eng.Events() {
		switch ev.Kind {
		case chat.EventStream:
			switch ev.Stream {
			case client.EventConnected:
				b.emit(ui.StreamChanged{State: ui.StreamConnected})
			case client.EventDisconnected:
				b.emit(ui.StreamChanged{State: ui.StreamReconnecting, Err: ev.Err})
			}
		case chat.EventStopped:
			switch {
			case errors.Is(ev.Err, client.ErrDeviceRemoved):
				b.emit(ui.SignedOut{Reason: "another of your devices removed this one."})
			case errors.Is(ev.Err, context.Canceled):
			default:
				b.emit(ui.StreamChanged{State: ui.StreamOffline, Err: ev.Err})
			}
		case chat.EventContact:
			b.contactEvent(eng, *ev.Contact)
		case chat.EventChat:
			b.emit(ui.ChatChanged{Chat: b.uiChat(eng, *ev.Chat)})
		case chat.EventChatRemoved:
			b.emit(ui.ChatRemoved{Chat: ev.ChatID})
		case chat.EventMessage:
			m := uiMessage(eng, me, ev.ChatID, *ev.Message)
			if ev.New {
				b.emit(ui.MessageAdded{Message: m})
			} else {
				b.emit(ui.MessageChanged{Message: m})
			}
		case chat.EventTyping:
			b.emit(ui.Typing{Chat: ev.ChatID, Who: member(eng, ev.Who), On: ev.On})
		case chat.EventPresence:
			b.presenceEvent(eng, ev.Who)
		}
	}
}

// requestPrefix starts the IDs of contact requests in the chat list.
const requestPrefix = "request:"

// contactEvent shows a contact change as a request entry in the chat list,
// or takes the entry away.
func (b *Backend) contactEvent(eng *chat.Engine, c chat.Contact) {
	if rc, ok := requestChat(c); ok {
		b.emit(ui.ChatChanged{Chat: rc})
		return
	}
	b.emit(ui.ChatRemoved{Chat: requestPrefix + c.ID})
}

// requestChat is a contact request's entry in the chat list.
func requestChat(c chat.Contact) (ui.Chat, bool) {
	rc := ui.Chat{
		ID: requestPrefix + c.ID, Title: "@" + c.Handle, Members: []ui.Member{{Handle: c.Handle}},
		LastActivity: time.Unix(c.Since, 0),
	}
	switch c.State {
	case chat.StatePendingIn:
		rc.Kind, rc.Preview, rc.Unread = ui.ContactRequest, "Wants to be your contact", 1
	case chat.StatePendingOut:
		rc.Kind, rc.Preview = ui.SentRequest, "Request sent"
	case chat.StateDeclined:
		rc.Kind, rc.Preview = ui.SentRequest, "Declined your request"
		rc.Stopped = "@" + c.Handle + " declined your request."
	default:
		return ui.Chat{}, false
	}
	return rc, true
}

// uiChat is a chat as the interface shows it.
func (b *Backend) uiChat(eng *chat.Engine, c chat.Chat) ui.Chat {
	me := eng.Client().ID()
	out := ui.Chat{
		ID: c.ID, Kind: ui.DirectChat, Unread: c.Unread, Preview: c.Preview,
		LastActivity: time.Unix(c.LastActivity, 0), Stopped: c.Stopped,
	}
	if c.Kind == chat.KindGroup {
		out.Kind = ui.GroupChat
		out.Topic, out.Admin = c.Topic, slices.Contains(c.Admins, me)
		for _, r := range c.Requests {
			out.Requests = append(out.Requests, ui.AddRequest{ID: r.ID, From: member(eng, r.From), Who: member(eng, r.Who)})
		}
	}
	if c.Kind == chat.KindChannel {
		return channelChat(eng, c, out)
	}
	for _, id := range c.Members {
		if id != me {
			m := member(eng, id)
			m.Admin = slices.Contains(c.Admins, id)
			out.Members = append(out.Members, m)
		}
	}
	if c.Kind == chat.KindGroup && c.Name != "" {
		out.Title = c.Name
	} else if c.Peer != "" {
		// The display name, when they have one; the handle shows under
		// it (§13.1).
		out.Title = "@" + eng.Handle(c.Peer)
		if p, ok := eng.ProfileOf(c.Peer); ok && p.Name != "" {
			out.Title = p.Name
		}
	} else {
		names := make([]string, 0, len(out.Members))
		for _, m := range out.Members {
			names = append(names, "@"+m.Handle)
		}
		out.Title = strings.Join(names, ", ")
	}
	if c.Pending && out.Stopped == "" {
		out.Stopped = "Waiting to check a change to this chat"
	}
	return out
}

// uiMessage is a message as the interface shows it.
func uiMessage(eng *chat.Engine, me, chatID string, m chat.Message) ui.Message {
	out := ui.Message{
		ID: m.ID, Chat: chatID, Sender: member(eng, m.From), Mine: m.From == me,
		Sent: time.Unix(m.TS, 0), Text: m.Body, ReplyTo: m.Reply, Edited: m.Edited,
		Deleted: m.Deleted, Unsupported: m.Unsupported != "", Read: len(m.ReadBy) > 0, System: m.System, Seq: m.Seq,
	}
	switch m.Status {
	case chat.Sending:
		out.Status = ui.StatusSending
	case chat.Failed:
		out.Status = ui.StatusFailed
	}
	emojis := make([]string, 0, len(m.Reactions))
	for e := range m.Reactions {
		emojis = append(emojis, e)
	}
	sort.Strings(emojis)
	for _, e := range emojis {
		who := m.Reactions[e]
		out.Reactions = append(out.Reactions, ui.Reaction{Emoji: e, Count: len(who), Mine: slices.Contains(who, me)})
	}
	return out
}

// Chats implements ui.Backend: every chat, and every contact request.
func (b *Backend) Chats(context.Context) ([]ui.Chat, error) {
	eng, err := b.engine()
	if err != nil {
		return nil, err
	}
	var out []ui.Chat
	for _, c := range eng.Chats() {
		out = append(out, b.uiChat(eng, c))
	}
	for _, c := range eng.Contacts() {
		if rc, ok := requestChat(c); ok {
			out = append(out, rc)
		}
	}
	return out, nil
}

// Messages implements ui.Backend.
func (b *Backend) Messages(_ context.Context, chatID, before string, limit int) ([]ui.Message, error) {
	eng, err := b.engine()
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(chatID, requestPrefix) {
		return nil, nil
	}
	msgs, err := eng.Messages(chatID)
	if err != nil {
		return nil, err
	}
	end := len(msgs)
	if before != "" {
		if i := slices.IndexFunc(msgs, func(m chat.Message) bool { return m.ID == before }); i >= 0 {
			end = i
		}
	}
	start := 0
	if limit > 0 && end-limit > 0 {
		start = end - limit
	}
	me := eng.Client().ID()
	out := make([]ui.Message, 0, end-start)
	for _, m := range msgs[start:end] {
		out = append(out, uiMessage(eng, me, chatID, m))
	}
	return out, nil
}

// Send implements ui.Backend.
func (b *Backend) Send(ctx context.Context, chatID string, d ui.Draft) (ui.Message, error) {
	eng, err := b.engine()
	if err != nil {
		return ui.Message{}, err
	}
	m, err := eng.SendText(ctx, chatID, d.Text, d.ReplyTo)
	if err != nil {
		if m.ID != "" {
			// It's in the history, marked as not sent.
			return uiMessage(eng, eng.Client().ID(), chatID, m), nil
		}
		return ui.Message{}, friendly(err)
	}
	return uiMessage(eng, eng.Client().ID(), chatID, m), nil
}

// Edit implements ui.Backend.
func (b *Backend) Edit(ctx context.Context, chatID, id, text string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	return friendly(eng.Edit(ctx, chatID, id, text))
}

// Delete implements ui.Backend.
func (b *Backend) Delete(ctx context.Context, chatID, id string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	return friendly(eng.Delete(ctx, chatID, id))
}

// React implements ui.Backend: a reaction the identity already made with
// that emoji is taken back.
func (b *Backend) React(ctx context.Context, chatID, id, emoji string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	msgs, err := eng.Messages(chatID)
	if err != nil {
		return err
	}
	remove := false
	if i := slices.IndexFunc(msgs, func(m chat.Message) bool { return m.ID == id }); i >= 0 {
		remove = slices.Contains(msgs[i].Reactions[emoji], eng.Client().ID())
	}
	return friendly(eng.React(ctx, chatID, id, emoji, remove))
}

// SetTyping implements ui.Backend.
func (b *Backend) SetTyping(ctx context.Context, chatID string, on bool) error {
	eng, err := b.engine()
	if err != nil || strings.HasPrefix(chatID, requestPrefix) {
		return err
	}
	return eng.SetTyping(ctx, chatID, on)
}

// MarkRead implements ui.Backend.
func (b *Backend) MarkRead(_ context.Context, chatID string) error {
	eng, err := b.engine()
	if err != nil || strings.HasPrefix(chatID, requestPrefix) {
		return err
	}
	return eng.MarkRead(chatID)
}

// MakeInvite implements ui.Backend.
func (b *Backend) MakeInvite(ctx context.Context) (string, error) {
	eng, err := b.engine()
	if err != nil {
		return "", err
	}
	code, err := eng.Invite(ctx)
	return code, friendly(err)
}

// AddContact implements ui.Backend.
func (b *Backend) AddContact(ctx context.Context, code string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	_, err = eng.Add(ctx, code)
	switch {
	case errors.Is(err, client.ErrUnknownCode):
		return errors.New("That isn't a contact code. It looks like @ravi.example.com::Kx82mPq1Za")
	case errors.Is(err, chat.ErrNeedsCode):
		return errors.New("Ask them for a code: a handle alone isn't enough to send a request")
	case errors.Is(err, client.ErrHandleMismatch):
		return errors.New("That handle is no longer valid")
	case wire.IsCode(err, wire.CodeNotFound):
		return errors.New("Nobody has that handle")
	}
	return friendly(err)
}

// AcceptRequest implements ui.Backend.
func (b *Backend) AcceptRequest(ctx context.Context, chatID string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	id, ok := strings.CutPrefix(chatID, requestPrefix)
	if !ok {
		return errors.New("That isn't a contact request")
	}
	_, err = eng.Accept(ctx, id)
	return friendly(err)
}

// DeclineRequest implements ui.Backend.
func (b *Backend) DeclineRequest(ctx context.Context, chatID string) error {
	eng, err := b.engine()
	if err != nil {
		return err
	}
	id, ok := strings.CutPrefix(chatID, requestPrefix)
	if !ok {
		return errors.New("That isn't a contact request")
	}
	return friendly(eng.Decline(ctx, id))
}

// SignOut implements ui.Backend: it ends the session on the homeserver and
// deletes the profile from this device. The device stays in the
// identity's chain; another device removes it.
func (b *Backend) SignOut(ctx context.Context) error {
	b.mu.Lock()
	c := b.c
	b.mu.Unlock()
	if c == nil {
		return nil
	}
	b.stop()
	if err := c.SignOut(ctx); err != nil {
		b.log.Info("app: signing out", "err", err)
	}
	if err := c.DeleteProfile(); err != nil {
		return err
	}
	b.emit(ui.StreamChanged{State: ui.StreamOffline})
	return nil
}

// Close implements ui.Backend.
func (b *Backend) Close() error {
	b.stop()
	b.mu.Lock()
	if b.signup != nil {
		b.signup.Abort()
		b.signup = nil
	}
	b.mu.Unlock()
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.events)
	}
	return nil
}

// friendly turns an error into a line for the interface: without the
// package prefixes, and with the homeserver's error codes in words.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	var we *wire.Error
	if errors.As(err, &we) {
		switch we.Code {
		case wire.CodeRateLimited:
			return errors.New("The homeserver is limiting requests. Try again later")
		case wire.CodeUnavailable:
			return errors.New("The homeserver can't be reached right now")
		case wire.CodeNotAllowed:
			return errors.New("The homeserver didn't allow that")
		}
	}
	var cw *client.ChainWarning
	if errors.As(err, &cw) {
		return errors.New("That identity's history doesn't match what this device saw before, so Railcar didn't trust it")
	}
	if errors.Is(err, store.ErrNoKeychain) {
		return errors.New("There's no system keychain to keep your keys in. Run railcar with --key-file to keep them in a file instead")
	}
	if errors.Is(err, client.ErrRegistrationClosed) {
		return errors.New("That homeserver isn't taking new identities")
	}
	s := err.Error()
	for _, p := range []string{"chat: ", "client: ", "discovery: ", "identity: ", "store: ", "wire: "} {
		s = strings.ReplaceAll(s, p, "")
	}
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	return fmt.Errorf("%s", s)
}
