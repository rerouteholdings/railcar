package ui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// drive runs a command and everything it leads to, feeding each message to
// the app, the way a Bubble Tea program would. Commands that wait, such as
// the event listener and the spinner, are left behind after a short time;
// events are drained from the backend directly instead.
func drive(a *App, b *DemoBackend, cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func() { done <- c() }()
		var msg tea.Msg
		select {
		case msg = <-done:
		case <-time.After(50 * time.Millisecond):
			continue
		}
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case eventMsg:
			queue = append(queue, a.handleEvent(msg.event))
		case eventsDoneMsg:
		default:
			if v := reflect.ValueOf(msg); v.Kind() == reflect.Slice && v.Type().Elem() == reflect.TypeOf(tea.Cmd(nil)) {
				for i := range v.Len() {
					queue = append(queue, v.Index(i).Interface().(tea.Cmd))
				}
				continue
			}
			_, next := a.Update(msg)
			queue = append(queue, next)
		}
		if len(queue) == 0 {
			queue = append(queue, drainEvents(a, b)...)
		}
	}
	for _, c := range drainEvents(a, b) {
		drive(a, b, c)
	}
}

// drainEvents applies the backend's queued events without waiting.
func drainEvents(a *App, b *DemoBackend) []tea.Cmd {
	var cmds []tea.Cmd
	for {
		select {
		case e, ok := <-b.Events():
			if !ok {
				return cmds
			}
			cmds = append(cmds, a.handleEvent(e))
		default:
			return cmds
		}
	}
}

// started returns a signed-in app at a size, with its chats loaded.
func started(t *testing.T, w, h int) (*App, *DemoBackend) {
	t.Helper()
	a, b := newTestApp(t, false)
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drive(a, b, call(func(ctx context.Context) tea.Msg {
		id, err := b.Open(ctx)
		return openedMsg{id, err}
	}))
	if a.screen != screenMain {
		t.Fatalf("screen = %v, want the main view", a.screen)
	}
	return a, b
}

func send(a *App, b *DemoBackend, msgs ...tea.Msg) {
	for _, m := range msgs {
		_, cmd := a.Update(m)
		drive(a, b, cmd)
	}
}

