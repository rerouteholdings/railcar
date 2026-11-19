package ui

import (
	"image/color"
	"os"

	"charm.land/bubbles/v2/help"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// palette is the set of colors one theme uses. The dark one is Reroute's
// graphite, with Reroute orange for what matters.
type palette struct {
	bg, panel, raised, active color.Color
	line, line2               color.Color
	text, muted, dim          color.Color
	accent, onAccent          color.Color
	ok, away, danger          color.Color
}

var darkPalette = palette{
	bg:       lipgloss.Color("#1e1d1f"),
	panel:    lipgloss.Color("#252326"),
	raised:   lipgloss.Color("#2e2c2f"),
	active:   lipgloss.Color("#353236"),
	line:     lipgloss.Color("#2f2d30"),
	line2:    lipgloss.Color("#423f43"),
	text:     lipgloss.Color("#ebe7ea"),
	muted:    lipgloss.Color("#a8a2a7"),
	dim:      lipgloss.Color("#767076"),
	accent:   lipgloss.Color("#ff4a1c"),
	onAccent: lipgloss.Color("#1e1d1f"),
	ok:       lipgloss.Color("#4ade80"),
	away:     lipgloss.Color("#facc15"),
	danger:   lipgloss.Color("#f05252"),
}

// lightPalette keeps the same roles on a light terminal. The orange is
// darker, so it still reads against white.
var lightPalette = palette{
	bg:       lipgloss.Color("#fbfafb"),
	panel:    lipgloss.Color("#f3f1f2"),
	raised:   lipgloss.Color("#ebe8ea"),
	active:   lipgloss.Color("#e2dee1"),
	line:     lipgloss.Color("#dedadd"),
	line2:    lipgloss.Color("#bdb8bc"),
	text:     lipgloss.Color("#1e1d1f"),
	muted:    lipgloss.Color("#5f5a5e"),
	dim:      lipgloss.Color("#8a8489"),
	accent:   lipgloss.Color("#c7350f"),
	onAccent: lipgloss.Color("#ffffff"),
	ok:       lipgloss.Color("#15803d"),
	away:     lipgloss.Color("#a16207"),
	danger:   lipgloss.Color("#c81e1e"),
}

// theme holds every style the interface uses. It's rebuilt when the
// terminal reports a light or dark background.
type theme struct {
	dark    bool
	noColor bool
	p       palette

	text, muted, dim, accent, bold lipgloss.Style
	ok, away, danger               lipgloss.Style
	avatar                         lipgloss.Style

	pane, paneFocused lipgloss.Style
	title             lipgloss.Style
	titleFocused      lipgloss.Style

	chatRow, chatRowSelected lipgloss.Style
	chatMarker               lipgloss.Style
	badge                    lipgloss.Style

	sender, senderMine lipgloss.Style
	daySep             lipgloss.Style
	reply              lipgloss.Style

	status      lipgloss.Style
	statusKey   lipgloss.Style
	statusValue lipgloss.Style

	overlay    lipgloss.Style
	paletteRow lipgloss.Style
	paletteSel lipgloss.Style
	warning    lipgloss.Style
	wordNumber lipgloss.Style
	word       lipgloss.Style
	brand      lipgloss.Style
	help       help.Styles
}

// noColorSet reports whether NO_COLOR asks for no color (no-color.org). Bubble
// Tea already strips colors then. The theme also swaps color cues for shapes,
// so nothing depends on color alone.
func noColorSet() bool {
	v, ok := os.LookupEnv("NO_COLOR")
	return ok && v != ""
}

func newTheme(dark, noColor bool) *theme {
	p := darkPalette
	if !dark {
		p = lightPalette
	}
	t := &theme{dark: dark, noColor: noColor, p: p}
	s := lipgloss.NewStyle

	t.text = s().Foreground(p.text)
	t.muted = s().Foreground(p.muted)
	t.dim = s().Foreground(p.dim)
	t.accent = s().Foreground(p.accent)
	t.bold = s().Foreground(p.text).Bold(true)
	t.ok = s().Foreground(p.ok)
	t.away = s().Foreground(p.away)
	t.danger = s().Foreground(p.danger)
	t.avatar = s().Foreground(p.onAccent).Background(p.line2).Bold(true)
	if noColor {
		t.avatar = s().Reverse(true).Bold(true)
	}

	// Square corners everywhere. Without color, the focused pane gets a
	// thick border, so focus doesn't depend on color.
	t.pane = s().Border(lipgloss.NormalBorder()).BorderForeground(p.line)
	t.paneFocused = s().Border(lipgloss.NormalBorder()).BorderForeground(p.line2)
	if noColor {
		t.paneFocused = s().Border(lipgloss.ThickBorder())
	}
	t.title = s().Foreground(p.muted).Bold(true)
	t.titleFocused = s().Foreground(p.accent).Bold(true)

	t.chatRow = s().Foreground(p.text)
	t.chatRowSelected = s().Foreground(p.text).Background(p.active)
	t.chatMarker = s().Foreground(p.accent).Background(p.active)
	t.badge = s().Foreground(p.onAccent).Background(p.accent).Bold(true)
	if noColor {
		t.chatRowSelected = s().Reverse(true)
		t.chatMarker = s().Reverse(true)
		t.badge = s().Bold(true)
	}

	t.sender = s().Foreground(p.text).Bold(true)
	t.senderMine = s().Foreground(p.accent).Bold(true)
	t.daySep = s().Foreground(p.dim)
	t.reply = s().Foreground(p.muted).Italic(true)

	t.status = s().Foreground(p.muted).Background(p.panel)
	t.statusKey = s().Foreground(p.dim).Background(p.panel)
	t.statusValue = s().Foreground(p.text).Background(p.panel)

	t.overlay = s().Border(lipgloss.NormalBorder()).BorderForeground(p.line2).
		Background(p.panel).Padding(0, 1)
	t.paletteRow = s().Foreground(p.text).Background(p.panel)
	t.paletteSel = s().Foreground(p.text).Background(p.active).Bold(true)
	if noColor {
		t.paletteSel = s().Reverse(true)
	}
	t.warning = s().Foreground(p.text).Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(p.accent).PaddingLeft(1)
	t.wordNumber = s().Foreground(p.dim)
	t.word = s().Foreground(p.text).Bold(true)
	t.brand = s().Foreground(p.accent).Bold(true)

	t.help = help.Styles{
		ShortKey:       s().Foreground(p.muted),
		ShortDesc:      s().Foreground(p.dim),
		ShortSeparator: s().Foreground(p.line2),
		Ellipsis:       s().Foreground(p.line2),
		FullKey:        s().Foreground(p.text),
		FullDesc:       s().Foreground(p.muted),
		FullSeparator:  s().Foreground(p.line2),
	}
	return t
}

// huhTheme styles Huh forms to match: square corners, graphite, and orange
// for the focused field.
func (t *theme) huhTheme() huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		p := t.p
		h := huh.ThemeBase(t.dark)
		s := lipgloss.NewStyle

		h.Focused.Base = h.Focused.Base.BorderForeground(p.accent)
		if t.noColor {
			h.Focused.Base = h.Focused.Base.BorderStyle(lipgloss.ThickBorder())
		}
		h.Focused.Card = h.Focused.Base
		h.Focused.Title = s().Foreground(p.text).Bold(true)
		h.Focused.NoteTitle = s().Foreground(p.text).Bold(true).MarginBottom(1)
		h.Focused.Description = s().Foreground(p.muted)
		h.Focused.ErrorIndicator = h.Focused.ErrorIndicator.Foreground(p.danger)
		h.Focused.ErrorMessage = h.Focused.ErrorMessage.Foreground(p.danger)
		h.Focused.SelectSelector = s().Foreground(p.accent).SetString("> ")
		h.Focused.Option = s().Foreground(p.text)
		h.Focused.SelectedOption = s().Foreground(p.accent)
		h.Focused.NextIndicator = h.Focused.NextIndicator.Foreground(p.accent)
		h.Focused.PrevIndicator = h.Focused.PrevIndicator.Foreground(p.accent)
		h.Focused.FocusedButton = h.Focused.FocusedButton.Foreground(p.onAccent).Background(p.accent).Bold(true)
		h.Focused.BlurredButton = h.Focused.BlurredButton.Foreground(p.muted).Background(p.raised)
		h.Focused.Next = h.Focused.FocusedButton
		h.Focused.TextInput.Cursor = s().Foreground(p.accent)
		h.Focused.TextInput.Placeholder = s().Foreground(p.dim)
		h.Focused.TextInput.Prompt = s().Foreground(p.accent)
		h.Focused.TextInput.Text = s().Foreground(p.text)
		if t.noColor {
			h.Focused.FocusedButton = h.Focused.FocusedButton.Reverse(true)
		}

		h.Blurred = h.Focused
		h.Blurred.Base = h.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
		h.Blurred.Card = h.Blurred.Base
		h.Blurred.Title = s().Foreground(p.muted)
		h.Blurred.TextInput.Prompt = s().Foreground(p.dim)
		h.Blurred.TextInput.Text = s().Foreground(p.muted)
		h.Blurred.NextIndicator = lipgloss.NewStyle()
		h.Blurred.PrevIndicator = lipgloss.NewStyle()

		h.Group.Title = h.Focused.Title
		h.Group.Description = h.Focused.Description
		h.Help = t.help
		return h
	})
}
