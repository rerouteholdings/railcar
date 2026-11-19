package ui

import (
	"context"
	"errors"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// linkWarning is the warning shown before a confirmation code or an
// approval code (§7.2 step 2, §7.2.1).
const linkWarning = "Only continue if you're setting up your own device, right now. Your app will never ask you for this code."

// devicesState is the state of the device overlays: adding a device by
// its link code, showing an approval code, and the device list.
type devicesState struct {
	// step is 0 while the user enters what's needed, and 1 while a code is
	// shown and Railcar waits for the new device.
	step     int
	code     textinput.Model
	name     textinput.Model
	nameOnly bool // the approval code box has only the name field
	onName   bool
	busy     bool
	err      string
	// gen numbers the approval in progress, so a poll from one the user
	// left does nothing.
	gen int

	confirmation string
	approval     ApprovalCode

	list     []Device
	cursor   int
	removing bool
}

// Messages from the device overlays.
type (
	approvingMsg struct {
		confirmation string
		approval     ApprovalCode
		err          error
	}
	approvalTickMsg struct{ gen int }
	approvedMsg     struct {
		gen  int
		done bool
		err  error
	}
	devicesMsg struct {
		devices []Device
		err     error
	}
)

func newInput(a *App, placeholder string, limit int) textinput.Model {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = placeholder
	in.CharLimit = limit
	st := textinput.DefaultStyles(a.theme.dark)
	st.Focused.Prompt = a.theme.accent
	st.Focused.Text = a.theme.text
	st.Focused.Placeholder = a.theme.dim
	st.Cursor.Color = a.theme.p.accent
	in.SetStyles(st)
	return in
}

// openAddDevice shows the box to enter a new device's link code.
func (m *mainView) openAddDevice(a *App, code string) tea.Cmd {
	m.dev = devicesState{gen: m.dev.gen + 1}
	m.dev.code = newInput(a, "link-…", 400)
	m.dev.code.SetValue(code)
	m.dev.name = newInput(a, "New device", 64)
	m.overlay = overlayAddDevice
	return m.dev.code.Focus()
}

// openApprovalCode shows the warning, and the name for the new device,
// before an approval code (§7.2.1 step 1).
func (m *mainView) openApprovalCode(a *App) tea.Cmd {
	m.dev = devicesState{gen: m.dev.gen + 1, nameOnly: true, onName: true}
	m.dev.name = newInput(a, "New device", 64)
	m.overlay = overlayApprovalCode
	return m.dev.name.Focus()
}

// openDevices shows the identity's devices.
func (m *mainView) openDevices(a *App) tea.Cmd {
	m.dev = devicesState{gen: m.dev.gen + 1, busy: true}
	m.overlay = overlayDevices
	return call(func(ctx context.Context) tea.Msg {
		ds, err := a.b.Devices(ctx)
		return devicesMsg{ds, err}
	})
}

// closeDevices closes a device overlay, and stops an approval in progress.
func (m *mainView) closeDevices(a *App) {
	if m.overlay == overlayAddDevice || m.overlay == overlayApprovalCode {
		go a.b.CancelApproval()
	}
	m.dev.gen++
	m.overlay = overlayNone
}

func (m *mainView) pollApproval() tea.Cmd {
	gen := m.dev.gen
	return tea.Tick(linkPoll, func(time.Time) tea.Msg { return approvalTickMsg{gen} })
}

// deviceName is the name typed for the new device, or the default.
func (d *devicesState) deviceName() string {
	if n := strings.TrimSpace(d.name.Value()); n != "" {
		return n
	}
	return "New device"
}

// devicesUpdate handles the device overlays' messages. It reports whether
// it handled msg.
func (m *mainView) devicesUpdate(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case approvingMsg:
		m.dev.busy = false
		if msg.err != nil {
			m.dev.err = sentence(msg.err)
			return nil, true
		}
		m.dev.step, m.dev.err = 1, ""
		m.dev.confirmation, m.dev.approval = msg.confirmation, msg.approval
		return tea.Batch(a.startSpinner(), m.pollApproval()), true
	case approvalTickMsg:
		if msg.gen != m.dev.gen || m.dev.step != 1 {
			return nil, true
		}
		gen := msg.gen
		return call(func(ctx context.Context) tea.Msg {
			done, err := a.b.FinishApproval(ctx)
			return approvedMsg{gen, done, err}
		}), true
	case approvedMsg:
		if msg.gen != m.dev.gen || m.dev.step != 1 {
			return nil, true
		}
		switch {
		case msg.done:
			m.overlay = overlayNone
			m.notice = "Added " + m.dev.deviceName() + ". It joins your chats as soon as it's ready."
			return nil, true
		case errors.Is(msg.err, ErrApprovalUsedTwice), errors.Is(msg.err, ErrApprovalExpired):
			// The code is gone; nothing more to wait for.
			m.dev.err, m.dev.step = sentence(msg.err), 2
			return nil, true
		case msg.err != nil:
			m.dev.err = sentence(msg.err)
		default:
			m.dev.err = ""
		}
		return m.pollApproval(), true
	case devicesMsg:
		m.dev.busy = false
		if msg.err != nil {
			m.dev.err = sentence(msg.err)
			return nil, true
		}
		m.dev.list = msg.devices
		m.dev.cursor = min(m.dev.cursor, max(0, len(m.dev.list)-1))
		return nil, true
	}
	return nil, false
}

