package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rerouteholdings/railcar/internal/chat"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/ui"
)

// actFiles runs the file actions (§14), and reports whether a.Kind was one
// of them.
func actFiles(ctx context.Context, eng *chat.Engine, chatID string, a ui.Action) (string, bool, error) {
	switch a.Kind {
	case "attach":
		if chatID == "" || strings.HasPrefix(chatID, requestPrefix) {
			return "", true, errors.New("open a chat to send a file")
		}
		path := expandHome(strings.Trim(strings.TrimSpace(a.Arg), `"'`))
		if path == "" {
			return "", true, errors.New("name a file, as :attach ~/Pictures/map.png")
		}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			return "", true, fmt.Errorf("Railcar can't read %s", a.Arg)
		}
		if st.Size() > client.MaxFile {
			return "", true, errors.New("files are at most 64 MiB")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", true, fmt.Errorf("Railcar can't read %s", a.Arg)
		}
		if _, err := eng.SendFile(ctx, chatID, filepath.Base(path), data); err != nil {
			return "", true, fileError(err)
		}
		return "Sent " + filepath.Base(path), true, nil
	case "save":
		msgs, err := eng.Messages(chatID)
		if err != nil {
			return "", true, err
		}
		var m *chat.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			if len(msgs[i].Files) > 0 && !msgs[i].Deleted {
				m = &msgs[i]
				break
			}
		}
		if m == nil {
			return "", true, errors.New("there's no file in this chat to save")
		}
		dir := expandHome(strings.TrimSpace(a.Arg))
		if dir == "" {
			dir = downloads()
		}
		var saved []string
		for i := range m.Files {
			p, err := eng.SaveFile(ctx, chatID, m.ID, i, dir)
			if err != nil {
				return "", true, fileError(err)
			}
			saved = append(saved, p)
		}
		if len(saved) == 1 {
			return "Saved to " + saved[0], true, nil
		}
		return fmt.Sprintf("Saved %d files to %s", len(saved), dir), true, nil
	}
	return "", false, nil
}

// fileError puts the file errors in words (§14).
func fileError(err error) error {
	switch {
	case errors.Is(err, client.ErrFileExpired):
		return errors.New("This file has expired")
	case errors.Is(err, client.ErrFileRemoved):
		return errors.New("This file was removed")
	case errors.Is(err, client.ErrFileTooLarge):
		return errors.New("Files are at most 64 MiB")
	}
	return err
}

// downloads is the folder files are saved to: Downloads in the home
// folder when there is one, or the home folder.
func downloads() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	if st, err := os.Stat(filepath.Join(h, "Downloads")); err == nil && st.IsDir() {
		return filepath.Join(h, "Downloads")
	}
	return h
}
