package ui

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// focus is the pane that gets keys.
type focus int

const (
	focusChats focus = iota
	focusMessages
	focusComposer
)

// overlay is a box drawn over the main view.
type overlay int

const (
	overlayNone overlay = iota
	overlayHelp
	overlayPalette
	overlaySignOut
	overlayAddContact
	overlayInvite
	overlayAddDevice
	overlayApprovalCode
	overlayDevices
	overlayNewGroup
)

// narrowWidth is the width under which the main view shows one pane at a
// time.
const narrowWidth = 80

// historyLimit is how many messages a chat loads when it opens.
const historyLimit = 200

// composerHeight is the composer's height in lines.
const composerHeight = 3

// rect is an area of the screen, in cells.
type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool {
	return r.w > 0 && x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h
}

// layout is where each pane goes. Hidden panes have a zero rect.
type layout struct {
	narrow               bool
	chats, conv, details rect
	viewport, composer   rect
	listTop              int
}

// mainView is the chat list, the message view, the composer, and the
// details pane, with the status bar under them.
type mainView struct {
	// lastInput is the last key or click, for auto-away (§13.2), and
	// awayBusy is set while a change of it is on its way.
	lastInput time.Time
	awayBusy  bool

	chats    []Chat
	list     list.Model
	active   string
	messages map[string][]Message
	loaded   map[string]bool
	drafts   map[string]string

	vp       viewport.Model
	composer textarea.Model
	help     help.Model

	focus       focus
	showDetails bool
	// narrowDetails shows the details pane in place of the conversation,
	// in the narrow layout.
	narrowDetails bool
	overlay       overlay
	palette       *commandPalette
	notice        string
	layout        layout

	// The add-contact box, and the invite code being shown.
	addInput   textinput.Model
	addErr     string
	addBusy    bool
	inviteCode string

	// The device boxes: adding one, an approval code, and the list.
	dev devicesState

	// The composer's mode: answering a message, or changing one of the
	// identity's own.
	replyTo string
	editing string

	// Typing indicators: who's typing in each chat, and what this device
	// last sent (§13.3).
	typing     map[string]map[string]typist
	typingChat string
	typingSent time.Time
	typingSeq  int

	// follow is the handle of a contact request that was open when it left
	// the list. Their direct chat opens when it arrives.
	follow string

	// folded lists the communities whose channels the chat list hides,
	// by community server.
	folded map[string]bool

	// The create-group box.
	grp groupForm
}

func newMainView(a *App) *mainView {
	m := &mainView{
		list:     newChatList(a),
		messages: map[string][]Message{},
		loaded:   map[string]bool{},
		drafts:   map[string]string{},
		typing:   map[string]map[string]typist{},
		folded:   map[string]bool{},
		help:     help.New(),
	}
	m.vp = viewport.New()
	m.vp.MouseWheelEnabled = false
	m.vp.KeyMap.Left.SetEnabled(false)
	m.vp.KeyMap.Right.SetEnabled(false)

	ta := textarea.New()
	ta.Prompt = " "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = composerHeight
	ta.SetHeight(composerHeight)
	ta.KeyMap.InsertNewline = a.keys.Newline
	// Ctrl+P opens the command palette, so the composer moves between lines
	// with the arrows only.
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys("down"))
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys("up"))
	m.composer = ta
	m.applyTheme(a.theme)
	return m
}

func (m *mainView) applyTheme(t *theme) {
	styleChatList(&m.list, t)
	st := textarea.DefaultStyles(t.dark)
	for _, s := range []*textarea.StyleState{&st.Focused, &st.Blurred} {
		s.Base = lipgloss.NewStyle()
		s.CursorLine = lipgloss.NewStyle()
		s.Text = t.text
		s.Placeholder = t.dim
		s.EndOfBuffer = t.dim
	}
	st.Cursor.Color = t.p.accent
	st.Cursor.Blink = false
	m.composer.SetStyles(st)
	m.help.Styles = t.help
}

func (m *mainView) init(a *App) tea.Cmd {
	m.resize(a)
	m.lastInput = a.opts.Now()
	return tea.Batch(m.loadChats(a), idleTick())
}

// Auto-away (§13.2): after idleAfter without a key or a click, Railcar
// sets the user away, and back when they return.
const (
	idleAfter = 10 * time.Minute
	idleCheck = 30 * time.Second
)

type idleTickMsg struct{}

func idleTick() tea.Cmd {
	return tea.Tick(idleCheck, func(time.Time) tea.Msg { return idleTickMsg{} })
}

// noteInput records input, and ends auto-away.
func (m *mainView) noteInput(a *App) tea.Cmd {
	m.lastInput = a.opts.Now()
	if !a.identity.Away || m.awayBusy {
		return nil
	}
	m.awayBusy = true
	return call(func(ctx context.Context) tea.Msg {
		_, err := a.b.Act(ctx, "", Action{Kind: "auto-away", Arg: "off"})
		return awayMsg{err}
	})
}

type awayMsg struct{ err error }

func (m *mainView) loadChats(a *App) tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		chats, err := a.b.Chats(ctx)
		return chatsMsg{chats, err}
	})
}

func (m *mainView) loadMessages(a *App, chat string) tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		msgs, err := a.b.Messages(ctx, chat, "", historyLimit)
		return messagesMsg{chat, msgs, err}
	})
}

func (m *mainView) markRead(a *App, chat string) tea.Cmd {
	return call(func(ctx context.Context) tea.Msg {
		if err := a.b.MarkRead(ctx, chat); err != nil {
			return noticeMsg{"Railcar couldn't mark the chat as read: " + err.Error()}
		}
		return nil
	})
}