// addDeviceKey handles a key in the add-device box.
func (m *mainView) addDeviceKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	d := &m.dev
	switch msg.String() {
	case "esc":
		m.closeDevices(a)
		return nil
	}
	if d.step > 0 {
		return nil
	}
	switch msg.String() {
	case "tab", "shift+tab", "down", "up":
		if d.nameOnly {
			return nil
		}
		d.onName = !d.onName
		if d.onName {
			d.code.Blur()
			return d.name.Focus()
		}
		d.name.Blur()
		return d.code.Focus()
	case "enter":
		if d.busy {
			return nil
		}
		name := d.deviceName()
		if d.nameOnly {
			d.busy, d.err = true, ""
			return call(func(ctx context.Context) tea.Msg {
				ac, err := a.b.ShowApprovalCode(ctx, name)
				return approvingMsg{approval: ac, err: err}
			})
		}
		code := strings.TrimSpace(d.code.Value())
		if code == "" {
			return nil
		}
		d.busy, d.err = true, ""
		return call(func(ctx context.Context) tea.Msg {
			c, err := a.b.ApproveLinkCode(ctx, code, name)
			return approvingMsg{confirmation: c, err: err}
		})
	}
	var cmd tea.Cmd
	if d.onName {
		d.name, cmd = d.name.Update(msg)
	} else {
		d.code, cmd = d.code.Update(msg)
	}
	return cmd
}

// devicePaste puts pasted text in the device box's field.
func (m *mainView) devicePaste(msg tea.PasteMsg) tea.Cmd {
	var cmd tea.Cmd
	if m.dev.onName {
		m.dev.name, cmd = m.dev.name.Update(msg)
	} else {
		m.dev.code, cmd = m.dev.code.Update(msg)
	}
	return cmd
}

// devicesKey handles a key in the device list.
func (m *mainView) devicesKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	d := &m.dev
	if d.removing {
		switch msg.String() {
		case "enter", "y":
			d.removing = false
			if d.cursor >= len(d.list) {
				return nil
			}
			dev := d.list[d.cursor]
			d.busy = true
			return call(func(ctx context.Context) tea.Msg {
				if err := a.b.RemoveDevice(ctx, dev.Key); err != nil {
					return devicesMsg{err: err}
				}
				ds, err := a.b.Devices(ctx)
				return devicesMsg{ds, err}
			})
		case "esc", "n":
			d.removing = false
		}
		return nil
	}
	switch msg.String() {
	case "esc", "q":
		m.closeDevices(a)
	case "up", "k":
		d.cursor = max(0, d.cursor-1)
	case "down", "j":
		d.cursor = min(max(0, len(d.list)-1), d.cursor+1)
	case "d", "delete", "backspace":
		if d.cursor < len(d.list) && !d.list[d.cursor].This && !d.busy {
			d.removing, d.err = true, ""
		}
	case "a":
		return m.openAddDevice(a, "")
	}
	return nil
}

