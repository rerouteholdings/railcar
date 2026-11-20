package ui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// audioState is the state of the **Audio devices** overlay, which picks
// the microphone and speakers for calls and voice channels.
type audioState struct {
	setup  AudioSetup
	rows   []audioRow
	cursor int
	busy   bool
	err    string
}

// audioRow is one choice in the overlay: a device, or the system's
// default when name is empty.
type audioRow struct {
	speakers bool
	name     string
	gone     bool // chosen, but not connected now
}

type audioMsg struct {
	setup AudioSetup
	err   error
}

type audioSetMsg struct{ err error }

// openAudio shows the audio devices.
func (m *mainView) openAudio(a *App) tea.Cmd {
	m.audio = audioState{busy: true}
	m.overlay = overlayAudio
	return call(func(ctx context.Context) tea.Msg {
		s, err := a.b.AudioDevices(ctx)
		return audioMsg{s, err}
	})
}

// audioUpdate handles the overlay's messages, and reports whether it
// handled msg.
func (m *mainView) audioUpdate(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case audioMsg:
		m.audio.busy = false
		m.audio.setup = msg.setup
		if msg.err != nil {
			m.audio.err = msg.err.Error()
		}
		m.audio.rows = audioRows(msg.setup)
		return true
	case audioSetMsg:
		m.audio.busy = false
		if msg.err != nil {
			m.audio.err = "Railcar couldn't save that: " + msg.err.Error()
		} else {
			m.audio.err = ""
			m.notice = "Calls and voice channels you join from now on use it"
		}
		return true
	}
	return false
}

// audioRows lists the choices: the default, then each device, for the
// microphone and then the speakers. A chosen device that isn't connected
// stays in the list.
func audioRows(s AudioSetup) []audioRow {
	var rows []audioRow
	add := func(speakers bool, names []string, chosen string) {
		rows = append(rows, audioRow{speakers: speakers})
		found := chosen == ""
		for _, n := range names {
			rows = append(rows, audioRow{speakers: speakers, name: n})
			found = found || n == chosen
		}
		if !found {
			rows = append(rows, audioRow{speakers: speakers, name: chosen, gone: true})
		}
	}
	add(false, s.Microphones, s.Microphone)
	add(true, s.Speakers, s.Speaker)
	return rows
}

func (m *mainView) audioKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	st := &m.audio
	switch msg.String() {
	case "esc", "q":
		m.overlay = overlayNone
	case "up", "k":
		st.cursor = max(0, st.cursor-1)
	case "down", "j":
		st.cursor = min(max(0, len(st.rows)-1), st.cursor+1)
	case "enter", "space":
		if st.busy || st.cursor >= len(st.rows) {
			return nil
		}
		r := st.rows[st.cursor]
		if r.speakers {
			st.setup.Speaker = r.name
		} else {
			st.setup.Microphone = r.name
		}
		st.busy = true
		mic, spk := st.setup.Microphone, st.setup.Speaker
		return call(func(ctx context.Context) tea.Msg {
			return audioSetMsg{a.b.SetAudioDevices(ctx, mic, spk)}
		})
	}
	return nil
}

// audioView draws the overlay.
func (m *mainView) audioView(a *App) string {
	t := a.theme
	st := &m.audio
	w := min(a.width-6, 64)
	var b strings.Builder
	b.WriteString(t.bold.Render("Audio devices") + "\n\n")
	if st.busy && len(st.rows) == 0 {
		b.WriteString(a.spinner.View() + " " + t.muted.Render("Loading") + "\n\n")
	}
	for i, r := range st.rows {
		if i == 0 || r.speakers && !st.rows[i-1].speakers {
			if i > 0 {
				b.WriteString("\n")
			}
			head := "Microphone"
			if r.speakers {
				head = "Speakers"
			}
			b.WriteString(t.muted.Render(head) + "\n")
		}
		name := r.name
		if name == "" {
			name = "System default"
		}
		chosen := st.setup.Microphone
		if r.speakers {
			chosen = st.setup.Speaker
		}
		note := ""
		if r.name == chosen {
			note = "  (chosen)"
		}
		if r.gone {
			note = "  (chosen, not connected now, so calls use the default)"
		}
		if i == st.cursor {
			b.WriteString(t.paletteSel.Render("> "+name) + t.dim.Render(note) + "\n")
		} else {
			b.WriteString("  " + name + t.dim.Render(note) + "\n")
		}
	}
	b.WriteString("\n")
	if st.err != "" {
		b.WriteString(t.danger.Width(w).Render(st.err) + "\n\n")
	}
	b.WriteString(t.muted.Width(w).Render("A call or voice channel going on keeps the devices it started with.") + "\n\n")
	b.WriteString(t.accent.Render("enter") + t.muted.Render(" choose   ") + t.accent.Render("esc") + t.muted.Render(" close"))
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}