// computeLayout places the panes for the current size.
func (m *mainView) computeLayout(a *App) layout {
	w, h := a.width, max(3, a.height-1)
	var l layout
	if w < narrowWidth {
		l.narrow = true
		full := rect{0, 0, w, h}
		switch {
		case m.focus == focusChats:
			l.chats = full
		case m.narrowDetails:
			l.details = full
		default:
			l.conv = full
		}
	} else {
		chatsW := min(34, max(26, w*27/100))
		detailsW := 0
		if m.showDetails {
			detailsW = min(34, max(26, w/4))
		}
		if w-chatsW-detailsW < 44 {
			chatsW = 0
		}
		convW := w - chatsW - detailsW
		if chatsW > 0 {
			l.chats = rect{0, 0, chatsW, h}
		}
		l.conv = rect{chatsW, 0, convW, h}
		if detailsW > 0 {
			l.details = rect{chatsW + convW, 0, detailsW, h}
		}
	}
	if l.chats.w > 0 {
		// The border, then the list's title row.
		l.listTop = l.chats.y + 2
	}
	if c := l.conv; c.w > 0 {
		// Inside the border: two header lines and a rule, the messages, a
		// rule, and the composer.
		vh := max(1, c.h-2-3-1-composerHeight)
		l.viewport = rect{c.x + 1, c.y + 4, c.w - 2, vh}
		l.composer = rect{c.x + 1, c.y + 4 + vh + 1, c.w - 2, composerHeight}
	}
	return l
}

// resize lays out the panes and sizes the components to fit.
func (m *mainView) resize(a *App) {
	m.layout = m.computeLayout(a)
	l := m.layout
	if l.chats.w > 0 {
		m.list.SetSize(l.chats.w-2, l.chats.h-2)
	}
	if l.viewport.w > 0 {
		m.vp.SetWidth(l.viewport.w)
		m.vp.SetHeight(l.viewport.h)
		m.composer.SetWidth(l.composer.w - 1)
	}
	m.help.SetWidth(min(a.width-6, 96))
	m.refresh(a, false)
}

// activeChat returns the open chat.
func (m *mainView) activeChat() (Chat, bool) {
	for _, c := range m.chats {
		if c.ID == m.active {
			return c, true
		}
	}
	return Chat{}, false
}

// conversationShown reports whether the open chat is on screen, so new
// messages in it count as read.
func (m *mainView) conversationShown() bool {
	return m.active != "" && m.layout.conv.w > 0
}

// refresh redraws the open chat's messages, keeping the view at the latest
// message when it was already there.
func (m *mainView) refresh(a *App, toBottom bool) {
	if m.vp.Width() == 0 {
		return
	}
	atBottom := m.vp.AtBottom()
	content := ""
	if m.active != "" {
		if m.loaded[m.active] {
			content = renderMessages(a.theme, m.messages[m.active], m.vp.Width()-2, a.opts.Now(), a.opts.Location)
		} else {
			content = a.theme.dim.Render("Loading messages")
		}
	}
	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	// A short chat sits at the bottom, next to the composer, like a long one.
	if pad := m.vp.Height() - len(lines); pad > 0 {
		lines = append(make([]string, pad), lines...)
	}
	m.vp.SetContentLines(lines)
	if toBottom || atBottom {
		m.vp.GotoBottom()
	}
}

// setChats replaces the chat list, newest activity first, keeping the
// selection on the same chat.
func (m *mainView) setChats(chats []Chat) tea.Cmd {
	for i := range chats {
		if chats[i].ID == m.active && m.conversationShown() {
			chats[i].Unread = 0
		}
	}
	// Direct chats, group chats, and requests first, newest activity
	// first; then each community, by name, with its channels in their
	// order (§16.4).
	sort.SliceStable(chats, func(i, j int) bool {
		a, b := chats[i], chats[j]
		if a.IsChannel() != b.IsChannel() {
			return !a.IsChannel()
		}
		if a.IsChannel() {
			if a.Community != b.Community {
				return a.Community < b.Community
			}
			if a.CommunityServer != b.CommunityServer {
				return a.CommunityServer < b.CommunityServer
			}
			return a.Order < b.Order
		}
		if !a.LastActivity.Equal(b.LastActivity) {
			return a.LastActivity.After(b.LastActivity)
		}
		return a.Title < b.Title
	})
	m.chats = chats
	var items []list.Item
	server := ""
	for _, c := range chats {
		if c.IsChannel() && c.CommunityServer != server {
			server = c.CommunityServer
			h := communityItem{name: c.Community, server: server, folded: m.folded[server]}
			for _, x := range chats {
				if x.CommunityServer == server {
					h.unread += x.Unread
				}
			}
			items = append(items, h)
		}
		if c.IsChannel() && m.folded[c.CommunityServer] && c.ID != m.active {
			continue
		}
		items = append(items, chatItem{c})
	}
	cmd := m.list.SetItems(items)
	m.selectInList(m.active)
	return cmd
}

// toggleFold folds or unfolds the community under the list's cursor.
func (m *mainView) toggleFold() tea.Cmd {
	h, ok := m.list.SelectedItem().(communityItem)
	if !ok {
		return nil
	}
	m.folded[h.server] = !m.folded[h.server]
	cmd := m.setChats(m.chats)
	for i, it := range m.list.VisibleItems() {
		if x, ok := it.(communityItem); ok && x.server == h.server {
			m.list.Select(i)
		}
	}
	return cmd
}

// selectInList moves the list's cursor to a chat, if it's visible.
func (m *mainView) selectInList(id string) {
	for i, it := range m.list.VisibleItems() {
		if ci, ok := it.(chatItem); ok && ci.chat.ID == id {
			m.list.Select(i)
			return
		}
	}
}

