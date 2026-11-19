package ui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// chatItem is one chat in the list.
type chatItem struct{ chat Chat }

// FilterValue implements list.Item. Filtering matches the title, then
// every member's name and handle, after a tab.
func (c chatItem) FilterValue() string {
	parts := []string{}
	for _, m := range c.chat.Members {
		parts = append(parts, m.Name, m.Handle)
	}
	return c.chat.Title + "\t" + strings.Join(parts, " ")
}

// communityItem is a community's heading in the list, above its
// channels. Enter folds and unfolds it.
type communityItem struct {
	name, server string
	folded       bool
	unread       int
}

// FilterValue implements list.Item.
func (c communityItem) FilterValue() string { return c.name + "\t" + c.server }

// filterChats keeps the chats that contain what's typed, in any case. Chats
// whose title starts with it come first, then titles with a word that
// starts with it, then other titles, then chats that match only by a
// member.
func filterChats(term string, targets []string) []list.Rank {
	term = strings.ToLower(strings.TrimSpace(term))
	type ranked struct {
		rank  list.Rank
		score int
	}
	var out []ranked
	for i, t := range targets {
		title, rest, _ := strings.Cut(strings.ToLower(t), "\t")
		score := -1
		pos := strings.Index(title, term)
		switch {
		case strings.HasPrefix(title, term):
			score = 0
		case pos > 0 && title[pos-1] == ' ':
			score = 1
		case pos > 0:
			score = 2
		case strings.Contains(rest, term):
			score = 3
		}
		if score < 0 {
			continue
		}
		r := list.Rank{Index: i}
		if pos >= 0 {
			for j := range len(term) {
				r.MatchedIndexes = append(r.MatchedIndexes, pos+j)
			}
		}
		out = append(out, ranked{r, score})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score < out[j].score })
	ranks := make([]list.Rank, len(out))
	for i, r := range out {
		ranks[i] = r.rank
	}
	return ranks
}

// chatDelegate draws each chat as two lines: the title with its time or
// unread count, and a preview of the latest message.
type chatDelegate struct {
	t   **theme
	now func() time.Time
	loc *time.Location
}

func (d chatDelegate) Height() int                         { return 2 }
func (d chatDelegate) Spacing() int                        { return 1 }
func (d chatDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d chatDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	t := *d.t
	if h, ok := item.(communityItem); ok {
		d.renderCommunity(w, m, index, h)
		return
	}
	ci, ok := item.(chatItem)
	if !ok {
		return
	}
	c := ci.chat
	width := m.Width()
	selected := index == m.Index()

	title, muted, marker := t.bold, t.muted, " "
	if c.Unread == 0 {
		title = t.text
	}
	if selected {
		bg := t.p.active
		title = title.Background(bg)
		muted = muted.Background(bg)
		marker = "▌"
		if t.noColor {
			title, muted = t.chatRowSelected, t.chatRowSelected
		}
	}
	mark := lipgloss.NewStyle()
	if selected {
		mark = t.chatMarker
	}

	var right string
	var rightW int
	if c.Unread > 0 {
		n := fmt.Sprint(c.Unread)
		if c.Unread > 99 {
			n = "99+"
		}
		right = t.badge.Render(" " + n + " ")
		rightW = lipgloss.Width(right)
	} else if !c.LastActivity.IsZero() {
		label := listTime(c.LastActivity, d.now(), d.loc)
		right = muted.Render(label)
		rightW = lipgloss.Width(label)
	}

	inner := width - 2
	titleW := max(1, inner-rightW-1)
	label := c.Title
	switch c.Kind {
	case ChannelChat:
		label = "  # " + c.Title
	case VoiceChannel:
		label = "  🔊 " + c.Title
		if len(c.InVoice) > 0 {
			label += fmt.Sprintf(" (%d)", len(c.InVoice))
		}
	}
	name := ansi.Truncate(label, titleW, "…")
	gap := max(1, inner-lipgloss.Width(name)-rightW)
	line1 := mark.Render(marker) + title.Render(" "+name) + title.Render(strings.Repeat(" ", max(0, gap-1))) + right + title.Render(" ")

	preview := c.Preview
	switch {
	case c.Kind == VoiceChannel:
		preview = "Voice channel"
	case preview == "":
		preview = "No messages yet"
	}
	if c.IsChannel() {
		preview = "  " + preview
	}
	preview = ansi.Truncate(preview, max(1, inner-1), "…")
	line2 := mark.Render(marker) + muted.Render(" "+preview+strings.Repeat(" ", max(0, inner-1-lipgloss.Width(preview)))+" ")

	fmt.Fprint(w, line1+"\n"+line2)
}