// addDeviceView draws the add-device box: the code field, then the
// confirmation code to type on the new device.
func (m *mainView) addDeviceView(a *App) string {
	t := a.theme
	d := &m.dev
	w := min(a.width-6, 64)
	var b strings.Builder
	b.WriteString(t.bold.Render("Add a device") + "\n\n")
	if d.step == 0 {
		d.code.SetWidth(w - 3)
		d.name.SetWidth(w - 3)
		b.WriteString(t.muted.Width(w).Render("On the new device, choose to sign in with another device, and enter the code it shows here. Only enter a code shown on a device you're holding.") + "\n\n")
		b.WriteString(t.text.Render("Code") + "\n" + d.code.View() + "\n\n")
		b.WriteString(t.text.Render("Name for the new device") + "\n" + d.name.View() + "\n\n")
		b.WriteString(m.deviceStatus(a, w, "Checking the code"))
		b.WriteString(t.accent.Render("enter") + t.muted.Render(" continue   ") + t.accent.Render("tab") + t.muted.Render(" next field   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	} else {
		b.WriteString(t.warning.Width(w).Render(linkWarning) + "\n\n")
		b.WriteString(t.muted.Width(w).Render("Enter this confirmation code on the new device. Never read it to anyone or paste it into a website.") + "\n\n")
		b.WriteString(lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(t.accent.Bold(true).Render(d.confirmation)) + "\n\n")
		b.WriteString(m.deviceStatus(a, w, "Waiting for the new device"))
		b.WriteString(t.accent.Render("esc") + t.muted.Render(" cancel"))
	}
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// approvalCodeView draws the approval code box: the warning and name
// first, then the code as a QR code and as text (§7.2.1).
func (m *mainView) approvalCodeView(a *App) string {
	t := a.theme
	d := &m.dev
	w := min(a.width-6, 64)
	var b strings.Builder
	b.WriteString(t.bold.Render("Show an approval code") + "\n\n")
	b.WriteString(t.warning.Width(w).Render(linkWarning) + "\n\n")
	switch d.step {
	case 0:
		d.name.SetWidth(w - 3)
		b.WriteString(t.muted.Width(w).Render("A new device that reads this code is added to your identity. The code works for 10 minutes, for one device.") + "\n\n")
		b.WriteString(t.text.Render("Name for the new device") + "\n" + d.name.View() + "\n\n")
		b.WriteString(m.deviceStatus(a, w, "Making the code"))
		b.WriteString(t.accent.Render("enter") + t.muted.Render(" show the code   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	case 1:
		b.WriteString(t.muted.Width(w).Render("On the new device, choose to sign in with another device, then scan this code or enter it there.") + "\n\n")
		cols, rows := qrSize(d.approval.Text)
		if cols > 0 && cols <= w && rows+16 <= a.height {
			b.WriteString(lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(renderQR(d.approval.Text, t.dark)) + "\n\n")
		}
		b.WriteString(t.accent.Width(w).Render(d.approval.Text) + "\n")
		b.WriteString(t.dim.Render("Works until "+d.approval.Expires.In(a.opts.Location).Format("3:04 PM")) + "\n\n")
		b.WriteString(m.deviceStatus(a, w, "Waiting for the new device"))
		b.WriteString(t.accent.Render("esc") + t.muted.Render(" cancel"))
	default:
		b.WriteString(m.deviceStatus(a, w, ""))
		b.WriteString(t.accent.Render("esc") + t.muted.Render(" close"))
	}
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// deviceStatus is the error or the spinner line of a device box.
func (m *mainView) deviceStatus(a *App, w int, waiting string) string {
	t := a.theme
	switch {
	case m.dev.err != "":
		return t.danger.Width(w).Render(m.dev.err) + "\n\n"
	case m.dev.busy || m.dev.step == 1 && waiting != "":
		return a.spinner.View() + " " + t.muted.Render(waiting) + "\n\n"
	}
	return ""
}

// devicesView draws the device list.
func (m *mainView) devicesView(a *App) string {
	t := a.theme
	d := &m.dev
	w := min(a.width-6, 64)
	var b strings.Builder
	b.WriteString(t.bold.Render("Your devices") + "\n\n")
	switch {
	case d.busy && len(d.list) == 0:
		b.WriteString(a.spinner.View() + " " + t.muted.Render("Loading") + "\n\n")
	case len(d.list) == 0:
		b.WriteString(t.dim.Render("No devices") + "\n\n")
	}
	for i, dev := range d.list {
		name := dev.Name
		if name == "" {
			name = "Unnamed device"
		}
		if dev.This {
			name += " (this device)"
		}
		line := name + "  " + t.dim.Render("added "+dev.Added.In(a.opts.Location).Format("January 2, 2006"))
		if i == d.cursor {
			b.WriteString(t.paletteSel.Render("> "+name) + "  " + t.dim.Render("added "+dev.Added.In(a.opts.Location).Format("January 2, 2006")) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	if d.removing && d.cursor < len(d.list) {
		b.WriteString(t.warning.Width(w).Render("Remove "+d.list[d.cursor].Name+"? It signs out, and your chats take it out, so it can't read anything new.") + "\n\n")
		b.WriteString(t.accent.Render("enter") + t.muted.Render(" remove   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	} else {
		if d.err != "" {
			b.WriteString(t.danger.Width(w).Render(d.err) + "\n\n")
		}
		b.WriteString(t.accent.Render("a") + t.muted.Render(" add a device   ") + t.accent.Render("d") + t.muted.Render(" remove   ") + t.accent.Render("esc") + t.muted.Render(" close"))
	}
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}
