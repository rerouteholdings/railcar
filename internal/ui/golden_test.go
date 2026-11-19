package ui

import (
	"bytes"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"
)

// testNow is the clock for every test: Wednesday, November 18, 2026, at
// 3:30 PM UTC.
var testNow = time.Date(2026, 11, 18, 15, 30, 0, 0, time.UTC)

// newTestApp returns the interface on a demo backend with a fixed clock,
// time zone, and random source.
func newTestApp(t *testing.T, signedOut bool) (*App, *DemoBackend) {
	t.Helper()
	now := func() time.Time { return testNow }
	b := NewDemoBackend(DemoOptions{Now: now, Location: time.UTC, SignedOut: signedOut, Seed: 7})
	t.Cleanup(func() { b.Close() })
	noColor := false
	a := New(Options{
		Backend:  b,
		Now:      now,
		Location: time.UTC,
		Rand:     rand.New(rand.NewPCG(1, 2)),
		Demo:     true,
		NoColor:  &noColor,
	})
	return a, b
}

// screenText is the app's current frame as plain text, for golden files.
func screenText(a *App) []byte {
	lines := strings.Split(ansi.Strip(a.View().Content), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// waitText waits until the program has drawn every one of the strings.
func waitText(t *testing.T, tm *teatest.TestModel, want ...string) {
	t.Helper()
	var seen bytes.Buffer
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		seen.Write(b)
		plain := ansi.Strip(seen.String())
		for _, w := range want {
			if !strings.Contains(plain, w) {
				return false
			}
		}
		return true
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
}

// finish quits the program and returns the final model.
func finish(t *testing.T, tm *teatest.TestModel) *App {
	t.Helper()
	// Give the program a moment to apply any events still queued.
	time.Sleep(100 * time.Millisecond)
	if err := tm.Quit(); err != nil {
		t.Fatal(err)
	}
	m := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	go io.Copy(io.Discard, tm.Output())
	return m.(*App)
}

func press(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func TestGoldenOnboarding(t *testing.T) {
	a, _ := newTestApp(t, true)
	tm := teatest.NewTestModel(t, a, teatest.WithInitialTermSize(100, 30))
	waitText(t, tm, "Create an identity")
	golden.RequireEqual(t, screenText(finish(t, tm)))
}

func TestGoldenRecoveryWords(t *testing.T) {
	a, _ := newTestApp(t, true)
	tm := teatest.NewTestModel(t, a, teatest.WithInitialTermSize(100, 34))
	waitText(t, tm, "Create an identity")
	tm.Send(press(tea.KeyEnter))
	waitText(t, tm, "Registration code")
	tm.Type("chat.example.com")
	tm.Send(press(tea.KeyEnter))
	tm.Type("ravi.chat.example.com")
	tm.Send(press(tea.KeyEnter))
	tm.Type("RC-4821")
	tm.Send(press(tea.KeyEnter))
	waitText(t, tm, "written them down")
	golden.RequireEqual(t, screenText(finish(t, tm)))
}

func TestGoldenMainWide(t *testing.T) {
	a, _ := newTestApp(t, false)
	tm := teatest.NewTestModel(t, a, teatest.WithInitialTermSize(120, 40))
	waitText(t, tm, "Trail crew", "Connected")
	golden.RequireEqual(t, screenText(finish(t, tm)))
}

func TestGoldenMainNarrow(t *testing.T) {
	a, _ := newTestApp(t, false)
	tm := teatest.NewTestModel(t, a, teatest.WithInitialTermSize(70, 30))
	waitText(t, tm, "Trail crew", "Connected")
	golden.RequireEqual(t, screenText(finish(t, tm)))
}

func TestGoldenSend(t *testing.T) {
	a, _ := newTestApp(t, false)
	tm := teatest.NewTestModel(t, a, teatest.WithInitialTermSize(120, 40))
	waitText(t, tm, "Trail crew", "Connected")
	// Open the chat with Ravi, which is first, and write in it.
	tm.Send(press(tea.KeyEnter))
	tm.Type("The trailhead is open. See you at 7.")
	tm.Send(press(tea.KeyEnter))
	waitText(t, tm, "See you at 7.")
	golden.RequireEqual(t, screenText(finish(t, tm)))
}