// renderCommunity draws a community's heading: its name, with its
// unread count, and its community server.
func (d chatDelegate) renderCommunity(w io.Writer, m list.Model, index int, h communityItem) {
	t := *d.t
	width := m.Width()
	selected := index == m.Index()
	title, muted, marker := t.bold, t.muted, " "
	if selected {
		title, muted, marker = title.Background(t.p.active), muted.Background(t.p.active), "▌"
		if t.noColor {
			title, muted = t.chatRowSelected, t.chatRowSelected
		}
	}
	mark := lipgloss.NewStyle()
	if selected {
		mark = t.chatMarker
	}
	arrow := "▾ "
	if h.folded {
		arrow = "▸ "
	}
	right, rightW := "", 0
	if h.unread > 0 && h.folded {
		right = t.badge.Render(fmt.Sprintf(" %d ", h.unread))
		rightW = lipgloss.Width(right)
	}
	inner := width - 2
	name := ansi.Truncate(arrow+h.name, max(1, inner-rightW-1), "…")
	gap := max(1, inner-lipgloss.Width(name)-rightW)
	line1 := mark.Render(marker) + title.Render(" "+name) + title.Render(strings.Repeat(" ", max(0, gap-1))) + right + title.Render(" ")
	sub := ansi.Truncate("Community on "+h.server, max(1, inner-1), "…")
	line2 := mark.Render(marker) + muted.Render(" "+sub+strings.Repeat(" ", max(0, inner-1-lipgloss.Width(sub)))+" ")
	fmt.Fprint(w, line1+"\n"+line2)
}

// newChatList returns an empty, styled chat list.
func newChatList(a *App) list.Model {
	d := chatDelegate{t: &a.theme, now: a.opts.Now, loc: a.opts.Location}
	l := list.New(nil, d, 0, 0)
	l.Title = "Chats"
	l.SetShowStatusBar(false)
	l.SetShowPagination(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(true)
	l.Filter = filterChats
	l.DisableQuitKeybindings()
	l.KeyMap.ShowFullHelp.SetEnabled(false)
	l.KeyMap.CloseFullHelp.SetEnabled(false)
	// Left and right move between panes, not pages.
	l.KeyMap.PrevPage = key.NewBinding(key.WithKeys("pgup"))
	l.KeyMap.NextPage = key.NewBinding(key.WithKeys("pgdown"))
	l.KeyMap.AcceptWhileFiltering = key.NewBinding(key.WithKeys("enter", "tab", "up", "down"))
	styleChatList(&l, a.theme)
	return l
}

// styleChatList applies the theme to the list's own parts.
func styleChatList(l *list.Model, t *theme) {
	s := list.DefaultStyles(t.dark)
	s.TitleBar = lipgloss.NewStyle().Padding(0, 1)
	s.Title = t.title
	s.NoItems = t.dim.Padding(0, 1)
	s.Filter.Focused.Prompt = t.accent
	s.Filter.Blurred.Prompt = t.muted
	s.Filter.Focused.Text = t.text
	s.Filter.Cursor.Color = t.p.accent
	l.Styles = s
	l.FilterInput.Prompt = "Find: "
	l.FilterInput.SetStyles(s.Filter)
	l.SetStatusBarItemName("chat", "chats")
}

// listTime is the short time beside a chat: the time today, then
// Yesterday, then the weekday, then the date.
func listTime(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	days := daysBetween(t, now)
	switch {
	case days <= 0:
		return t.Format("3:04 PM")
	case days == 1:
		return "Yesterday"
	case days < 7:
		return t.Format("Mon")
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2, 2006")
	}
}

// daysBetween counts calendar days from a to b, in a's time zone.
func daysBetween(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}
