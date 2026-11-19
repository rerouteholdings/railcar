package ui

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Notifications for mentions and @everyone (§12, §16.3): the terminal
// bell always, plus a desktop notification where it's cheap. Terminals
// that show OSC 9 or OSC 777 notifications get one of those; elsewhere,
// Railcar runs notify-send on Linux or osascript on macOS when they're
// there. A notification names who and where, never the message's text.

// notifyKind is how this terminal shows notifications.
type notifyKind int

const (
	notifyBell   notifyKind = iota // the bell, and a desktop notifier if there is one
	notifyOSC9                     // iTerm2, WezTerm, ghostty, kitty, Windows Terminal
	notifyOSC777                   // foot, urxvt, and VTE-based terminals
)

func detectNotify(getenv func(string) string) notifyKind {
	tp := getenv("TERM_PROGRAM")
	term := getenv("TERM")
	switch {
	case tp == "iTerm.app", tp == "WezTerm", tp == "ghostty", getenv("WT_SESSION") != "", strings.Contains(term, "kitty"):
		return notifyOSC9
	case strings.HasPrefix(term, "foot"), strings.Contains(term, "rxvt"), getenv("VTE_VERSION") != "":
		return notifyOSC777
	}
	return notifyBell
}

// oscSafe keeps text that goes in an escape sequence to printable
// characters, without the ones that end it.
func oscSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == ';' {
			return ' '
		}
		return r
	}, s)
}

// notifyCmd rings the bell and shows a notification.
func notifyCmd(kind notifyKind, title, body string) tea.Cmd {
	title, body = oscSafe(title), oscSafe(body)
	switch kind {
	case notifyOSC9:
		return tea.Raw("\a\x1b]9;" + title + ": " + body + "\x07")
	case notifyOSC777:
		return tea.Raw("\a\x1b]777;notify;" + title + ";" + body + "\x07")
	}
	return tea.Batch(tea.Raw("\a"), func() tea.Msg {
		desktopNotify(title, body)
		return nil
	})
}

// desktopNotify shows a notification with the system's own tool, when
// there's one, and gives up after a few seconds.
func desktopNotify(title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "linux", "freebsd", "openbsd":
		if p, err := exec.LookPath("notify-send"); err == nil && os.Getenv("DISPLAY")+os.Getenv("WAYLAND_DISPLAY") != "" {
			exec.CommandContext(ctx, p, "--app-name=Railcar", title, body).Run()
		}
	case "darwin":
		if p, err := exec.LookPath("osascript"); err == nil {
			script := "display notification " + appleString(body) + " with title " + appleString(title)
			exec.CommandContext(ctx, p, "-e", script).Run()
		}
	}
}

// appleString quotes s for AppleScript.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// mentionNotice notifies about a new message that mentions the user, or
// an @everyone, from someone else, unless notifications are off. Old
// messages, such as history, don't ring.
func (m *mainView) mentionNotice(a *App, msg Message) tea.Cmd {
	if !msg.Mention || msg.Mine || m.quiet || a.opts.Demo || a.opts.Now().Sub(msg.Sent) > 10*time.Minute {
		return nil
	}
	who := msg.Sender.Name
	if who == "" {
		who = "@" + msg.Sender.Handle
	}
	where := "a chat"
	for _, c := range m.chats {
		if c.ID == msg.Chat {
			where = c.Title
			if c.IsChannel() {
				where = "#" + c.Title + " in " + c.Community
			}
		}
	}
	return notifyCmd(m.notify, "Railcar", who+" mentioned you in "+where)
}
