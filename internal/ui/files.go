package ui

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// openAttach shows the file picker, to send a file to the open chat
// (§14).
func (m *mainView) openAttach(a *App) tea.Cmd {
	c, ok := m.activeChat()
	if !ok || c.IsRequest() || c.Kind == VoiceChannel {
		m.notice = "Open a chat to send a file"
		return nil
	}
	fp := filepicker.New()
	fp.ShowHidden = false
	fp.ShowPermissions = false
	fp.ShowSize = true
	fp.AutoHeight = false
	fp.SetHeight(max(5, min(a.height-12, 20)))
	if dir, err := os.UserHomeDir(); err == nil {
		fp.CurrentDirectory = dir
	}
	st := filepicker.DefaultStyles()
	st.Cursor = a.theme.accent
	st.Selected = a.theme.accent.Bold(true)
	st.Directory = a.theme.text.Bold(true)
	st.File = a.theme.text
	st.FileSize = a.theme.dim
	st.EmptyDirectory = a.theme.dim
	fp.Styles = st
	m.picker, m.pickChat = fp, c.ID
	m.overlay = overlayAttach
	return m.picker.Init()
}

// attachKey handles a key in the file picker.
func (m *mainView) attachKey(a *App, msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "esc" || msg.String() == "q" {
		m.overlay = overlayNone
		return nil
	}
	return m.pickerUpdate(a, msg)
}

// pickerUpdate passes a message to the file picker, and sends the file
// once one is chosen.
func (m *mainView) pickerUpdate(a *App, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)
	if ok, path := m.picker.DidSelectFile(msg); ok {
		m.overlay = overlayNone
		return tea.Batch(cmd, m.act(a, "attach", path, "", false))
	}
	return cmd
}

func (m *mainView) attachView(a *App) string {
	t := a.theme
	w := min(a.width-6, 80)
	var b strings.Builder
	b.WriteString(t.bold.Render("Send a file") + "\n")
	b.WriteString(t.dim.Render(m.picker.CurrentDirectory) + "\n\n")
	b.WriteString(m.picker.View() + "\n\n")
	b.WriteString(t.muted.Width(w).Render("Files are end-to-end encrypted (E2EE), at most 64 MiB, and kept for 30 days.") + "\n\n")
	b.WriteString(t.accent.Render("enter") + t.muted.Render(" send or open   ") + t.accent.Render("←") + t.muted.Render(" up a folder   ") + t.accent.Render("esc") + t.muted.Render(" cancel"))
	content := b.String()
	return t.overlay.Render(fit(content, w, lipgloss.Height(content)))
}

// attachmentLines shows a message's files: what each is, its name, its
// size, an image's size in pixels when Railcar knows it, and where it was
// saved. A voice message is an audio file, shown as one (§14).
func attachmentLines(t *theme, files []Attachment, width int) []string {
	var out []string
	for i, f := range files {
		kind := "File"
		switch {
		case strings.HasPrefix(f.MIME, "image/"):
			kind = "Image"
		case strings.HasPrefix(f.MIME, "audio/"):
			kind = "Voice message"
		case strings.HasPrefix(f.MIME, "video/"):
			kind = "Video"
		}
		parts := []string{}
		if f.Name != "" {
			parts = append(parts, f.Name)
		}
		parts = append(parts, sizeText(f.Size))
		if f.Width > 0 && f.Height > 0 {
			parts = append(parts, fmt.Sprintf("%d × %d", f.Width, f.Height))
		}
		line := t.accent.Render("▪ "+kind) + t.muted.Render(": "+strings.Join(parts, ", "))
		switch {
		case f.Gone == "expired":
			line += t.dim.Render(". This file has expired")
		case f.Gone == "removed":
			line += t.dim.Render(". This file was removed")
		case f.Saved != "":
			line += t.dim.Render(". Saved to " + f.Saved)
		case i == 0:
			line += t.dim.Render(". :save to download")
		}
		out = append(out, lipgloss.NewStyle().Width(width).Render(line))
	}
	return out
}

// sizeText is a file's size in bytes, KiB, or MiB.
func sizeText(n uint64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}