// selectedID returns the chat under the list's cursor.
func (m *mainView) selectedID() string {
	if ci, ok := m.list.SelectedItem().(chatItem); ok {
		return ci.chat.ID
	}
	return ""
}

// openChat shows a chat, loads its messages if needed, and marks it read.
func (m *mainView) openChat(a *App, id string, toComposer bool) tea.Cmd {
	var cmds []tea.Cmd
	if id != m.active {
		if m.active != "" && m.editing == "" {
			m.drafts[m.active] = m.composer.Value()
		}
		m.replyTo, m.editing = "", ""
		m.active = id
		m.composer.SetValue(m.drafts[id])
		if !m.loaded[id] {
			cmds = append(cmds, m.loadMessages(a, id))
		}
	}
	m.selectInList(id)
	if c, ok := m.activeChat(); ok {
		m.updatePlaceholder()
		if c.Unread > 0 {
			for i := range m.chats {
				if m.chats[i].ID == id {
					m.chats[i].Unread = 0
				}
			}
			cmds = append(cmds, m.setChats(m.chats), m.markRead(a, id))
		}
	}
	if c, ok := m.activeChat(); ok && c.IsRequest() {
		toComposer = false
	}
	if toComposer {
		m.narrowDetails = false
		cmds = append(cmds, m.setFocus(a, focusComposer))
	} else {
		m.resize(a)
	}
	m.refresh(a, true)
	return tea.Batch(cmds...)
}

// setFocus moves the keyboard to a pane.
func (m *mainView) setFocus(a *App, f focus) tea.Cmd {
	if c, ok := m.activeChat(); ok && c.IsRequest() && f == focusComposer {
		f = focusMessages
	}
	m.focus = f
	m.resize(a)
	if f == focusComposer && m.active != "" {
		return m.composer.Focus()
	}
	m.composer.Blur()
	return nil
}

// cycleFocus moves to the next or previous pane that's showing.
func (m *mainView) cycleFocus(a *App, step int) tea.Cmd {
	order := []focus{focusChats, focusMessages, focusComposer}
	if m.layout.narrow {
		// In the narrow layout, Tab moves between the messages and the
		// composer. Esc goes back to the chat list.
		if m.focus == focusChats {
			return nil
		}
		order = []focus{focusMessages, focusComposer}
	} else if m.layout.chats.w == 0 {
		order = []focus{focusMessages, focusComposer}
	}
	if m.active == "" {
		order = []focus{focusChats}
	}
	i := 0
	for j, f := range order {
		if f == m.focus {
			i = j
		}
	}
	return m.setFocus(a, order[(i+step+len(order))%len(order)])
}

func (m *mainView) toggleDetails(a *App) {
	if m.layout.narrow {
		m.narrowDetails = !m.narrowDetails
		if m.narrowDetails && m.focus == focusChats {
			m.focus = focusMessages
		}
	} else {
		m.showDetails = !m.showDetails
	}
	m.resize(a)
}

// findChat focuses the chat list and starts filtering it.
func (m *mainView) findChat(a *App) tea.Cmd {
	cmd := m.setFocus(a, focusChats)
	var cmd2 tea.Cmd
	m.list, cmd2 = m.list.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	return tea.Batch(cmd, cmd2)
}

// moveChat opens the next or previous chat in the list.
func (m *mainView) moveChat(a *App, step int) tea.Cmd {
	if step > 0 {
		m.list.CursorDown()
	} else {
		m.list.CursorUp()
	}
	if id := m.selectedID(); id != "" {
		return m.openChat(a, id, false)
	}
	return nil
}

// send sends what's in the composer.
func (m *mainView) send(a *App) tea.Cmd {
	text := strings.TrimSpace(m.composer.Value())
	if text == "" || m.active == "" {
		return nil
	}
	chat := m.active
	m.composer.Reset()
	m.drafts[chat] = ""
	typing := m.stopTyping(a)
	if id := m.editing; id != "" {
		m.editing = ""
		m.updatePlaceholder()
		return tea.Batch(typing, do("", "Railcar couldn't change the message: ", func(ctx context.Context) error {
			return a.b.Edit(ctx, chat, id, text)
		}))
	}
	reply := m.replyTo
	m.replyTo = ""
	m.updatePlaceholder()
	return tea.Batch(typing, call(func(ctx context.Context) tea.Msg {
		msg, err := a.b.Send(ctx, chat, Draft{Text: text, ReplyTo: reply})
		if err != nil {
			msg = Message{Chat: chat, Text: text}
		}
		return sentMsg{chat, msg, err}
	}))
}

// upsert adds a message to a loaded chat, or replaces the one with its ID
// when replace is set.
func (m *mainView) upsert(msg Message, replace bool) {
	if !m.loaded[msg.Chat] {
		return
	}
	msgs := m.messages[msg.Chat]
	for i := range msgs {
		if msgs[i].ID == msg.ID {
			if replace {
				msgs[i] = msg
			}
			return
		}
	}
	msgs = append(msgs, msg)
	sort.SliceStable(msgs, func(i, j int) bool {
		if msgs[i].Seq != 0 && msgs[j].Seq != 0 {
			return msgs[i].Seq < msgs[j].Seq // channels keep sequence order (§16.5)
		}
		return msgs[i].Sent.Before(msgs[j].Sent)
	})
	m.messages[msg.Chat] = msgs
}

