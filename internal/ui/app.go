package ui

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Options configures the interface.
type Options struct {
	// Backend is the protocol side. It's required.
	Backend Backend
	// Now is the clock. It defaults to time.Now.
	Now func() time.Time
	// Location is the time zone for times and days. It defaults to
	// time.Local.
	Location *time.Location
	// Rand picks which recovery words to ask for back. It defaults to a
	// random source.
	Rand *rand.Rand
	// Demo marks the interface as running against made-up data, and says so
	// in the status bar.
	Demo bool
	// NoColor turns color cues into shapes, for NO_COLOR. It defaults to
	// whether NO_COLOR is set.
	NoColor *bool
}

// screen is the part of the interface that's showing.
type screen int

const (
	screenLoading screen = iota
	screenOnboarding
	screenMain
)

// App is the root Bubble Tea model. It's a pointer, and every Update
// changes it in place, because Huh forms keep pointers to its fields.
type App struct {
	opts  Options
	b     Backend
	keys  keyMap
	theme *theme

	width, height int
	screen        screen

	identity  Identity
	stream    StreamState
	streamErr error

	spinner  spinner.Model
	spinning bool

	onboarding *onboarding
	main       *mainView

	// fatal is an error that stops the interface from starting, such as a
	// profile it can't read.
	fatal string
}

// New returns the interface's root model. Run it with tea.NewProgram.
func New(opts Options) *App {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	noColor := noColorSet()
	if opts.NoColor != nil {
		noColor = *opts.NoColor
	}
	a := &App{
		opts:  opts,
		b:     opts.Backend,
		keys:  newKeyMap(),
		theme: newTheme(true, noColor),
	}
	a.spinner = spinner.New(spinner.WithSpinner(spinner.MiniDot))
	a.applyTheme()
	return a
}

// Messages from commands.
type (
	openedMsg struct {
		identity Identity
		err      error
	}
	eventMsg      struct{ event Event }
	eventsDoneMsg struct{}
	chatsMsg      struct {
		chats []Chat
		err   error
	}
	messagesMsg struct {
		chat     string
		messages []Message
		err      error
	}
	sentMsg struct {
		chat    string
		message Message
		err     error
	}
	signedOutMsg struct{ err error }
	// noticeMsg puts a short line in the status bar.
	noticeMsg struct{ text string }
)

// timeout bounds each backend call made from a command.
const timeout = 30 * time.Second

// call runs f with a timeout and turns its result into a message.
func call(f func(ctx context.Context) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return f(ctx)
	}
}

// listen waits for the backend's next event.
func (a *App) listen() tea.Cmd {
	ch := a.b.Events()
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsDoneMsg{}
		}
		return eventMsg{e}
	}
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd {
	return tea.Batch(
		tea.RequestBackgroundColor,
		a.listen(),
		call(func(ctx context.Context) tea.Msg {
			id, err := a.b.Open(ctx)
			return openedMsg{id, err}
		}),
	)
}

// applyTheme passes the theme to the parts that keep their own styles.
func (a *App) applyTheme() {
	a.spinner.Style = lipgloss.NewStyle().Foreground(a.theme.p.away)
	if a.main != nil {
		a.main.applyTheme(a.theme)
	}
	if a.onboarding != nil {
		a.onboarding.applyTheme(a)
	}
}

// startSpinner starts the spinner's ticks, if they aren't already going.
func (a *App) startSpinner() tea.Cmd {
	if a.spinning {
		return nil
	}
	a.spinning = true
	return a.spinner.Tick
}

// needsSpinner reports whether anything on screen is waiting.
func (a *App) needsSpinner() bool {
	if a.screen == screenOnboarding && a.onboarding != nil && a.onboarding.busy() {
		return true
	}
	if a.screen == screenMain {
		if m := a.main; m != nil && (m.overlay == overlayAddDevice || m.overlay == overlayApprovalCode) && m.dev.step == 1 {
			return true
		}
		return a.stream == StreamConnecting || a.stream == StreamReconnecting
	}
	return a.screen == screenLoading
}