func typeText(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

func plain(a *App) string { return ansi.Strip(a.View().Content) }

func TestStartsAtOnboardingWithoutIdentity(t *testing.T) {
	a, b := newTestApp(t, true)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drive(a, b, a.Init())
	if a.screen != screenOnboarding {
		t.Fatalf("screen = %v, want onboarding", a.screen)
	}
}

func TestOpensFirstChatAndClearsUnread(t *testing.T) {
	a, _ := started(t, 120, 40)
	m := a.main
	if m.active != "ravi" {
		t.Fatalf("active chat = %q, want ravi, the most recent", m.active)
	}
	c, _ := m.activeChat()
	if c.Unread != 0 {
		t.Errorf("open chat shows %d unread, want 0", c.Unread)
	}
	if a.stream != StreamConnected {
		t.Errorf("stream = %v, want connected", a.stream)
	}
	if got := len(m.messages["ravi"]); got != 6 {
		t.Errorf("loaded %d messages, want 6", got)
	}
}

// Chats come newest first, then each community's channels in their order.
func TestChatsSortedByActivity(t *testing.T) {
	a, _ := started(t, 120, 40)
	var got []string
	for _, c := range a.main.chats {
		got = append(got, c.ID)
	}
	want := []string{"ravi", "trail", "oskar", "mia", "books", "noor", "c-general", "c-notices", "c-lounge"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestMovingSelectionOpensChat(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown})
	if a.main.active != "trail" {
		t.Fatalf("active = %q, want trail", a.main.active)
	}
	if !strings.Contains(plain(a), "Who has the water filter?") {
		t.Error("the Trail crew messages aren't showing")
	}
}

func TestFocusCycle(t *testing.T) {
	a, b := started(t, 120, 40)
	want := []focus{focusMessages, focusComposer, focusChats}
	for _, f := range want {
		send(a, b, tea.KeyPressMsg{Code: tea.KeyTab})
		if a.main.focus != f {
			t.Fatalf("focus = %v, want %v", a.main.focus, f)
		}
	}
}

func TestSendAndNewline(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter}) // open, into the composer
	send(a, b, typeText("first line")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	send(a, b, typeText("second line")...)
	if got := a.main.composer.Value(); got != "first line\nsecond line" {
		t.Fatalf("composer = %q", got)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.composer.Value() != "" {
		t.Error("the composer isn't empty after sending")
	}
	msgs := a.main.messages["ravi"]
	last := msgs[len(msgs)-1]
	if last.Text != "first line\nsecond line" || !last.Mine {
		t.Fatalf("last message = %+v", last)
	}
	if last.Status != StatusSent {
		t.Errorf("status = %v, want sent", last.Status)
	}
	if a.main.chats[0].ID != "ravi" || a.main.chats[0].Preview != "first line second line" {
		t.Errorf("chat list top = %+v", a.main.chats[0])
	}
}

func TestEmptyMessageIsNotSent(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	before := len(a.main.messages["ravi"])
	send(a, b, typeText("   ")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := len(a.main.messages["ravi"]); got != before {
		t.Errorf("messages went from %d to %d", before, got)
	}
}

func TestDraftsKeptPerChat(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("half a thought")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
	if a.main.active != "trail" || a.main.composer.Value() != "" {
		t.Fatalf("active = %q, composer = %q", a.main.active, a.main.composer.Value())
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt})
	if a.main.composer.Value() != "half a thought" {
		t.Errorf("composer = %q, want the draft back", a.main.composer.Value())
	}
}

func TestIncomingMessageInOtherChatCountsUnread(t *testing.T) {
	a, _ := started(t, 120, 40)
	c := Chat{ID: "mia", Kind: DirectChat, Title: "Mia Lund", Unread: 1, Preview: "Are you free?", LastActivity: testNow}
	a.handleEvent(ChatChanged{Chat: c})
	if a.main.chats[0].ID != "mia" || a.main.chats[0].Unread != 1 {
		t.Errorf("top chat = %+v", a.main.chats[0])
	}
	if a.main.active != "ravi" {
		t.Errorf("active changed to %q", a.main.active)
	}
}

func TestStreamStates(t *testing.T) {
	a, _ := started(t, 120, 40)
	for _, tc := range []struct {
		state StreamState
		want  string
	}{
		{StreamReconnecting, "Reconnecting"},
		{StreamOffline, "Offline"},
		{StreamConnected, "Connected"},
	} {
		a.handleEvent(StreamChanged{State: tc.state, Err: errors.New("closed")})
		lines := strings.Split(plain(a), "\n")
		if status := lines[len(lines)-1]; !strings.Contains(status, tc.want) {
			t.Errorf("%v: status bar = %q", tc.state, status)
		}
	}
}

func TestNarrowShowsOnePane(t *testing.T) {
	a, b := started(t, 70, 30)
	if a.main.active != "" {
		t.Fatalf("narrow layout opened %q on its own", a.main.active)
	}
	if !a.main.layout.narrow || a.main.layout.conv.w != 0 {
		t.Fatalf("layout = %+v", a.main.layout)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.layout.chats.w != 0 || a.main.layout.conv.w != 70 {
		t.Fatalf("after opening, layout = %+v", a.main.layout)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.main.focus != focusChats || a.main.layout.chats.w != 70 {
		t.Fatalf("after esc, focus = %v, layout = %+v", a.main.focus, a.main.layout)
	}
}

func TestDetailsToggle(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if a.main.layout.details.w == 0 {
		t.Fatal("details pane isn't showing")
	}
	if !strings.Contains(plain(a), "@ravi.chat.example.com") || !strings.Contains(plain(a), "Direct chat") {
		t.Error("details pane is missing the contact")
	}
	send(a, b, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if a.main.layout.details.w != 0 {
		t.Error("details pane didn't close")
	}
}

func TestFilterChats(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: '/', Text: "/"})
	send(a, b, typeText("book")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.active != "books" {
		t.Errorf("active = %q, want books", a.main.active)
	}
	if a.main.focus != focusComposer {
		t.Errorf("focus = %v, want the composer", a.main.focus)
	}
}

func TestPaletteCommands(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: ':', Text: ":"})
	if a.main.overlay != overlayPalette {
		t.Fatal("the palette didn't open")
	}
	send(a, b, typeText("open noor")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayNone || a.main.active != "noor" {
		t.Fatalf("overlay = %v, active = %q", a.main.overlay, a.main.active)
	}

	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape}) // the composer to the messages
	send(a, b, tea.KeyPressMsg{Code: ':', Text: ":"})
	send(a, b, typeText("help")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayHelp || !strings.Contains(plain(a), "Press Esc to close.") {
		t.Fatalf("help isn't showing")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})

	_, cmd := a.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	drive(a, b, cmd)
	send(a, b, typeText("q")...)
	_, cmd = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || cmd() != tea.Quit() {
		t.Error(":q didn't quit")
	}
}

