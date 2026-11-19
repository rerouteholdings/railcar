package ui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// linkPoll is how often a new device tries to sign in while it waits for
// the other device (client API, "Sign in with another device" step 5).
const linkPoll = 2 * time.Second

// Messages from signing in with another device.
type (
	linkStartedMsg struct {
		code string
		err  error
	}
	linkConfirmedMsg struct{ err error }
	// linkTickMsg asks to try signing in again. gen is the link it's for,
	// so a tick from a link the user left does nothing.
	linkTickMsg struct{ gen int }
	linkedMsg   struct {
		gen      int
		identity Identity
		err      error
	}
)

// toLinkHow asks which way round the two devices link.
func (o *onboarding) toLinkHow(a *App) {
	o.step = stepLinkHow
	o.linkHow = "show"
	o.form = newForm(a, huh.NewGroup(
		huh.NewSelect[string]().
			Title("Sign in with another device").
			Description("Use a device that's already signed in to your identity.").
			Options(
				huh.NewOption("Show a code here for the other device to approve", "show"),
				huh.NewOption("Enter an approval code from the other device", "enter"),
			).
			Value(&o.linkHow),
	))
}

// toLinkForm asks for the identity to sign in to.
func (o *onboarding) toLinkForm(a *App) {
	o.step = stepLinkForm
	o.form = newForm(a, huh.NewGroup(
		huh.NewInput().
			Title("Handle").
			Placeholder("kat.chat.example.com").
			Value(&o.linkHandle).
			Validate(func(s string) error { return checkHandle(normalizeHandle(s)) }),
		huh.NewInput().
			Title("Homeserver").
			Description("The server that holds your identity.").
			Placeholder("chat.example.com").
			Value(&o.linkServer).
			Validate(func(s string) error { return checkHomeserver(normalizeHomeserver(s)) }),
	).Title("Sign in with another device"))
}

// toLinkCode shows the link code, and asks for the confirmation code.
func (o *onboarding) toLinkCode(a *App) {
	o.step = stepLinkCode
	o.confirm = ""
	o.form = newForm(a, huh.NewGroup(
		huh.NewInput().
			Title("Confirmation code").
			Description("If the other device shows a confirmation code, enter it here. Never read it to anyone or paste it into a website.").
			Placeholder("XXXX-XXXX").
			Value(&o.confirm).
			Validate(checkConfirmation),
	))
}

// toLinkEnter asks for an approval code.
func (o *onboarding) toLinkEnter(a *App) {
	o.step = stepLinkEnter
	o.approval = ""
	o.form = newForm(a, huh.NewGroup(
		huh.NewInput().
			Title("Approval code").
			Description("On a device that's already signed in, choose to show an approval code, and enter it here. It starts with approve-.").
			Placeholder("approve-…").
			CharLimit(300).
			Value(&o.approval).
			Validate(func(s string) error {
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "approve") {
					return errors.New("An approval code starts with approve-")
				}
				return nil
			}),
	).Title("Sign in with another device"))
}

// checkConfirmation checks a typed confirmation code: 8 letters and
// digits, with or without the dash.
func checkConfirmation(s string) error {
	n := 0
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			n++
		}
	}
	if n != 8 {
		return errors.New("A confirmation code has 8 letters and digits, like 7K2M-9QXD")
	}
	return nil
}

// pollLink schedules the next try at signing in.
func (o *onboarding) pollLink() tea.Cmd {
	gen := o.linkGen
	return tea.Tick(linkPoll, func(time.Time) tea.Msg { return linkTickMsg{gen} })
}

// linking reports whether this device is waiting for the other device.
func (o *onboarding) linking() bool {
	return o.step == stepLinkCode || o.step == stepLinkWait
}

// leaveLink gives up on the link in progress.
func (o *onboarding) leaveLink(a *App) {
	o.linkGen++
	o.linkCode = ""
	go a.b.CancelLink()
}

// linkUpdate handles the messages of signing in with another device. It
// reports whether it handled msg.
func (o *onboarding) linkUpdate(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case linkStartedMsg:
		if msg.err != nil {
			o.err = sentence(msg.err)
			if o.linkHow == "enter" {
				o.toLinkEnter(a)
			} else {
				o.toLinkForm(a)
			}
			return o.form.Init(), true
		}
		o.linkGen++
		if o.linkHow == "enter" {
			o.toBusy("Waiting for the other device to add this one")
			o.step = stepLinkWait
			return tea.Batch(a.startSpinner(), o.pollLink()), true
		}
		o.linkCode = msg.code
		o.toLinkCode(a)
		return tea.Batch(o.form.Init(), o.pollLink()), true
	case linkConfirmedMsg:
		if msg.err != nil {
			o.err = sentence(msg.err)
			o.toLinkCode(a)
			return o.form.Init(), true
		}
		o.toBusy("Waiting for the other device to add this one")
		o.step = stepLinkWait
		return a.startSpinner(), true
	case linkTickMsg:
		if msg.gen != o.linkGen || !o.linking() {
			return nil, true
		}
		gen := msg.gen
		return call(func(ctx context.Context) tea.Msg {
			id, err := a.b.FinishLink(ctx)
			return linkedMsg{gen, id, err}
		}), true
	case linkedMsg:
		if msg.gen != o.linkGen || !o.linking() {
			return nil, true
		}
		switch {
		case msg.err == nil:
			o.linkCode = ""
			return a.enterMain(msg.identity), true
		case errors.Is(msg.err, ErrNotLinkedYet):
		default:
			// Keep trying: the homeserver may be unreachable for a moment.
			o.err = sentence(msg.err)
		}
		return o.pollLink(), true
	}
	return nil, false
}

// linkCodeView shows the link code as a QR code, when it fits, and as
// text, above the confirmation code field.
func (o *onboarding) linkCodeView(a *App, w int) string {
	t := a.theme
	var b strings.Builder
	b.WriteString(t.bold.Render("Sign in with another device") + "\n\n")
	b.WriteString(t.muted.Width(w).Render("On a device that's already signed in as @"+normalizeHandle(o.linkHandle)+
		", choose to add a device. Then scan this code, or enter it there.") + "\n\n")
	cols, rows := qrSize(o.linkCode)
	if cols > 0 && cols <= a.width-4 && rows+18 <= a.height {
		b.WriteString(lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(renderQR(o.linkCode, a.theme.dark)) + "\n\n")
	} else {
		b.WriteString(t.dim.Width(w).Render("Make the window larger to show the code as a QR code.") + "\n\n")
	}
	b.WriteString(t.accent.Width(w).Render(o.linkCode) + "\n\n")
	b.WriteString(o.form.View())
	return b.String()
}