// Update implements tea.Model.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		if dark := msg.IsDark(); dark != a.theme.dark {
			a.theme = newTheme(dark, a.theme.noColor)
			a.applyTheme()
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
	case spinner.TickMsg:
		if !a.needsSpinner() {
			a.spinning = false
			return a, nil
		}
		var cmd tea.Cmd
		a.spinner, cmd = a.spinner.Update(msg)
		return a, cmd
	case openedMsg:
		switch {
		case msg.err == nil:
			return a, a.enterMain(msg.identity)
		case errors.Is(msg.err, ErrNoIdentity):
			return a, a.enterOnboarding("")
		default:
			a.fatal = "Railcar couldn't open this profile: " + msg.err.Error()
			return a, nil
		}
	case eventsDoneMsg:
		return a, nil
	case eventMsg:
		return a, tea.Batch(a.handleEvent(msg.event), a.listen())
	case signedOutMsg:
		if msg.err != nil {
			return a, a.notice("Railcar couldn't sign out: " + msg.err.Error())
		}
		return a, a.enterOnboarding("You've signed out.")
	}

	switch a.screen {
	case screenOnboarding:
		return a, a.onboarding.update(a, msg)
	case screenMain:
		return a, a.main.update(a, msg)
	}
	return a, nil
}

// handleEvent applies one backend event.
func (a *App) handleEvent(e Event) tea.Cmd {
	switch e := e.(type) {
	case StreamChanged:
		a.stream, a.streamErr = e.State, e.Err
		if a.needsSpinner() {
			return a.startSpinner()
		}
		return nil
	case SignedOut:
		text := "This device was signed out."
		if e.Reason != "" {
			text = "This device was signed out: " + e.Reason
		}
		return a.enterOnboarding(text)
	}
	if a.main != nil {
		return a.main.handleEvent(a, e)
	}
	return nil
}

// enterOnboarding shows onboarding, with an optional note at the top.
func (a *App) enterOnboarding(note string) tea.Cmd {
	a.screen = screenOnboarding
	a.main = nil
	a.identity = Identity{}
	a.stream, a.streamErr = StreamOffline, nil
	a.onboarding = newOnboarding(a, note)
	return a.onboarding.init(a)
}

// enterMain shows the main view for a signed-in identity, connects the
// stream, and loads the chats.
func (a *App) enterMain(id Identity) tea.Cmd {
	a.screen = screenMain
	a.onboarding = nil
	a.identity = id
	a.stream = StreamConnecting
	a.main = newMainView(a)
	return tea.Batch(
		a.startSpinner(),
		a.main.init(a),
		call(func(ctx context.Context) tea.Msg {
			if err := a.b.Connect(ctx); err != nil {
				return eventMsg{StreamChanged{State: StreamOffline, Err: err}}
			}
			return nil
		}),
	)
}

// notice shows a short line in the status bar.
func (a *App) notice(text string) tea.Cmd {
	return func() tea.Msg { return noticeMsg{text} }
}

// View implements tea.Model.
func (a *App) View() tea.View {
	var content string
	switch {
	case a.fatal != "":
		content = lipgloss.NewStyle().Padding(1, 2).Render(a.theme.danger.Render(a.fatal) + "\n\n" +
			a.theme.muted.Render("Press Ctrl+C to quit."))
	case a.width == 0:
		content = ""
	case a.screen == screenOnboarding:
		content = a.onboarding.view(a)
	case a.screen == screenMain:
		content = a.main.view(a)
	default:
		content = lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Center,
			a.spinner.View()+" "+a.theme.muted.Render("Opening your profile"))
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Railcar"
	if a.theme.dark && !a.theme.noColor {
		v.BackgroundColor = a.theme.p.bg
	}
	return v
}
