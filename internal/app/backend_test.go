package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/ui"
)

func init() { keyring.MockInit() }

func wait(t *testing.T, b *Backend, what string, match func(ui.Event) bool) ui.Event {
	t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case e := <-b.Events():
			if match(e) {
				return e
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func signUp(t *testing.T, o map[string]string, server, handle string) (*Backend, string) {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), handle)
	b := New(Options{ProfileDir: dir, Client: client.Options{Origins: o, DeviceName: "Railcar test"}})
	t.Cleanup(func() { b.Close() })
	if _, err := b.Open(ctx); !errors.Is(err, ui.ErrNoIdentity) {
		t.Fatalf("a new profile: %v", err)
	}
	// A wrong registration code is refused after the words, and the next
	// SignUp keeps the same words.
	id, words, err := b.SignUp(ctx, ui.SignUpRequest{Homeserver: server, Handle: handle, RegistrationCode: "wrong-code"})
	if err != nil || len(words) != 12 || id.Handle != handle+"."+server {
		t.Fatalf("%+v %v %v", id, words, err)
	}
	if err := b.FinishSignUp(ctx); !errors.Is(err, ui.ErrSignUpRefused) {
		t.Fatalf("a wrong code: %v", err)
	}
	_, again, err := b.SignUp(ctx, ui.SignUpRequest{Homeserver: server, Handle: handle, RegistrationCode: testnet.SignupCode})
	if err != nil || strings.Join(again, " ") != strings.Join(words, " ") {
		t.Fatalf("the second SignUp: %v %v", again, err)
	}
	if err := b.FinishSignUp(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	wait(t, b, "the stream", func(e ui.Event) bool { s, ok := e.(ui.StreamChanged); return ok && s.State == ui.StreamConnected })
	return b, dir
}

// TestE2EBackend drives two real backends through the interface's calls:
// signing up, a contact request, a direct chat, and signing out.
func TestE2EBackend(t *testing.T) {
	bin := testnet.Rerouted(t)
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	kat, katDir := signUp(t, o, "a.test", "kat")
	ravi, _ := signUp(t, o, "b.test", "ravi")

	code, err := ravi.MakeInvite(ctx)
	if err != nil || !strings.HasPrefix(code, "@ravi.b.test::") {
		t.Fatalf("%q %v", code, err)
	}
	if err := kat.AddContact(ctx, "@ravi.b.test"); err == nil {
		t.Error("a request without a code went out")
	}
	if err := kat.AddContact(ctx, code); err != nil {
		t.Fatal(err)
	}
	wait(t, kat, "the sent request", func(e ui.Event) bool {
		c, ok := e.(ui.ChatChanged)
		return ok && c.Chat.Kind == ui.SentRequest
	})
	req := wait(t, ravi, "Kat's request", func(e ui.Event) bool {
		c, ok := e.(ui.ChatChanged)
		return ok && c.Chat.Kind == ui.ContactRequest && c.Chat.Title == "@kat.a.test"
	}).(ui.ChatChanged).Chat
	chats, _ := ravi.Chats(ctx)
	if len(chats) != 1 || chats[0].ID != req.ID {
		t.Fatalf("Ravi's chat list: %+v", chats)
	}
	if err := ravi.AcceptRequest(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	// The request leaves Kat's list, and the direct chat takes its place.
	var direct ui.Chat
	removed := false
	wait(t, kat, "the direct chat", func(e ui.Event) bool {
		switch e := e.(type) {
		case ui.ChatChanged:
			if e.Chat.Kind == ui.DirectChat {
				direct = e.Chat
			}
		case ui.ChatRemoved:
			removed = removed || strings.HasPrefix(e.Chat, requestPrefix)
		}
		return direct.ID != "" && removed
	})
	if direct.Title != "@ravi.b.test" {
		t.Errorf("the chat's title %q", direct.Title)
	}

	m, err := kat.Send(ctx, direct.ID, ui.Draft{Text: "hello from the backend"})
	if err != nil || m.Status != ui.StatusSent {
		t.Fatalf("%+v %v", m, err)
	}
	got := wait(t, ravi, "Kat's message", func(e ui.Event) bool {
		a, ok := e.(ui.MessageAdded)
		return ok && a.Message.Text == "hello from the backend"
	}).(ui.MessageAdded).Message
	if got.Mine || got.Sender.Handle != "kat.a.test" {
		t.Errorf("the message: %+v", got)
	}
	if err := ravi.React(ctx, got.Chat, got.ID, "👍"); err != nil {
		t.Fatal(err)
	}
	if err := kat.Edit(ctx, direct.ID, m.ID, "hello, edited"); err != nil {
		t.Fatal(err)
	}
	wait(t, kat, "Ravi's reaction", func(e ui.Event) bool {
		c, ok := e.(ui.MessageChanged)
		return ok && len(c.Message.Reactions) == 1 && c.Message.Reactions[0].Emoji == "👍"
	})
	wait(t, ravi, "Kat's edit", func(e ui.Event) bool {
		c, ok := e.(ui.MessageChanged)
		return ok && c.Message.Text == "hello, edited" && c.Message.Edited
	})
	if err := kat.SetTyping(ctx, direct.ID, true); err != nil {
		t.Fatal(err)
	}
	wait(t, ravi, "Kat typing", func(e ui.Event) bool { ty, ok := e.(ui.Typing); return ok && ty.On && ty.Who.Handle == "kat.a.test" })

	// The history loads again from a reopened profile.
	kat.Close()
	kat2 := New(Options{ProfileDir: katDir, Client: client.Options{Origins: o}})
	defer kat2.Close()
	id, err := kat2.Open(ctx)
	if err != nil || id.Handle != "kat.a.test" {
		t.Fatalf("%+v %v", id, err)
	}
	msgs, err := kat2.Messages(ctx, direct.ID, "", 50)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "hello, edited" || len(msgs[0].Reactions) != 1 {
		t.Fatalf("%+v %v", msgs, err)
	}

	// Signing out deletes the profile from this device.
	if err := kat2.SignOut(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := kat2.Open(ctx); !errors.Is(err, ui.ErrNoIdentity) {
		t.Errorf("after signing out: %v", err)
	}
}
