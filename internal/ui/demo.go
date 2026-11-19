package ui

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
)

// DemoOptions configures the demo backend.
type DemoOptions struct {
	// Now is the clock. The demo's messages are placed relative to it. It
	// defaults to time.Now.
	Now func() time.Time
	// Location is the time zone for the made-up days. It defaults to
	// time.Local.
	Location *time.Location
	// Live turns on the timed parts of the demo: a short wait while the
	// stream connects, a dropped connection now and then, and replies to what
	// you send. Tests leave it off, so every event happens at once.
	Live bool
	// SignedOut starts the demo without an identity, at onboarding.
	SignedOut bool
	// Seed picks the recovery words, so tests get the same ones every time.
	Seed uint64
}

// DemoBackend is a Backend with made-up chats and no network. railcar --demo
// runs the interface against it, and the tests use it too.
type DemoBackend struct {
	opts DemoOptions

	mu       sync.Mutex
	identity *Identity
	pending  *Identity
	chats    map[string]*Chat
	messages map[string][]Message
	nextID   int
	closed   bool
	events   chan Event
	stop     chan struct{}
	// connStop ends the current connection's timed loop.
	connStop chan struct{}

	// Linking devices: the identity a sign-in is for, the device being
	// added, how often each was polled, and the identity's devices.
	linking      *Identity
	linkPolls    int
	approving    string
	approvePolls int
	devices      []Device
}

// The words the demo draws recovery codes from. They're from the BIP-39
// English list, but the demo's codes aren't real: it never checks the
// checksum.
var demoWords = []string{
	"orbit", "ladder", "fossil", "tunnel", "harbor", "velvet", "ripple",
	"canyon", "sketch", "lemon", "galaxy", "window", "copper", "meadow",
	"signal", "anchor", "basket", "lantern", "pepper", "summit",
}

// NewDemoBackend returns a demo backend signed in as Kat, unless
// opts.SignedOut is set.
func NewDemoBackend(opts DemoOptions) *DemoBackend {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	d := &DemoBackend{
		opts:     opts,
		chats:    map[string]*Chat{},
		messages: map[string][]Message{},
		events:   make(chan Event, 256),
		stop:     make(chan struct{}),
	}
	if !opts.SignedOut {
		id := demoKat
		d.identity = &id
	}
	d.seed()
	return d
}

var demoKat = Identity{
	Handle:      "kat.chat.example.com",
	Homeserver:  "chat.example.com",
	DisplayName: "Kat",
	Device:      "Railcar on this computer",
	Presence:    "online",
}

var (
	demoRavi = Member{Name: "Ravi Shah", Handle: "ravi.chat.example.com", Presence: "busy", Status: "Fixing the night bus timetable",
		Bio: "Transit planner. Ask me about headways.", Links: []string{"https://ravi.example.com/timetables"}, Picture: true}
	demoMia   = Member{Name: "Mia Lund", Handle: "mia.example.org", Presence: "available", Bio: "Book club organizer"}
	demoOskar = Member{Name: "Oskar Berg", Handle: "oskar.chat.example.net", Presence: "away", Status: "Back Monday"}
	demoNoor  = Member{Name: "Noor Haddad", Handle: "noor.example.net", Presence: "offline"}
)