func (m *mainView) handleEvent(a *App, e Event) tea.Cmd {
	switch e := e.(type) {
	case ChatChanged:
		var cmds []tea.Cmd
		c := e.Chat
		if c.ID == m.active && m.conversationShown() && c.Unread > 0 {
			cmds = append(cmds, m.markRead(a, c.ID))
		}
		chats := append([]Chat(nil), m.chats...)
		found := false
		for i := range chats {
			if chats[i].ID == c.ID {
				chats[i], found = c, true
			}
		}
		if !found {
			chats = append(chats, c)
		}
		cmds = append(cmds, m.setChats(chats))
		if m.follow != "" && m.active == "" && c.Kind == DirectChat && len(c.Members) == 1 && c.Members[0].Handle == m.follow {
			m.follow = ""
			cmds = append(cmds, m.openChat(a, c.ID, m.focus != focusChats))
		}
		if c.ID == m.active {
			m.updatePlaceholder()
			if c.IsRequest() && m.focus == focusComposer {
				cmds = append(cmds, m.setFocus(a, focusMessages))
			}
		}
		return tea.Batch(cmds...)
	case ChatRemoved:
		chats := make([]Chat, 0, len(m.chats))
		for _, c := range m.chats {
			if c.ID != e.Chat {
				chats = append(chats, c)
			}
		}
		if m.active == e.Chat {
			if c, ok := m.activeChat(); ok && c.IsRequest() && len(c.Members) == 1 {
				m.follow = c.Members[0].Handle
			}
			m.active = ""
			m.composer.Blur()
			m.focus = focusChats
			m.resize(a)
		}
		return m.setChats(chats)
	case Typing:
		cmd := m.setTyping(a, e)
		return cmd
	case MessageAdded:
		if t := m.typing[e.Message.Chat]; t != nil {
			delete(t, e.Message.Sender.Handle)
		}
		m.upsert(e.Message, false)
		if e.Message.Chat == m.active {
			m.refresh(a, e.Message.Mine)
		}
	case MessageChanged:
		m.upsert(e.Message, true)
		if e.Message.Chat == m.active {
			m.refresh(a, false)
		}
	}
	return nil
}

func (m *mainView) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case idleTickMsg:
		if a.identity.Away || m.awayBusy || a.opts.Now().Sub(m.lastInput) < idleAfter {
			return idleTick()
		}
		m.awayBusy = true
		return tea.Batch(idleTick(), call(func(ctx context.Context) tea.Msg {
			_, err := a.b.Act(ctx, "", Action{Kind: "auto-away", Arg: "on"})
			return awayMsg{err}
		}))
	case awayMsg:
		m.awayBusy = false
		return nil
	case tea.WindowSizeMsg:
		m.resize(a)
		return nil
	case chatsMsg:
		if msg.err != nil {
			m.notice = "Railcar couldn't load your chats: " + msg.err.Error()
			return nil
		}
		cmd := m.setChats(msg.chats)
		if m.active == "" && len(m.chats) > 0 && !m.layout.narrow {
			return tea.Batch(cmd, m.openChat(a, m.chats[0].ID, false))
		}
		return cmd
	case messagesMsg:
		if msg.err != nil {
			m.notice = "Railcar couldn't load the messages: " + msg.err.Error()
			return nil
		}
		m.messages[msg.chat] = msg.messages
		m.loaded[msg.chat] = true
		if msg.chat == m.active {
			m.refresh(a, true)
		}
		return nil
	case sentMsg:
		if msg.err != nil {
			m.notice = "Railcar couldn't send the message: " + msg.err.Error()
			if msg.chat == m.active && m.composer.Value() == "" {
				m.composer.SetValue(msg.message.Text)
			}
			return nil
		}
		m.upsert(msg.message, false)
		if msg.chat == m.active {
			m.refresh(a, true)
		}
		return nil
	case noticeMsg:
		m.notice = msg.text
		return nil
	case contactsMsg, groupMadeMsg, actedMsg:
		cmd, _ := m.handleGroupMsg(a, msg)
		return cmd
	case actionMsg:
		if msg.err != nil {
			m.notice = msg.bad + msg.err.Error()
		} else {
			m.notice = msg.ok
		}
		return nil
	case inviteMsg:
		if msg.err != nil {
			m.notice = "Railcar couldn't make an invite code: " + msg.err.Error()
			return nil
		}
		m.notice = ""
		m.inviteCode = msg.code
		m.overlay = overlayInvite
		return tea.SetClipboard(msg.code)
	case addedMsg:
		m.addBusy = false
		if msg.err != nil {
			m.addErr = "Railcar couldn't send the request: " + msg.err.Error()
			return nil
		}
		if m.overlay == overlayAddContact {
			m.overlay = overlayNone
		}
		m.notice = "Request sent. Your chat starts when they accept."
		return nil
	case approvingMsg, approvalTickMsg, approvedMsg, devicesMsg:
		cmd, _ := m.devicesUpdate(a, msg)
		return cmd
	case typingOffMsg:
		if msg.seq == m.typingSeq && msg.chat == m.typingChat {
			return m.stopTyping(a)
		}
		return nil
	case typingExpiredMsg:
		return nil
	case tea.MouseClickMsg:
		return m.click(a, msg.Mouse())
	case tea.MouseWheelMsg:
		return m.wheel(a, msg.Mouse())
	case tea.PasteMsg:
		switch m.overlay {
		case overlayAddDevice, overlayApprovalCode:
			return m.devicePaste(msg)
		case overlayAddContact:
			var cmd tea.Cmd
			m.addInput, cmd = m.addInput.Update(msg)
			return cmd
		case overlayNewGroup:
			if g := &m.grp; g.field < len(g.inputs) {
				var cmd tea.Cmd
				g.inputs[g.field], cmd = g.inputs[g.field].Update(msg)
				return cmd
			}
			return nil
		}
		if m.overlay == overlayPalette {
			var cmd tea.Cmd
			m.palette.input, cmd = m.palette.input.Update(msg)
			m.palette.filter()
			return cmd
		}
		if m.focus == focusComposer {
			var cmd tea.Cmd
			m.composer, cmd = m.composer.Update(msg)
			return cmd
		}
		return nil
	case tea.KeyPressMsg:
		return m.key(a, msg)
	}

	// Everything else, such as the filter's cursor, goes to the components.
	var c1, c2 tea.Cmd
	m.list, c1 = m.list.Update(msg)
	m.composer, c2 = m.composer.Update(msg)
	return tea.Batch(c1, c2)
}

