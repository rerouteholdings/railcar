package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The file tests (§14). They're skipped unless RAILCAR_REROUTED names a
// rerouted binary, and the interop one unless RAILCAR_REROUTE_CLI names a
// reroute-cli binary.

// fileMessage finds the newest message with a file in a chat.
func (r *railcar) fileMessage(chat string) Message {
	r.t.Helper()
	msgs, err := r.e.Messages(chat)
	if err != nil {
		r.t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if len(msgs[i].Files) > 0 {
			return msgs[i]
		}
	}
	r.t.Fatalf("no file in %v", msgs)
	return Message{}
}

// TestE2EInteropFiles: files between Railcar and Reroute, across two
// homeservers. Railcar sends an image and Reroute downloads and decrypts
// it; Reroute sends a file and Railcar saves it. Then Railcar sends a file
// to another Railcar identity.
func TestE2EInteropFiles(t *testing.T) {
	bin := testnet.Rerouted(t)
	ravi := newRerouteCLI(t, "ravi")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	ravi.run("v2", "register", "ravi", "--server", o["b.test"], "--invite", testnet.SignupCode)
	kat := newRailcar(t, o, "a.test", "kat")
	mina := newRailcar(t, o, "b.test", "mina")
	if _, err := kat.e.Add(ctx, lastLine(ravi.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", "kat.a.test")
	settle()
	kat.sync()
	ch := kat.chatWith("ravi.b.test")

	// Railcar to Reroute: an image.
	pic := tinyPNG(t)
	m, err := kat.e.SendFile(ctx, ch.ID, "map.png", pic)
	if err != nil {
		t.Fatal(err)
	}
	if f := m.Files[0]; f.Width != 4 || f.Height != 3 || f.Ref.MIME != "image/png" {
		t.Fatalf("the sent image: %+v", f)
	}
	settle()
	ravi.run("sync")
	id := idOfFile(t, ravi.run("history", "kat.a.test"), "map.png")
	out := filepath.Join(t.TempDir(), "got.png")
	ravi.run("get-file", "kat.a.test", id, out)
	if got, err := os.ReadFile(out); err != nil || !bytes.Equal(got, pic) {
		t.Fatalf("Reroute's copy of the image: %v", err)
	}

	// Reroute to Railcar: a file of 200 KiB, with a caption.
	data := make([]byte, 200<<10)
	rand.Read(data)
	src := filepath.Join(t.TempDir(), "timetable.bin")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ravi.run("send-file", "--caption", "the new timetable", "kat.a.test", src)
	settle()
	kat.sync()
	fm := kat.fileMessage(ch.ID)
	if fm.Body != "the new timetable" || fm.Files[0].Ref.Name != "timetable.bin" || fm.Files[0].Ref.Size != uint64(len(data)) {
		t.Fatalf("the file from Reroute: %+v", fm.Files[0].Ref)
	}
	dir := t.TempDir()
	path, err := kat.e.SaveFile(ctx, ch.ID, fm.ID, 0, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Railcar's copy of Reroute's file: %v", err)
	}
	// Saving again doesn't overwrite.
	if p2, err := kat.e.SaveFile(ctx, ch.ID, fm.ID, 0, dir); err != nil || p2 == path {
		t.Fatalf("saved again to %s: %v", p2, err)
	}

	// Railcar to Railcar: a voice message, as an audio file.
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mina.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	if _, err := kat.e.Accept(ctx, mina.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	mch := mina.waitChat("kat.a.test")
	voice := append([]byte("OggS"), make([]byte, 3000)...)
	if _, err := mina.e.SendFile(ctx, mch.ID, "voice-message.ogg", voice); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	km := kat.fileMessage(kat.chatWith("mina.b.test").ID)
	if !strings.HasPrefix(km.Files[0].Ref.MIME, "audio/") {
		t.Fatalf("the voice message's type: %q", km.Files[0].Ref.MIME)
	}
	if got, err := kat.e.c.DownloadFile(ctx, km.Files[0].Ref); err != nil || !bytes.Equal(got, voice) {
		t.Fatalf("the voice message: %v", err)
	}

	// Too large for any homeserver.
	if _, err := kat.e.SendFile(ctx, ch.ID, "big.bin", make([]byte, client.MaxFile+1)); err == nil {
		t.Fatal("a file over 64 MiB was sent")
	}
}

// idOfFile finds the ID of the message with a file in reroute-cli's
// history.
func idOfFile(t *testing.T, history, name string) string {
	t.Helper()
	for _, l := range strings.Split(history, "\n") {
		if strings.Contains(l, name) {
			return strings.Fields(l)[0]
		}
	}
	t.Fatalf("no %q in reroute-cli's history", name)
	return ""
}