// seed fills in the made-up chats.
func (d *DemoBackend) seed() {
	now := d.opts.Now().In(d.opts.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, d.opts.Location)
	day := func(daysAgo int, hour, minute int) time.Time {
		return today.AddDate(0, 0, -daysAgo).Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	ago := func(minutes int) time.Time { return now.Add(-time.Duration(minutes) * time.Minute) }
	me := Member{Name: demoKat.DisplayName, Handle: demoKat.Handle}

	type line struct {
		from    Member
		at      time.Time
		text    string
		replyTo int // index of an earlier line, plus one
		edited  bool
		files   []Attachment
	}
	add := func(chat Chat, unread int, lines []line) {
		ids := make([]string, len(lines))
		for i, l := range lines {
			d.nextID++
			ids[i] = fmt.Sprintf("m%d", d.nextID)
			m := Message{
				ID:     ids[i],
				Chat:   chat.ID,
				Sender: l.from,
				Mine:   l.from.Handle == me.Handle,
				Sent:   l.at,
				Text:   l.text,
				Edited: l.edited,
				Files:  l.files,
			}
			if l.replyTo > 0 {
				m.ReplyTo = ids[l.replyTo-1]
			}
			d.messages[chat.ID] = append(d.messages[chat.ID], m)
		}
		last := lines[len(lines)-1]
		chat.Unread = unread
		chat.LastActivity = last.at
		chat.Preview = previewOf(last.text)
		c := chat
		d.chats[chat.ID] = &c
	}

	add(Chat{ID: "ravi", Kind: DirectChat, Title: demoRavi.Name, Members: []Member{demoRavi}}, 2, []line{
		{from: demoRavi, at: day(1, 18, 40), text: "Are we still on for the ridge trail on Saturday?"},
		{from: me, at: day(1, 18, 42), text: "Yes. I'll bring the map and the spare headlamp."},
		{from: me, at: day(1, 18, 43), text: "Meet at the north lot at 7?"},
		{from: demoRavi, at: day(1, 19, 2), text: "7 works. Mia might come too."},
		{from: demoRavi, at: ago(25), text: "The forecast says clear until noon."},
		{from: demoRavi, at: ago(24), text: "Can you check that the trailhead is open? The park's site was down earlier.", replyTo: 3},
	})
	miaAdmin := demoMia
	miaAdmin.Admin = true
	add(Chat{ID: "trail", Kind: GroupChat, Title: "Trail crew", Topic: "Saturday's ridge trail", Admin: true,
		Members: []Member{demoRavi, miaAdmin, demoOskar}}, 3, []line{
		{from: demoMia, at: ago(190), text: "I made a packing list for Saturday:\nwater filter, first aid kit, two maps, and snacks for four."},
		{from: demoOskar, at: ago(186), text: "I can drive. The car fits four, with the packs in the back.", edited: true},
		{from: me, at: ago(180), text: "I'll ride with Oskar, then."},
		{from: demoRavi, at: ago(42), text: "Who has the water filter?"},
		{from: demoMia, at: ago(41), text: "I do. It needs a new cartridge, so I'll pick one up on Friday.", replyTo: 4},
		{from: demoMia, at: ago(40), text: "Anyone want anything else from the shop?"},
	})
	add(Chat{ID: "mia", Kind: DirectChat, Title: demoMia.Name, Members: []Member{demoMia}}, 0, []line{
		{from: demoMia, at: day(2, 9, 15), text: "Did you finish the chapter on signal boxes?"},
		{from: me, at: day(2, 9, 31), text: "Halfway. The part about tablet working is better than I expected."},
		{from: demoMia, at: day(2, 9, 32), files: []Attachment{{Name: "signal-box-diagram.png", MIME: "image/png", Size: 284211, Width: 1600, Height: 900}}},
		{from: demoMia, at: day(2, 9, 33), text: "Wait until you get to the tie lines."},
	})
	add(Chat{ID: "oskar", Kind: DirectChat, Title: demoOskar.Name, Members: []Member{demoOskar}}, 0, []line{
		{from: demoOskar, at: day(1, 8, 5), text: "Can I borrow your tent stakes? Mine went missing on the last trip."},
		{from: demoOskar, at: day(1, 8, 6), files: []Attachment{{Name: "voice-message.ogg", MIME: "audio/ogg", Size: 48213}}},
		{from: me, at: day(1, 8, 20), text: "Sure, I'll put them in your car on Saturday."},
	})
	add(Chat{ID: "books", Kind: GroupChat, Title: "Book club", Members: []Member{demoMia, demoNoor, demoOskar}}, 0, []line{
		{from: demoNoor, at: day(3, 20, 0), text: "Next month's pick is the history of the telephone exchange."},
		{from: demoOskar, at: day(3, 20, 4), text: "The one with the switchboard photos? I'm in."},
	})
	// Noor asked for read receipts, so opening the chat asks whether to
	// send them (§13.3).
	add(Chat{ID: "noor", Kind: DirectChat, Title: demoNoor.Name, Members: []Member{demoNoor}, ReceiptAsk: &demoNoor}, 0, []line{
		{from: demoNoor, at: day(8, 12, 30), text: "Thanks for the invite code. I'm set up now."},
	})

	// A community, with its channels in their order.
	owl := func(m Member, roles string) Member { m.Roles = roles; return m }
	night := []Member{owl(demoRavi, "Night owls"), owl(demoMia, ""), owl(demoNoor, ""), owl(demoOskar, "")}
	channel := func(id, title, topic string, order int) Chat {
		return Chat{ID: id, Kind: ChannelChat, Title: title, Topic: topic, Community: "Night shift",
			CommunityServer: "community.example.com", Order: order, Members: night, Admin: true}
	}
	add(channel("c-general", "general", "Anything goes", 0), 1, []line{
		{from: demoNoor, at: ago(300), text: "Who's on the late train tonight?"},
		{from: demoRavi, at: ago(290), text: "Me, from 11 to 3."},
		{from: me, at: ago(120), text: "Bring the radio, the one at the box is flaky."},
		{from: demoOskar, at: ago(15), text: "Coffee's on in the signal box."},
	})
	add(channel("c-notices", "announcements", "Only the owls post here", 1), 0, []line{
		{from: demoRavi, at: day(2, 22, 0), text: "Track work on line 4 all week. Expect delays."},
	})
	lounge := channel("c-lounge", "Lounge", "", 2)
	lounge.Kind, lounge.InVoice, lounge.LastActivity = VoiceChannel, []Member{demoRavi, demoMia}, ago(5)
	d.chats[lounge.ID] = &lounge
	n := 1
	for i := range d.messages["c-general"] {
		d.messages["c-general"][i].Seq = uint64(n)
		n++
	}
}

// previewOf turns message text into one line for the chat list.
func previewOf(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// emit queues an event unless the backend is closed. The caller holds d.mu.
func (d *DemoBackend) emit(e Event) {
	if d.closed {
		return
	}
	select {
	case d.events <- e:
	default:
		// The interface isn't reading. Dropping a demo event is harmless.
	}
}

// later runs f after a delay, or at once when the demo isn't live.
func (d *DemoBackend) later(delay time.Duration, f func()) {
	if !d.opts.Live {
		f()
		return
	}
	go func() {
		select {
		case <-time.After(delay):
			f()
		case <-d.stop:
		}
	}()
}

// Open implements Backend.
func (d *DemoBackend) Open(context.Context) (Identity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.identity == nil {
		return Identity{}, ErrNoIdentity
	}
	return *d.identity, nil
}

// SignUp implements Backend. The demo takes any registration code except an
// empty one.
func (d *DemoBackend) SignUp(_ context.Context, req SignUpRequest) (Identity, []string, error) {
	if strings.TrimSpace(req.RegistrationCode) == "" {
		return Identity{}, nil, errors.New("Enter the registration code from your homeserver")
	}
	r := rand.New(rand.NewPCG(d.opts.Seed, d.opts.Seed^0x9e3779b97f4a7c15))
	words := make([]string, 12)
	for i, j := range r.Perm(len(demoWords))[:12] {
		words[i] = demoWords[j]
	}
	name, _, _ := strings.Cut(req.Handle, ".")
	if name != "" {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	id := Identity{
		Handle:      req.Handle,
		Homeserver:  req.Homeserver,
		DisplayName: name,
		Device:      demoKat.Device,
	}
	d.mu.Lock()
	d.pending = &id
	d.mu.Unlock()
	return id, words, nil
}

// FinishSignUp implements Backend.
func (d *DemoBackend) FinishSignUp(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending == nil {
		return errors.New("There's no sign-up to finish")
	}
	d.identity, d.pending = d.pending, nil
	return nil
}

// Recover implements Backend. The demo takes any 12 words.
func (d *DemoBackend) Recover(_ context.Context, req RecoverRequest) (Identity, error) {
	if len(req.Words) != 12 {
		return Identity{}, errors.New("A recovery code has 12 words")
	}
	id := demoKat
	id.Handle, id.Homeserver = req.Handle, req.Homeserver
	d.mu.Lock()
	d.identity = &id
	d.mu.Unlock()
	return id, nil
}

// Connect implements Backend. When the demo is live, the stream takes a
// moment to connect, and drops every few minutes to show reconnecting.
func (d *DemoBackend) Connect(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.opts.Live {
		d.emit(StreamChanged{State: StreamConnected})
		return nil
	}
	d.emit(StreamChanged{State: StreamConnecting})
	d.endConnection()
	conn := make(chan struct{})
	d.connStop = conn
	go func() {
		wait := 900 * time.Millisecond
		for {
			select {
			case <-time.After(wait):
			case <-conn:
				return
			case <-d.stop:
				return
			}
			d.mu.Lock()
			d.emit(StreamChanged{State: StreamConnected})
			d.mu.Unlock()
			select {
			case <-time.After(3 * time.Minute):
			case <-conn:
				return
			case <-d.stop:
				return
			}
			d.mu.Lock()
			d.emit(StreamChanged{State: StreamReconnecting, Err: errors.New("The connection to the homeserver closed")})
			d.mu.Unlock()
			wait = 4 * time.Second
		}
	}()
	return nil
}

// endConnection stops the current connection's loop. The caller holds d.mu.
func (d *DemoBackend) endConnection() {
	if d.connStop != nil {
		close(d.connStop)
		d.connStop = nil
	}
}

// Events implements Backend.
func (d *DemoBackend) Events() <-chan Event { return d.events }

// Chats implements Backend.
func (d *DemoBackend) Chats(context.Context) ([]Chat, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Chat, 0, len(d.chats))
	for _, c := range d.chats {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Messages implements Backend.
func (d *DemoBackend) Messages(_ context.Context, chat, before string, limit int) ([]Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, ok := d.messages[chat]
	if !ok {
		if _, known := d.chats[chat]; !known {
			return nil, fmt.Errorf("There's no chat %q", chat)
		}
	}
	end := len(all)
	if before != "" {
		for i, m := range all {
			if m.ID == before {
				end = i
				break
			}
		}
	}
	start := 0
	if limit > 0 && end-limit > 0 {
		start = end - limit
	}
	return append([]Message(nil), all[start:end]...), nil
}

// Send implements Backend. In a direct chat, the contact answers a moment
// later when the demo is live.
func (d *DemoBackend) Send(_ context.Context, chat string, draft Draft) (Message, error) {
	d.mu.Lock()
	c, ok := d.chats[chat]
	if !ok {
		d.mu.Unlock()
		return Message{}, fmt.Errorf("There's no chat %q", chat)
	}
	d.nextID++
	m := Message{
		ID:      fmt.Sprintf("m%d", d.nextID),
		Chat:    chat,
		Sender:  Member{Name: d.displayName(), Handle: d.handle()},
		Mine:    true,
		Sent:    d.opts.Now(),
		Text:    draft.Text,
		ReplyTo: draft.ReplyTo,
		Status:  StatusSending,
	}
	d.messages[chat] = append(d.messages[chat], m)
	c.LastActivity = m.Sent
	c.Preview = previewOf(m.Text)
	d.emit(ChatChanged{Chat: *c})
	live := d.opts.Live
	d.mu.Unlock()

	d.later(350*time.Millisecond, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		sent := d.setStatus(chat, m.ID, StatusSent)
		d.emit(MessageChanged{Message: sent})
	})
	if c.Kind == DirectChat && live {
		contact := c.Members[0]
		d.later(2500*time.Millisecond, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.nextID++
			reply := Message{
				ID:     fmt.Sprintf("m%d", d.nextID),
				Chat:   chat,
				Sender: contact,
				Sent:   d.opts.Now(),
				Text:   demoReplies[d.nextID%len(demoReplies)],
			}
			d.messages[chat] = append(d.messages[chat], reply)
			c.Unread++
			c.LastActivity = reply.Sent
			c.Preview = previewOf(reply.Text)
			d.emit(MessageAdded{Message: reply})
			d.emit(ChatChanged{Chat: *c})
		})
	}
	return m, nil
}

var demoReplies = []string{
	"Sounds good.",
	"Got it, thanks.",
	"I'll check and let you know.",
	"See you then.",
}

// setStatus changes a message's status and returns it. The caller holds d.mu.
func (d *DemoBackend) setStatus(chat, id string, s MessageStatus) Message {
	for i := range d.messages[chat] {
		if d.messages[chat][i].ID == id {
			d.messages[chat][i].Status = s
			return d.messages[chat][i]
		}
	}
	return Message{}
}

func (d *DemoBackend) displayName() string {
	if d.identity == nil {
		return ""
	}
	return d.identity.DisplayName
}

func (d *DemoBackend) handle() string {
	if d.identity == nil {
		return ""
	}
	return d.identity.Handle
}

// MarkRead implements Backend.
func (d *DemoBackend) MarkRead(_ context.Context, chat string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.chats[chat]
	if !ok {
		return fmt.Errorf("There's no chat %q", chat)
	}
	if c.Unread != 0 {
		c.Unread = 0
		d.emit(ChatChanged{Chat: *c})
	}
	return nil
}

// SignOut implements Backend. The demo keeps its chats, so signing up again
// shows them.
func (d *DemoBackend) SignOut(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.identity = nil
	d.endConnection()
	d.emit(StreamChanged{State: StreamOffline})
	return nil
}

// Close implements Backend.
func (d *DemoBackend) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.endConnection()
		close(d.stop)
		close(d.events)
	}
	return nil
}
