package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// onboardStep is one screen of onboarding.
type onboardStep int

const (
	stepWelcome onboardStep = iota
	stepCreate
	stepRecover
	stepWords
	stepConfirm
	stepBusy
	stepLinkHow
	stepLinkForm
	stepLinkCode
	stepLinkEnter
	stepLinkWait
)

// onboarding signs in to an existing identity or creates one. Creating one
// ends by showing the 12 recovery words and asking for 3 of them back, as
// §6.1 asks of clients. Signing in with another device shows a link code,
// or takes an approval code, and waits for the other device (§7.2).
type onboarding struct {
	step onboardStep
	form *huh.Form

	// note is a line above the form, such as after signing out.
	note string
	// err is why the last step failed.
	err string
	// busyText says what's happening while the backend works.
	busyText string

	choice     string
	homeserver string
	handle     string
	code       string
	recHandle  string
	recWords   string
	recServer  string

	// Signing in with another device: which way round, the identity, the
	// link code shown, the codes typed, and which link a poll is for.
	linkHow    string
	linkHandle string
	linkServer string
	linkCode   string
	confirm    string
	approval   string
	linkGen    int

	identity  Identity
	words     []string
	positions []int
	answers   []string
	written   bool
	// confirmed holds words already typed back, when the homeserver then
	// refused the handle or code.
	confirmed []string
}

// Messages from onboarding's commands.
type (
	signedUpMsg struct {
		identity Identity
		words    []string
		err      error
	}
	recoveredMsg struct {
		identity Identity
		err      error
	}
	finishedMsg struct{ err error }
)

func newOnboarding(a *App, note string) *onboarding {
	o := &onboarding{note: note}
	o.toWelcome(a)
	return o
}

func (o *onboarding) init(*App) tea.Cmd { return o.form.Init() }

func (o *onboarding) busy() bool { return o.step == stepBusy || o.step == stepLinkWait }

// applyTheme rebuilds the current form with the new theme.
func (o *onboarding) applyTheme(a *App) {
	if o.form != nil {
		o.form.WithTheme(a.theme.huhTheme())
	}
}

// formWidth is the width of the onboarding column.
func formWidth(a *App) int {
	return max(20, min(a.width-4, 68))
}

// newForm wraps groups in a form with Railcar's theme and keys.
func newForm(a *App, groups ...*huh.Group) *huh.Form {
	km := huh.NewDefaultKeyMap()
	// Ctrl+C quits the whole app, so the form doesn't need its own quit key.
	km.Quit.SetEnabled(false)
	f := huh.NewForm(groups...).
		WithTheme(a.theme.huhTheme()).
		WithKeyMap(km).
		WithWidth(formWidth(a)).
		WithShowHelp(true)
	// The form sizes its groups from the window size, which arrived before
	// the form existed.
	if a.width > 0 {
		f.Update(tea.WindowSizeMsg{Width: a.width, Height: a.height})
	}
	return f
}

func (o *onboarding) toWelcome(a *App) {
	o.step = stepWelcome
	o.choice = "create"
	o.form = newForm(a, huh.NewGroup(
		huh.NewSelect[string]().
			Title("Welcome to Railcar").
			Description("Railcar is a terminal client for Tieroute, a federated, end-to-end encrypted chat protocol.\nWhat would you like to do?").
			Options(
				huh.NewOption("Create an identity", "create"),
				huh.NewOption("Sign in with another device", "link"),
				huh.NewOption("Sign in with your recovery code", "recover"),
			).
			Value(&o.choice),
	))
}

func (o *onboarding) toCreate(a *App) {
	o.step = stepCreate
	o.form = newForm(a, huh.NewGroup(
		huh.NewInput().
			Title("Homeserver").
			Description("The server that holds your identity and messages.").
			Placeholder("chat.example.com").
			Value(&o.homeserver).
			Validate(func(s string) error { return checkHomeserver(normalizeHomeserver(s)) }),
		huh.NewInput().
			Title("Handle").
			Description("The domain name people find you by, under your homeserver's domain or one you own.").
			Placeholder("kat.chat.example.com").
			Value(&o.handle).
			Validate(func(s string) error { return checkHandle(normalizeHandle(s)) }),
		huh.NewInput().
			Title("Registration code").
			Description("Your homeserver's operator gives you this code.").
			Value(&o.code).
			EchoMode(huh.EchoModePassword).
			Validate(checkRequired("the registration code")),
	).Title("Create an identity"))
}