func (m *mainView) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	return tea.Batch(m.noteInput(a), m.keyPress(a, msg))
}

func (m *mainView) keyPress(a *App, msg tea.KeyPressMsg) tea.Cmd {
	k := a.keys
	m.notice = ""

	switch m.overlay {
	case overlayHelp:
		if key.Matches(msg, k.Back, k.Help) || msg.String() == "q" {
			m.overlay = overlayNone
		}
		return nil
	case overlayPalette:
		cmd, done := m.palette.update(a, msg)
		if done && m.overlay == overlayPalette {
			m.overlay = overlayNone
		}
		return cmd
	case overlayAddContact:
		return m.addContactKey(a, msg)
	case overlayNewGroup:
		return m.groupKey(a, msg)
	case overlayAddDevice, overlayApprovalCode:
		return m.addDeviceKey(a, msg)
	case overlayDevices:
		return m.devicesKey(a, msg)
	case overlayInvite:
		if key.Matches(msg, k.Back) || msg.String() == "enter" || msg.String() == "q" {
			m.overlay = overlayNone
		}
		return nil
	case overlaySignOut:
		switch msg.String() {
		case "enter", "y":
			m.overlay = overlayNone
			return call(func(ctx context.Context) tea.Msg {
				return signedOutMsg{a.b.SignOut(ctx)}
			})
		case "esc", "n":
			m.overlay = overlayNone
		}
		return nil
	}

	// While the filter is open, every key goes to it.
	if m.list.SettingFilter() {
		before := m.selectedID()
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		if id := m.selectedID(); !m.list.SettingFilter() && id != "" && (id != before || msg.String() == "enter") {
			return tea.Batch(cmd, m.openChat(a, id, msg.String() == "enter"))
		}
		return cmd
	}

	switch {
	case key.Matches(msg, k.Palette):
		return m.openPalette(a)
	case key.Matches(msg, k.Details):
		m.toggleDetails(a)
		return nil
	case key.Matches(msg, k.NextPane):
		return m.cycleFocus(a, 1)
	case key.Matches(msg, k.PrevPane):
		return m.cycleFocus(a, -1)
	case key.Matches(msg, k.NextChat):
		return m.moveChat(a, 1)
	case key.Matches(msg, k.PrevChat):
		return m.moveChat(a, -1)
	case key.Matches(msg, k.AddContact):
		return m.openAddContact(a, "")
	}

	if m.focus == focusComposer {
		switch {
		case key.Matches(msg, k.Send):
			return m.send(a)
		case key.Matches(msg, k.Back):
			if m.cancelMode() {
				return nil
			}
			return m.setFocus(a, focusMessages)
		}
		before := m.composer.Value()
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		if m.composer.Value() != before && m.editing == "" {
			return tea.Batch(cmd, m.typed(a))
		}
		return cmd
	}

	if c, ok := m.activeChat(); ok && c.Kind == ContactRequest && (m.focus == focusMessages || m.focus == focusChats && c.ID == m.selectedID()) {
		switch {
		case key.Matches(msg, k.Accept):
			return m.acceptRequest(a)
		case key.Matches(msg, k.Decline):
			return m.declineRequest(a)
		}
	}

	switch {
	case key.Matches(msg, k.Help):
		m.overlay = overlayHelp
		return nil
	case key.Matches(msg, k.Command):
		return m.openPalette(a)
	}

	if m.focus == focusChats {
		switch {
		case key.Matches(msg, k.Open):
			if id := m.selectedID(); id != "" {
				return m.openChat(a, id, true)
			}
			return m.toggleFold()
		case msg.String() == "right" || msg.String() == "l":
			if m.active != "" && !m.layout.narrow {
				return m.setFocus(a, focusMessages)
			}
			return nil
		case key.Matches(msg, k.Back):
			if m.list.IsFiltered() {
				m.list.ResetFilter()
			}
			return nil
		}
		before := m.selectedID()
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		if id := m.selectedID(); id != before && id != "" && !m.layout.narrow {
			return tea.Batch(cmd, m.openChat(a, id, false))
		}
		return cmd
	}

	// The message view.
	switch {
	case key.Matches(msg, k.Reply):
		return m.startReply(a)
	case key.Matches(msg, k.EditLast):
		return m.startEdit(a)
	case key.Matches(msg, k.Back):
		if m.narrowDetails {
			m.narrowDetails = false
			m.resize(a)
			return nil
		}
		return m.setFocus(a, focusChats)
	case key.Matches(msg, k.Open):
		return m.setFocus(a, focusComposer)
	case msg.String() == "left" || msg.String() == "h":
		if !m.layout.narrow && m.layout.chats.w > 0 {
			return m.setFocus(a, focusChats)
		}
		return nil
	case key.Matches(msg, k.Top):
		m.vp.GotoTop()
		return nil
	case key.Matches(msg, k.Bottom):
		m.vp.GotoBottom()
		return nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *mainView) openPalette(a *App) tea.Cmd {
	m.palette = newCommandPalette(a, m)
	m.overlay = overlayPalette
	return nil
}

// click focuses the pane under the pointer, and opens a chat clicked in the
// list.
func (m *mainView) click(a *App, mouse tea.Mouse) tea.Cmd {
	m.lastInput = a.opts.Now()
	if m.overlay != overlayNone || mouse.Button != tea.MouseLeft {
		return nil
	}
	l := m.layout
	switch {
	case l.chats.contains(mouse.X, mouse.Y):
		row := mouse.Y - l.listTop
		if row < 0 {
			return m.setFocus(a, focusChats)
		}
		per := m.list.Paginator.PerPage
		slot := row / 3
		if row%3 == 2 || slot >= per {
			return m.setFocus(a, focusChats)
		}
		i := m.list.Paginator.Page*per + slot
		visible := m.list.VisibleItems()
		if i >= len(visible) {
			return m.setFocus(a, focusChats)
		}
		m.list.Select(i)
		id := visible[i].(chatItem).chat.ID
		cmd := m.setFocus(a, focusChats)
		if l.narrow {
			return tea.Batch(cmd, m.openChat(a, id, true))
		}
		return tea.Batch(cmd, m.openChat(a, id, false))
	case l.composer.contains(mouse.X, mouse.Y):
		return m.setFocus(a, focusComposer)
	case l.conv.contains(mouse.X, mouse.Y):
		if m.active != "" {
			return m.setFocus(a, focusMessages)
		}
	}
	return nil
}

// wheel scrolls the messages, or moves through the chat list.
func (m *mainView) wheel(a *App, mouse tea.Mouse) tea.Cmd {
	if m.overlay != overlayNone {
		return nil
	}
	up := mouse.Button == tea.MouseWheelUp
	down := mouse.Button == tea.MouseWheelDown
	l := m.layout
	switch {
	case l.chats.contains(mouse.X, mouse.Y):
		if up {
			m.list.CursorUp()
		} else if down {
			m.list.CursorDown()
		}
		if id := m.selectedID(); id != "" && !l.narrow {
			return m.openChat(a, id, false)
		}
	case l.conv.contains(mouse.X, mouse.Y):
		if up {
			m.vp.ScrollUp(3)
		} else if down {
			m.vp.ScrollDown(3)
		}
	}
	return nil
}

// fit pads or cuts s to exactly w by h cells.
func fit(s string, w, h int) string {
	return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h).Render(s)
}