func TestSignOutReturnsToOnboarding(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	send(a, b, typeText("sign out")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlaySignOut || !strings.Contains(plain(a), "Sign out of @kat.chat.example.com?") {
		t.Fatal("the confirmation isn't showing")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.screen != screenOnboarding {
		t.Fatalf("screen = %v, want onboarding", a.screen)
	}
	if _, err := b.Open(t.Context()); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("Open after sign-out = %v", err)
	}
}

func TestMouseClickOpensChat(t *testing.T) {
	a, b := started(t, 120, 40)
	// Rows: the border, the title, then 3 rows per chat. Book club is fifth.
	send(a, b, tea.MouseClickMsg{X: 5, Y: 2 + 4*3, Button: tea.MouseLeft})
	if a.main.active != "books" {
		t.Errorf("active = %q, want books", a.main.active)
	}
	send(a, b, tea.MouseClickMsg{X: 60, Y: 37, Button: tea.MouseLeft})
	if a.main.focus != focusComposer {
		t.Errorf("focus = %v, want the composer", a.main.focus)
	}
}

func TestMouseWheelScrolls(t *testing.T) {
	a, b := started(t, 120, 12)
	if !a.main.vp.AtBottom() {
		t.Fatal("the messages don't start at the latest")
	}
	send(a, b, tea.MouseWheelMsg{X: 60, Y: 5, Button: tea.MouseWheelUp})
	if a.main.vp.AtBottom() {
		t.Error("scrolling up didn't move the messages")
	}
}

func TestSignUpAsksForWordsBack(t *testing.T) {
	a, b := newTestApp(t, true)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 34})
	drive(a, b, a.Init())
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter}) // Create an identity
	send(a, b, typeText("chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("@Ravi.chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("RC-4821")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	o := a.onboarding
	if o.step != stepWords || len(o.words) != 12 {
		t.Fatalf("step = %v, %d words", o.step, len(o.words))
	}
	words := append([]string(nil), o.words...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter}) // I've written them down
	if o.step != stepConfirm || len(o.positions) != 3 {
		t.Fatalf("step = %v, positions = %v", o.step, o.positions)
	}

	// A wrong word keeps the form where it is.
	send(a, b, typeText("wrong")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if o.step != stepConfirm || !strings.Contains(plain(a), "That isn't word") {
		t.Fatalf("a wrong word was accepted, step = %v", o.step)
	}
	for range len("wrong") {
		send(a, b, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	// Esc goes back to the words, and the same positions aren't required.
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	if o.step != stepWords {
		t.Fatalf("esc didn't show the words again, step = %v", o.step)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	for _, pos := range o.positions {
		// Any case, with spaces around it.
		send(a, b, typeText(" "+strings.ToUpper(words[pos])+" ")...)
		send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	if a.screen != screenMain {
		t.Fatalf("screen = %v, want the main view", a.screen)
	}
	if a.identity.Handle != "ravi.chat.example.com" {
		t.Errorf("handle = %q", a.identity.Handle)
	}
}

func TestRecover(t *testing.T) {
	a, b := newTestApp(t, true)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drive(a, b, a.Init())
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.onboarding.step != stepRecover {
		t.Fatalf("step = %v", a.onboarding.step)
	}
	send(a, b, typeText("kat.chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("too few words")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.screen != screenOnboarding || !strings.Contains(plain(a), "12 words") {
		t.Fatal("3 words were accepted")
	}
	for range len("too few words") {
		send(a, b, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	send(a, b, typeText(strings.Repeat("orbit ", 12))...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.screen != screenMain {
		t.Fatalf("screen = %v, want the main view", a.screen)
	}
}

func TestCheckHandle(t *testing.T) {
	for h, ok := range map[string]bool{
		"kat.chat.example.com":                   true,
		"example.org":                            true,
		"kat":                                    false,
		"-kat.example.com":                       false,
		"kat..example.com":                       false,
		"kat_1.example.com":                      false,
		strings.Repeat("a", 64) + ".example.com": false,
	} {
		if err := checkHandle(h); (err == nil) != ok {
			t.Errorf("checkHandle(%q) = %v", h, err)
		}
	}
	if got := normalizeHandle("  @Kat.Example.COM "); got != "kat.example.com" {
		t.Errorf("normalizeHandle = %q", got)
	}
}

func TestCheckHomeserver(t *testing.T) {
	for s, ok := range map[string]bool{
		"chat.example.com":      true,
		"chat.example.com:8443": true,
		"localhost:8080":        true,
		"chat":                  false,
		"chat.example.com:0":    false,
	} {
		if err := checkHomeserver(s); (err == nil) != ok {
			t.Errorf("checkHomeserver(%q) = %v", s, err)
		}
	}
	if got := normalizeHomeserver("https://Chat.Example.com/"); got != "chat.example.com" {
		t.Errorf("normalizeHomeserver = %q", got)
	}
}

func TestTimeLabels(t *testing.T) {
	loc := time.UTC
	for _, tc := range []struct {
		at        time.Time
		list, day string
	}{
		{testNow.Add(-time.Hour), "2:30 PM", "Today"},
		{testNow.AddDate(0, 0, -1), "Yesterday", "Yesterday"},
		{testNow.AddDate(0, 0, -3), "Sun", "Sunday, November 15"},
		{testNow.AddDate(0, 0, -20), "Oct 29", "Thursday, October 29"},
		{testNow.AddDate(-1, 0, 0), "Nov 18, 2025", "Tuesday, November 18, 2025"},
	} {
		if got := listTime(tc.at, testNow, loc); got != tc.list {
			t.Errorf("listTime(%v) = %q, want %q", tc.at, got, tc.list)
		}
		if got := dayLabel(tc.at, testNow, loc); got != tc.day {
			t.Errorf("dayLabel(%v) = %q, want %q", tc.at, got, tc.day)
		}
	}
}

func TestMessageGrouping(t *testing.T) {
	th := newTheme(true, false)
	ravi := Member{Name: "Ravi Shah", Handle: "ravi.chat.example.com"}
	msgs := []Message{
		{ID: "1", Sender: ravi, Sent: testNow.Add(-10 * time.Minute), Text: "one"},
		{ID: "2", Sender: ravi, Sent: testNow.Add(-9 * time.Minute), Text: "two", Edited: true},
		{ID: "3", Sender: ravi, Sent: testNow.Add(-1 * time.Minute), Text: "three", ReplyTo: "1"},
		{ID: "4", Mine: true, Sent: testNow, Text: "four", Status: StatusSending},
	}
	got := ansi.Strip(renderMessages(th, msgs, 60, testNow, time.UTC))
	for _, want := range []string{"Today", "two (edited)", "↳ Replying to Ravi Shah: one", "four Sending…", "You  3:30 PM"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Messages 1 and 2 share a name; 3 comes after a pause, so it gets its own.
	if n := strings.Count(got, "Ravi Shah  "); n != 2 {
		t.Errorf("Ravi's name shows %d times, want 2:\n%s", n, got)
	}
}

func TestNoColorUsesShapes(t *testing.T) {
	th := newTheme(true, true)
	if th.paneFocused.GetBorderStyle() == th.pane.GetBorderStyle() {
		t.Error("without color, the focused pane looks like the others")
	}
}

func TestSignInWithLinkCode(t *testing.T) {
	a, b := newTestApp(t, true)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	drive(a, b, a.Init())
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.onboarding.step != stepLinkHow {
		t.Fatalf("step = %v", a.onboarding.step)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("kat.chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	o := a.onboarding
	if o.step != stepLinkCode {
		t.Fatalf("step = %v", o.step)
	}
	screen := plain(a)
	for _, want := range []string{"link-", "@chat.example.com", "Confirmation code", "Never read it to anyone", "█"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the link code screen doesn't show %q", want)
		}
	}
	send(a, b, typeText("7k2m-9qx")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(plain(a), "8 letters and digits") {
		t.Error("a short confirmation code was taken")
	}
	send(a, b, typeText("d")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if o.step != stepLinkWait {
		t.Fatalf("step = %v", o.step)
	}
	send(a, b, linkTickMsg{o.linkGen})
	if a.screen != screenMain || a.identity.Handle != "kat.chat.example.com" {
		t.Fatalf("screen = %v, identity = %+v", a.screen, a.identity)
	}
}

func TestSignInWithApprovalCode(t *testing.T) {
	a, b := newTestApp(t, true)
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	drive(a, b, a.Init())
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.onboarding.step != stepLinkEnter {
		t.Fatalf("step = %v", a.onboarding.step)
	}
	send(a, b, typeText("approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@ravi.example.org@chat.example.org")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	o := a.onboarding
	if o.step != stepLinkWait || !strings.Contains(plain(a), "Waiting for the other device") {
		t.Fatalf("step = %v", o.step)
	}
	// Esc gives up and goes back.
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	if o.step != stepLinkHow {
		t.Fatalf("step = %v", o.step)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, typeText("approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@ravi.example.org@chat.example.org")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	send(a, b, linkTickMsg{o.linkGen})
	if a.screen != screenMain || a.identity.Handle != "ravi.example.org" {
		t.Fatalf("screen = %v, identity = %+v", a.screen, a.identity)
	}
}

func TestAddDevice(t *testing.T) {
	a, b := started(t, 120, 40)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	send(a, b, tea.KeyPressMsg{Code: ':', Text: ":"})
	send(a, b, typeText("add-device link-aif5-ij2e@chat.example.com")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayAddDevice || !strings.Contains(plain(a), "Only enter a code shown") {
		t.Fatalf("overlay = %v", a.main.overlay)
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyTab})
	send(a, b, typeText("Kat's tablet")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	screen := plain(a)
	for _, want := range []string{"7K2M-9QXD", "Only continue if you're", "Never read it"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the confirmation step doesn't show %q", want)
		}
	}
	send(a, b, approvalTickMsg{a.main.dev.gen})
	if a.main.overlay != overlayNone || !strings.Contains(a.main.notice, "Kat's tablet") {
		t.Fatalf("overlay = %v, notice = %q", a.main.overlay, a.main.notice)
	}

	// The device list has it, and removes it.
	send(a, b, tea.KeyPressMsg{Code: ':', Text: ":"})
	send(a, b, typeText("devices")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayDevices || !strings.Contains(plain(a), "Kat's tablet") || !strings.Contains(plain(a), "(this device)") {
		t.Fatalf("the device list: %s", plain(a))
	}
	send(a, b, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if a.main.dev.removing {
		t.Error("the list offered to remove this device")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if !strings.Contains(plain(a), "Remove Kat's tablet?") {
		t.Fatal("no question before removing")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if strings.Contains(plain(a), "Kat's tablet") {
		t.Error("the device is still listed")
	}
}

func TestShowApprovalCode(t *testing.T) {
	a, b := started(t, 120, 50)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEscape})
	send(a, b, tea.KeyPressMsg{Code: ':', Text: ":"})
	send(a, b, typeText("approval-code")...)
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.main.overlay != overlayApprovalCode || strings.Contains(plain(a), "approve-") {
		t.Fatal("the code showed before the warning")
	}
	if !strings.Contains(plain(a), "Only continue if you're") {
		t.Error("no warning")
	}
	send(a, b, tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(plain(a), "approve-ljnf") || !strings.Contains(plain(a), "█") {
		t.Fatalf("no code: %s", plain(a))
	}
	send(a, b, approvalTickMsg{a.main.dev.gen})
	if a.main.overlay != overlayNone || !strings.Contains(a.main.notice, "New device") {
		t.Fatalf("overlay = %v, notice = %q", a.main.overlay, a.main.notice)
	}
}

func TestQRCodeShape(t *testing.T) {
	for _, dark := range []bool{true, false} {
		q := renderQR(demoLink, dark)
		lines := strings.Split(q, "\n")
		cols, rows := qrSize(demoLink)
		if len(lines) != rows {
			t.Fatalf("%d rows, want %d", len(lines), rows)
		}
		for _, l := range lines {
			if n := len([]rune(l)); n != cols {
				t.Fatalf("a row of %d, want %d", n, cols)
			}
		}
		// The quiet zone is light: drawn on a dark terminal, blank on a
		// light one.
		if first := []rune(lines[0])[0]; (first != ' ') != dark {
			t.Errorf("dark=%v: the corner is %q", dark, first)
		}
	}
}
