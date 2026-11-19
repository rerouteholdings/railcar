package chat

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The profile and presence tests (§13). They're skipped unless
// RAILCAR_REROUTED names a rerouted binary, and the interop one unless
// RAILCAR_REROUTE_CLI names a reroute-cli binary.

// run starts the engine's live stream until the test ends.
func (r *railcar) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.e.Run(ctx)
	}()
	r.t.Cleanup(func() { cancel(); <-done })
}

// waitFor polls until ok, for up to 20 seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 80 {
		if ok() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func tinyPNG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{255, 74, 28, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestE2EInteropPeople: profiles and presence between Railcar and Reroute.
// Reroute publishes a profile and sets its presence, and Railcar, on the
// live stream, sees both. Railcar publishes a profile with a picture and
// sets its presence; Reroute reads the profile, and a second Railcar
// contact sees the presence.
func TestE2EInteropPeople(t *testing.T) {
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
	code, err := kat.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mina.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.run("sync")
	ravi.run("accept", "kat.a.test")
	settle()
	kat.sync()
	if _, err := kat.e.Accept(ctx, mina.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	mina.waitChat("kat.a.test")
	raviID := kat.chatWith("ravi.b.test").Peer

	// Reroute to Railcar.
	kat.run()
	ravi.run("profile-set", "--name", "Ravi Shah", "--bio", "Night shift at the depot", "--link", "https://ravi.example.org/trains")
	ravi.run("presence", "busy", "in", "a", "meeting")
	waitFor(t, "Ravi's presence", func() bool {
		p, ok := kat.e.Presence(raviID)
		return ok && p.State == client.PresenceBusy && p.Status == "in a meeting"
	})
	waitFor(t, "Ravi's profile", func() bool {
		p, ok := kat.e.ProfileOf(raviID)
		return ok && p.Name == "Ravi Shah" && p.Bio == "Night shift at the depot" && len(p.Links) == 1
	})

	// Railcar to Reroute and Railcar.
	mina.run()
	pic, err := kat.e.Client().UploadPublic(ctx, tinyPNG(t), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	name := "Kat Cole"
	if _, err := kat.e.EditProfile(ctx, ProfileEdit{Name: &name, Avatar: &pic}); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.SetPresence(ctx, client.PresenceAway, "lunch"); err != nil {
		t.Fatal(err)
	}
	if out := ravi.run("profile", "kat.a.test"); !strings.Contains(out, "Kat Cole") {
		t.Fatal("Reroute didn't show Railcar's display name")
	}
	katID := kat.e.Client().ID()
	waitFor(t, "Kat's presence at Mina", func() bool {
		p, ok := mina.e.Presence(katID)
		return ok && p.State == client.PresenceAway && p.Status == "lunch"
	})
	waitFor(t, "Kat's profile at Mina", func() bool {
		p, ok := mina.e.ProfileOf(katID)
		return ok && p.Name == "Kat Cole" && p.Avatar != nil
	})
	p, _ := mina.e.ProfileOf(katID)
	data, err := mina.e.Client().DownloadPublic(ctx, *p.Avatar)
	if err != nil || !bytes.Equal(data, tinyPNG(t)) {
		t.Fatalf("Kat's picture: %v", err)
	}

	// Auto-away, and back.
	if err := kat.e.SetPresence(ctx, client.PresenceAvailable, ""); err != nil {
		t.Fatal(err)
	}
	kat.e.AutoAway(ctx, true)
	waitFor(t, "auto-away", func() bool { p, _ := mina.e.Presence(katID); return p.State == client.PresenceAway })
	kat.e.AutoAway(ctx, false)
	waitFor(t, "back from auto-away", func() bool { p, _ := mina.e.Presence(katID); return p.State == client.PresenceAvailable })
}