// box draws a pane's border around content of the inner size.
func box(t *theme, focused bool, content string, r rect) string {
	st := t.pane
	if focused {
		st = t.paneFocused
	}
	return st.Render(fit(content, r.w-2, r.h-2))
}

func (m *mainView) view(a *App) string {
	t := a.theme
	l := m.layout
	var panes []string
	if l.chats.w > 0 {
		panes = append(panes, box(t, m.focus == focusChats, m.chatsView(t), l.chats))
	}
	if l.conv.w > 0 {
		panes = append(panes, box(t, m.focus != focusChats, m.convView(a), l.conv))
	}
	if l.details.w > 0 {
		panes = append(panes, box(t, false, m.detailsView(a, l.details.w-2), l.details))
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, panes...)
	screen := lipgloss.JoinVertical(lipgloss.Left, body, m.statusBar(a))

	switch m.overlay {
	case overlayHelp:
		return overlayOn(screen, m.helpView(a), a.width, a.height)
	case overlayPalette:
		w := min(56, a.width-6)
		content := m.palette.view(t, w, min(10, max(3, a.height-10)))
		return overlayOn(screen, t.overlay.Render(fit(content, w, lipgloss.Height(content))), a.width, a.height)
	case overlaySignOut:
		return overlayOn(screen, m.signOutView(a), a.width, a.height)
	case overlayAddContact:
		return overlayOn(screen, m.addContactView(a), a.width, a.height)
	case overlayInvite:
		return overlayOn(screen, m.inviteView(a), a.width, a.height)
	case overlayAddDevice:
		return overlayOn(screen, m.addDeviceView(a), a.width, a.height)
	case overlayApprovalCode:
		return overlayOn(screen, m.approvalCodeView(a), a.width, a.height)
	case overlayDevices:
		return overlayOn(screen, m.devicesView(a), a.width, a.height)
	case overlayNewGroup:
		return overlayOn(screen, m.groupView(a), a.width, a.height)
	}
	return screen
}

func (m *mainView) chatsView(t *theme) string {
	if len(m.chats) == 0 {
		return t.title.PaddingLeft(1).Render("Chats") + "\n\n" + t.dim.PaddingLeft(1).Render("No chats yet")
	}
	if m.focus == focusChats {
		m.list.Styles.Title = t.titleFocused
	} else {
		m.list.Styles.Title = t.title
	}
	return m.list.View()
}

func (m *mainView) convView(a *App) string {
	t := a.theme
	l := m.layout
	w := l.conv.w - 2
	c, ok := m.activeChat()
	if !ok {
		msg := "Select a chat to read it."
		if len(m.chats) == 0 {
			msg = "No chats yet.\n\nTo add a contact, press Ctrl+N and paste their code.\nTo make a code to give someone, type :invite."
			if a.opts.Demo {
				msg = "No chats yet."
			}
		}
		return lipgloss.Place(w, l.conv.h-2, lipgloss.Center, lipgloss.Center, t.dim.Render(msg))
	}

	title := t.title
	if m.focus != focusChats {
		title = t.titleFocused
	}
	hint := t.dim.Render("ctrl+o details")
	name := ansi.Truncate(c.Title, max(1, w-lipgloss.Width(hint)-3), "…")
	gap := max(1, w-2-lipgloss.Width(name)-lipgloss.Width(hint))
	if l.narrow || w < 50 {
		hint, gap = "", 0
	}
	header := " " + title.Render(name) + strings.Repeat(" ", gap) + hint + " "
	subText := chatSubtitle(c)
	switch c.Kind {
	case ContactRequest:
		subText = "Contact request"
	case SentRequest:
		subText = "Your contact request"
	}
	if typing := m.typingLine(a, c.ID); typing != "" {
		subText = typing
	} else if c.Stopped != "" {
		subText = c.Stopped
	}
	sub := " " + t.muted.Render(ansi.Truncate(subText, max(1, w-2), "…"))
	rule := t.dim.Foreground(t.p.line).Render(strings.Repeat("─", w))
	if c.IsRequest() {
		return strings.Join([]string{header, sub, rule, m.requestView(a, c, w, l.conv.h-5)}, "\n")
	}
	if c.Kind == VoiceChannel {
		return strings.Join([]string{header, sub, rule, voiceView(a, c, w, l.conv.h-5)}, "\n")
	}

	return strings.Join([]string{
		header,
		sub,
		rule,
		fit(m.vp.View(), l.viewport.w, l.viewport.h),
		rule,
		fit(m.composer.View(), l.composer.w, l.composer.h),
	}, "\n")
}