func (o *onboarding) toRecover(a *App) {
	o.step = stepRecover
	o.form = newForm(a, huh.NewGroup(
		huh.NewInput().
			Title("Handle").
			Placeholder("kat.chat.example.com").
			Value(&o.recHandle).
			Validate(func(s string) error { return checkHandle(normalizeHandle(s)) }),
		huh.NewInput().
			Title("Homeserver").
			Description("The server that holds your identity.").
			Placeholder("chat.example.com").
			Value(&o.recServer).
			Validate(func(s string) error { return checkHomeserver(normalizeHomeserver(s)) }),
		huh.NewInput().
			Title("Recovery code").
			Description("The 12 words you wrote down when you created the identity, in order. This signs out every other device of the identity.").
			Value(&o.recWords).
			EchoMode(huh.EchoModePassword).
			Validate(checkRecoveryWords),
	).Title("Sign in with your recovery code"))
}

func (o *onboarding) toWords(a *App) {
	o.step = stepWords
	o.written = false
	o.form = newForm(a, huh.NewGroup(
		huh.NewConfirm().
			Title("Have you written down all 12 words?").
			Affirmative("I've written them down").
			Negative("").
			Value(&o.written),
	))
}

func (o *onboarding) toConfirm(a *App) {
	o.step = stepConfirm
	// Ask for 3 of the 12 words, chosen at random by position.
	o.positions = a.opts.Rand.Perm(len(o.words))[:3]
	slices.Sort(o.positions)
	o.answers = make([]string, len(o.positions))
	fields := make([]huh.Field, len(o.positions))
	for i, pos := range o.positions {
		want := o.words[pos]
		n := pos + 1
		fields[i] = huh.NewInput().
			Title(fmt.Sprintf("Word %d", n)).
			Value(&o.answers[i]).
			Validate(func(s string) error {
				if strings.EqualFold(strings.TrimSpace(s), want) {
					return nil
				}
				return fmt.Errorf("That isn't word %d. Check what you wrote down, or press Esc to see the words again", n)
			})
	}
	nums := make([]string, len(o.positions))
	for i, pos := range o.positions {
		nums[i] = fmt.Sprint(pos + 1)
	}
	o.form = newForm(a, huh.NewGroup(fields...).
		Title("Check your recovery code").
		Description(fmt.Sprintf("Enter words %s, %s, and %s from what you wrote down.", nums[0], nums[1], nums[2])))
}

func (o *onboarding) toBusy(text string) {
	o.step = stepBusy
	o.busyText = text
	o.err = ""
	o.form = nil
}

func (o *onboarding) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if o.form != nil {
			o.form.WithWidth(formWidth(a))
		}
	case tea.KeyPressMsg:
		if msg.String() == "esc" {
			switch o.step {
			case stepCreate, stepRecover, stepLinkHow:
				o.err = ""
				o.toWelcome(a)
				return o.form.Init()
			case stepLinkForm, stepLinkEnter, stepLinkCode, stepLinkWait:
				if o.step == stepLinkCode || o.step == stepLinkWait {
					o.leaveLink(a)
				}
				o.err = ""
				o.toLinkHow(a)
				return o.form.Init()
			case stepConfirm:
				o.toWords(a)
				return o.form.Init()
			}
		}
	case linkStartedMsg, linkConfirmedMsg, linkTickMsg, linkedMsg:
		cmd, _ := o.linkUpdate(a, msg)
		return cmd
	case signedUpMsg:
		if msg.err != nil {
			o.err = sentence(msg.err)
			o.toCreate(a)
			return o.form.Init()
		}
		o.identity, o.words = msg.identity, msg.words
		o.code = ""
		if slices.Equal(o.confirmed, msg.words) {
			// The same words, already typed back before the homeserver
			// refused the handle or code: register straight away.
			o.toBusy("Creating your identity on " + msg.identity.Homeserver)
			return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
				return finishedMsg{a.b.FinishSignUp(ctx)}
			}))
		}
		o.toWords(a)
		return o.form.Init()
	case recoveredMsg:
		if msg.err != nil {
			o.err = sentence(msg.err)
			o.toRecover(a)
			return o.form.Init()
		}
		o.recWords = ""
		return a.enterMain(msg.identity)
	case finishedMsg:
		if errors.Is(msg.err, ErrSignUpRefused) {
			o.err = sentence(msg.err)
			o.confirmed = o.words
			o.toCreate(a)
			return o.form.Init()
		}
		if msg.err != nil {
			o.err = sentence(msg.err)
			o.toWords(a)
			return o.form.Init()
		}
		// The words are on paper now. Forget them here too (§6.1).
		o.words, o.answers, o.confirmed = nil, nil, nil
		return a.enterMain(o.identity)
	}

	if o.form == nil {
		return nil
	}
	m, cmd := o.form.Update(msg)
	if f, ok := m.(*huh.Form); ok {
		o.form = f
	}
	if o.form.State != huh.StateCompleted {
		return cmd
	}
	return tea.Batch(cmd, o.submit(a))
}

