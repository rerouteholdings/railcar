package ui

import "charm.land/bubbles/v2/key"

// keyMap lists every key binding in the main view. The help overlay shows
// them grouped as in FullHelp.
type keyMap struct {
	// Anywhere.
	Quit       key.Binding
	Help       key.Binding
	Palette    key.Binding
	Details    key.Binding
	NextPane   key.Binding
	PrevPane   key.Binding
	NextChat   key.Binding
	PrevChat   key.Binding
	Back       key.Binding
	Command    key.Binding
	ChatsFind  key.Binding
	AddContact key.Binding
	Attach     key.Binding

	// Calls and voice channels.
	Answer key.Binding
	HangUp key.Binding
	Mute   key.Binding

	// A contact request.
	Accept  key.Binding
	Decline key.Binding

	// The chat list.
	Up   key.Binding
	Down key.Binding
	Open key.Binding

	// The message view.
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding
	Reply    key.Binding
	EditLast key.Binding

	// The composer.
	Send    key.Binding
	Newline key.Binding
}

func newKeyMap() keyMap {
	b := func(keys []string, k, desc string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(k, desc))
	}
	return keyMap{
		Quit:       b([]string{"ctrl+c"}, "ctrl+c", "quit"),
		Help:       b([]string{"?", "f1"}, "?", "help"),
		Palette:    b([]string{"ctrl+p"}, "ctrl+p", "commands"),
		Command:    b([]string{":"}, ":", "commands"),
		Details:    b([]string{"ctrl+o"}, "ctrl+o", "details"),
		NextPane:   b([]string{"tab"}, "tab", "next pane"),
		PrevPane:   b([]string{"shift+tab"}, "shift+tab", "previous pane"),
		NextChat:   b([]string{"alt+down", "ctrl+down"}, "alt+↓", "next chat"),
		PrevChat:   b([]string{"alt+up", "ctrl+up"}, "alt+↑", "previous chat"),
		Back:       b([]string{"esc"}, "esc", "back"),
		ChatsFind:  b([]string{"/"}, "/", "find a chat"),
		AddContact: b([]string{"ctrl+n"}, "ctrl+n", "add a contact"),
		Attach:     b([]string{"ctrl+a"}, "ctrl+a", "send a file"),

		Answer: b([]string{"ctrl+y"}, "ctrl+y", "answer a call"),
		HangUp: b([]string{"ctrl+x"}, "ctrl+x", "hang up, decline, or leave voice"),
		Mute:   b([]string{"ctrl+r"}, "ctrl+r", "mute or unmute"),

		Accept:  b([]string{"a"}, "a", "accept request"),
		Decline: b([]string{"d"}, "d", "decline request"),

		Up:   b([]string{"up", "k"}, "↑/k", "up"),
		Down: b([]string{"down", "j"}, "↓/j", "down"),
		Open: b([]string{"enter"}, "enter", "open"),

		PageUp:   b([]string{"pgup", "b"}, "pgup", "page up"),
		PageDown: b([]string{"pgdown", "f", "space"}, "pgdn", "page down"),
		Top:      b([]string{"home", "g"}, "home", "oldest"),
		Bottom:   b([]string{"end", "G"}, "end", "latest"),
		Reply:    b([]string{"r"}, "r", "reply to the last message"),
		EditLast: b([]string{"e"}, "e", "edit your last message"),

		Send:    b([]string{"enter"}, "enter", "send"),
		Newline: b([]string{"shift+enter", "alt+enter", "ctrl+j"}, "shift+enter", "new line"),
	}
}

// shortHelp is the one-line hint for the focused pane.
func (k keyMap) shortHelp(f focus) []key.Binding {
	switch f {
	case focusChats:
		return []key.Binding{k.Open, k.ChatsFind, k.NextPane, k.Palette, k.Help}
	case focusMessages:
		return []key.Binding{k.Up, k.PageUp, k.Bottom, k.NextPane, k.Help}
	default:
		return []key.Binding{k.Send, k.Newline, k.Back, k.Palette, k.Details}
	}
}

// fullHelp is every binding, in columns.
func (k keyMap) fullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.NextPane, k.PrevPane, k.NextChat, k.PrevChat, k.Back},
		{k.Up, k.Down, k.Open, k.ChatsFind, k.AddContact, k.Accept, k.Decline},
		{k.PageUp, k.PageDown, k.Top, k.Bottom, k.Reply, k.EditLast},
		{k.Send, k.Newline, k.Attach, k.Details, k.Palette, k.Command, k.Help, k.Quit},
		{k.Answer, k.HangUp, k.Mute},
	}
}