// chatSubtitle is the line under a chat's title: the contact's handle, or
// the group's members.
func chatSubtitle(c Chat) string {
	if c.IsChannel() {
		if c.Topic != "" {
			return c.Topic
		}
		return c.Community
	}
	if c.Kind == GroupChat && c.Topic != "" {
		return c.Topic
	}
	if c.Kind == DirectChat && len(c.Members) == 1 {
		return "@" + c.Members[0].Handle
	}
	names := make([]string, 0, len(c.Members)+1)
	for _, mb := range c.Members {
		n := mb.Name
		if n == "" {
			n = "@" + mb.Handle
		}
		names = append(names, firstName(n))
	}
	names = append(names, "you")
	if len(names) == 2 {
		return names[0] + " and you"
	}
	return strings.Join(names[:len(names)-1], ", ") + ", and you"
}

func firstName(n string) string {
	if strings.HasPrefix(n, "@") {
		return n
	}
	f, _, _ := strings.Cut(n, " ")
	return f
}

func (m *mainView) detailsView(a *App, w int) string {
	t := a.theme
	var b strings.Builder
	b.WriteString(" " + t.title.Render("Details") + "\n\n")
	c, ok := m.activeChat()
	if !ok {
		b.WriteString(" " + t.dim.Render("No chat open"))
		return b.String()
	}
	line := func(s string) { b.WriteString(" " + ansi.Truncate(s, w-2, "…") + "\n") }
	line(t.bold.Render(c.Title))
	switch c.Kind {
	case DirectChat:
		line(t.muted.Render("Direct chat"))
	case GroupChat:
		line(t.muted.Render("Group chat, " + plural(len(c.Members)+1, "member", "members")))
	case ChannelChat, VoiceChannel:
		kind := "Text channel"
		if c.Kind == VoiceChannel {
			kind = "Voice channel"
		}
		line(t.muted.Render(kind + " in " + c.Community))
		line(t.dim.Render(c.CommunityServer))
	}
	if c.Topic != "" {
		b.WriteString("\n")
		line(t.dim.Render("Topic"))
		b.WriteString(t.text.Width(w-1).PaddingLeft(1).Render(c.Topic) + "\n")
	}
	b.WriteString("\n")
	if c.Kind == DirectChat && len(c.Members) == 1 {
		profileBlock(&b, t, c.Members[0], w)
	} else if c.Kind == DirectChat {
		line(t.dim.Render("Handle"))
	} else {
		line(t.dim.Render("Members"))
	}
	for _, mb := range c.Members {
		if c.Kind == DirectChat {
			if len(c.Members) != 1 {
				line(t.muted.Render("@" + mb.Handle))
			}
			continue
		}
		label := mb.Name
		if label == "" {
			label = "@" + mb.Handle
		}
		switch {
		case mb.Admin:
			label += " (admin)"
		case mb.Roles != "":
			label += " (" + mb.Roles + ")"
		}
		if p := presenceWord(mb.Presence); p != "" {
			label += " · " + p
		}
		line(t.text.Render(label))
		if mb.Name != "" {
			line(t.muted.Render("@" + mb.Handle))
		}
	}
	if c.Kind != DirectChat {
		you := "You"
		if c.Kind == GroupChat && c.Admin {
			you += " (admin)"
		}
		line(t.text.Render(you))
		line(t.muted.Render("@" + a.identity.Handle))
	}
	if len(c.Requests) > 0 {
		b.WriteString("\n")
		line(t.dim.Render("Waiting for an admin"))
		for _, r := range c.Requests {
			line(t.text.Render("@" + r.From.Handle + " wants to add @" + r.Who.Handle))
		}
		if c.Admin {
			line(t.muted.Render(":approve or :decline-add"))
		}
	}
	b.WriteString("\n")
	if a.opts.Demo {
		b.WriteString(t.away.Width(w - 1).PaddingLeft(1).Render("Demo data. Nothing here is sent or encrypted."))
	} else {
		b.WriteString(t.dim.Width(w - 1).PaddingLeft(1).Render("Messages here are end-to-end encrypted (E2EE) with MLS."))
	}
	return b.String()
}

// profileBlock shows someone's public profile and presence in the details
// pane (§13): their initials in place of a picture, which terminals can't
// show well, their display name and handle, presence and status, bio, and
// links with their full domain.
func profileBlock(b *strings.Builder, t *theme, mb Member, w int) {
	line := func(s string) { b.WriteString(" " + ansi.Truncate(s, w-2, "…") + "\n") }
	pic := t.avatar.Render(" " + initials(mb.Name, mb.Handle) + " ")
	name := mb.Name
	if name == "" {
		name = "@" + mb.Handle
	}
	line(pic + " " + t.bold.Render(name))
	if mb.Name != "" {
		line(strings.Repeat(" ", lipgloss.Width(pic)+1) + t.muted.Render("@"+mb.Handle))
	}
	if p := presenceWord(mb.Presence); p != "" {
		mark := "●"
		if mb.Presence == "offline" {
			mark = "○"
		}
		line(t.muted.Render(mark + " " + p))
	}
	if mb.Status != "" {
		b.WriteString(t.text.Width(w-1).PaddingLeft(1).Render("“"+mb.Status+"”") + "\n")
	}
	if mb.Bio != "" {
		b.WriteString("\n")
		line(t.dim.Render("Bio"))
		b.WriteString(t.text.Width(w-1).PaddingLeft(1).Render(mb.Bio) + "\n")
	}
	if len(mb.Links) > 0 {
		b.WriteString("\n")
		line(t.dim.Render("Links"))
		for _, l := range mb.Links {
			// The full domain first, so a link can't pass for another
			// site (§13.1).
			host := strings.TrimPrefix(l, "https://")
			host, _, _ = strings.Cut(host, "/")
			line(t.text.Render(host))
			line(t.muted.Render(l))
		}
	}
}