// submit moves on once the current form is complete.
func (o *onboarding) submit(a *App) tea.Cmd {
	o.err = ""
	switch o.step {
	case stepWelcome:
		switch o.choice {
		case "recover":
			o.toRecover(a)
		case "link":
			o.toLinkHow(a)
		default:
			o.toCreate(a)
		}
		return o.form.Init()
	case stepLinkHow:
		if o.linkHow == "enter" {
			o.toLinkEnter(a)
		} else {
			o.toLinkForm(a)
		}
		return o.form.Init()
	case stepLinkForm:
		req := LinkRequest{Handle: normalizeHandle(o.linkHandle), Homeserver: normalizeHomeserver(o.linkServer)}
		o.toBusy("Getting a code from " + req.Homeserver)
		return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
			code, err := a.b.StartLink(ctx, req)
			return linkStartedMsg{code, err}
		}))
	case stepLinkEnter:
		code := strings.TrimSpace(o.approval)
		o.toBusy("Leaving this device's key with your homeserver")
		return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
			return linkStartedMsg{err: a.b.StartLinkWithCode(ctx, code)}
		}))
	case stepLinkCode:
		code := strings.TrimSpace(o.confirm)
		o.form = nil
		return call(func(ctx context.Context) tea.Msg {
			return linkConfirmedMsg{a.b.ConfirmLink(ctx, code)}
		})
	case stepCreate:
		req := SignUpRequest{
			Homeserver:       normalizeHomeserver(o.homeserver),
			Handle:           normalizeHandle(o.handle),
			RegistrationCode: strings.TrimSpace(o.code),
		}
		o.toBusy("Creating your identity on " + req.Homeserver)
		return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
			id, words, err := a.b.SignUp(ctx, req)
			return signedUpMsg{id, words, err}
		}))
	case stepRecover:
		req := RecoverRequest{Handle: normalizeHandle(o.recHandle), Homeserver: normalizeHomeserver(o.recServer), Words: splitWords(o.recWords)}
		o.toBusy("Signing in as @" + req.Handle)
		return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
			id, err := a.b.Recover(ctx, req)
			return recoveredMsg{id, err}
		}))
	case stepWords:
		o.toConfirm(a)
		return o.form.Init()
	case stepConfirm:
		o.toBusy("Finishing setup")
		return tea.Batch(a.startSpinner(), call(func(ctx context.Context) tea.Msg {
			return finishedMsg{a.b.FinishSignUp(ctx)}
		}))
	}
	return nil
}

func (o *onboarding) view(a *App) string {
	t := a.theme
	w := formWidth(a)
	var b strings.Builder
	b.WriteString(t.brand.Render("Railcar") + t.dim.Render("  Tieroute client") + "\n\n")
	if o.note != "" {
		b.WriteString(t.muted.Render(o.note) + "\n\n")
	}
	if o.err != "" {
		b.WriteString(t.danger.Width(w).Render("! "+o.err) + "\n\n")
	}
	switch o.step {
	case stepBusy, stepLinkWait:
		b.WriteString(a.spinner.View() + " " + t.muted.Render(o.busyText))
	case stepLinkCode:
		if o.form == nil {
			b.WriteString(a.spinner.View() + " " + t.muted.Render("Sending the confirmation code"))
		} else {
			b.WriteString(o.linkCodeView(a, w))
		}
	case stepWords:
		b.WriteString(o.wordsView(t, w) + "\n\n")
		b.WriteString(o.form.View())
	default:
		b.WriteString(o.form.View())
	}
	block := lipgloss.NewStyle().Width(w).Render(b.String())
	return lipgloss.Place(a.width, a.height, lipgloss.Center, lipgloss.Top,
		lipgloss.NewStyle().PaddingTop(min(2, a.height/10)).Render(block))
}

// wordsView shows the recovery words once, numbered, in columns, with the
// warning to write them down.
func (o *onboarding) wordsView(t *theme, w int) string {
	var b strings.Builder
	b.WriteString(t.bold.Render("Your recovery code") + "\n\n")
	b.WriteString(t.warning.Width(w).Render(
		"Write these 12 words down on paper, in order, and keep them somewhere safe. "+
			"They're the only way back into your identity if you lose your devices. "+
			"Railcar shows them only now and doesn't keep a copy.") + "\n\n")
	cols := 3
	if w < 42 {
		cols = 2
	}
	rows := (len(o.words) + cols - 1) / cols
	colW := 16
	var lines []string
	for r := range rows {
		var line strings.Builder
		for c := range cols {
			i := c*rows + r
			if i >= len(o.words) {
				continue
			}
			cell := t.wordNumber.Render(fmt.Sprintf("%2d ", i+1)) + t.word.Render(o.words[i])
			line.WriteString(lipgloss.NewStyle().Width(colW).Render(cell))
		}
		lines = append(lines, "  "+line.String())
	}
	b.WriteString(strings.Join(lines, "\n"))
	return b.String()
}

// sentence turns an error into interface text, starting with a capital
// letter.
func sentence(err error) string {
	s := err.Error()
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