// initials are up to two letters for someone's picture: from their
// display name's first two words, or their handle's first letter.
func initials(name, handle string) string {
	var out []rune
	for _, f := range strings.Fields(name) {
		r := []rune(f)
		if len(r) > 0 && len(out) < 2 {
			out = append(out, unicode.ToUpper(r[0]))
		}
	}
	if len(out) == 0 && handle != "" {
		out = append(out, unicode.ToUpper([]rune(handle)[0]))
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// presenceWord is a presence state in words (§13.2).
func presenceWord(state string) string {
	switch state {
	case "available":
		return "Available"
	case "online":
		return "Online"
	case "away":
		return "Away"
	case "busy":
		return "Busy"
	case "offline":
		return "Offline"
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// statusBar shows the identity, its homeserver, and the stream's state.
func (m *mainView) statusBar(a *App) string {
	t := a.theme
	w := a.width
	seg := func(st lipgloss.Style, s string) string { return st.Render(s) }

	var left string
	if m.notice != "" {
		left = seg(t.status, " ") + seg(t.statusValue, m.notice)
	} else {
		left = seg(t.status, " ") + seg(t.statusValue, "@"+a.identity.Handle)
		if !m.layout.narrow && a.identity.Homeserver != "" {
			left += seg(t.statusKey, "  on ") + seg(t.status, a.identity.Homeserver)
		}
		if p := a.identity.Presence; p != "" && p != "online" || a.identity.Away {
			if a.identity.Away {
				p = "away"
			}
			left += seg(t.status, "  "+presenceWord(p))
		}
		if a.opts.Demo {
			left += seg(t.status, "  ") + seg(t.away.Background(t.p.panel), "Demo")
		}
	}

	var state string
	switch a.stream {
	case StreamConnected:
		state = seg(t.ok.Background(t.p.panel), "●") + seg(t.status, " Connected")
	case StreamConnecting, StreamReconnecting:
		sp := a.spinner
		sp.Style = sp.Style.Background(t.p.panel)
		state = sp.View() + seg(t.status, " "+a.stream.String())
	default:
		state = seg(t.danger.Background(t.p.panel), "○") + seg(t.status, " Offline")
	}
	right := state
	if !m.layout.narrow {
		right += seg(t.statusKey, "   ? help  ctrl+p commands")
	}
	right += seg(t.status, " ")

	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if lw+rw+1 > w {
		left = ansi.Truncate(left, max(0, w-rw-1), "…")
		lw = lipgloss.Width(left)
	}
	return left + seg(t.status, strings.Repeat(" ", max(0, w-lw-rw))) + right
}

func (m *mainView) helpView(a *App) string {
	t := a.theme
	w := min(a.width-6, 96)
	m.help.SetWidth(w)
	content := t.bold.Render("Keys") + "\n\n" +
		m.help.FullHelpView(a.keys.fullHelp()) + "\n\n" +
		t.muted.Width(w).Render("With a mouse, click a chat to open it, and scroll to move through messages or chats. "+
			"Type : or press Ctrl+P for commands, such as :sign-out and :quit.") + "\n\n" +
		t.dim.Render("Press Esc to close.")
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

func (m *mainView) signOutView(a *App) string {
	t := a.theme
	w := min(a.width-6, 56)
	content := t.bold.Render("Sign out of @"+a.identity.Handle+"?") + "\n\n" +
		t.muted.Width(w).Render("Railcar removes the identity from this device. To sign in again, use another of your devices, or your recovery code.") + "\n\n" +
		t.accent.Render("enter") + t.muted.Render(" sign out   ") + t.accent.Render("esc") + t.muted.Render(" cancel")
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// overlayOn draws box over base, centered across and a third of the way
// down.
func overlayOn(base, box string, w, h int) string {
	bw, bh := lipgloss.Width(box), lipgloss.Height(box)
	x, y := max(0, (w-bw)/2), max(0, (h-bh)/3)
	c := lipgloss.NewCompositor(lipgloss.NewLayer(base), lipgloss.NewLayer(box).X(x).Y(y).Z(1))
	canvas := lipgloss.NewCanvas(w, h)
	canvas.Compose(c)
	return canvas.Render()
}

// voiceView fills the conversation pane for a voice channel: who's in it.
// Railcar can't join voice channels.
func voiceView(a *App, c Chat, w, h int) string {
	t := a.theme
	lines := []string{t.bold.Render("Railcar can't join voice channels."), "",
		t.muted.Width(w - 4).Render("Voice needs audio, which a terminal doesn't have. Join it from Reroute, or another Tieroute client with voice.")}
	if len(c.InVoice) > 0 {
		lines = append(lines, "", t.dim.Render("In the channel now"))
		for _, mb := range c.InVoice {
			lines = append(lines, t.text.Render("@"+mb.Handle))
		}
	} else {
		lines = append(lines, "", t.dim.Render("Nobody is in it now."))
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
}
