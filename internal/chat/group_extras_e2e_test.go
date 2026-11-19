package chat

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
)

// testPNG makes a small PNG of one color.
func testPNG(t *testing.T, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		for y := range 8 {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestE2EInteropGroupExtras: group pictures both ways (§12), a hub moved
// to a Railcar admin's homeserver, and a group re-created by Reroute that
// Railcar switches to, and one Railcar re-creates that Reroute and
// another Railcar member follow (§11.11).
func TestE2EInteropGroupExtras(t *testing.T) {
	bin := testnet.Rerouted(t)
	lee := newRerouteCLI(t, "lee")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	lee.run("v2", "register", "lee", "--server", o["a.test"], "--invite", testnet.SignupCode)
	kat := newRailcar(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	const katH = "kat.a.test"
	befriend(t, kat, lee, katH)
	code, err := ravi.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	if _, err := ravi.e.Accept(ctx, kat.e.c.ID()); err != nil {
		t.Fatal(err)
	}
	kat.waitChat("ravi.b.test")
	syncAll := func() {
		settle()
		lee.run("sync")
		kat.sync()
		ravi.sync()
	}

	// Lee creates a group with Kat; Kat adds Ravi, her contact.
	lee.run("group-create", "Yard", katH)
	syncAll()
	yard := kat.group("Yard")
	lee.run("group-admin", "Yard", katH)
	syncAll()
	if ok, err := kat.e.AddMember(ctx, yard.ID, ravi.e.c.ID()); err != nil || !ok {
		t.Fatalf("adding Ravi: %v %v", ok, err)
	}
	syncAll()
	ravi.group("Yard")

	// Pictures: Reroute's reaches both Railcars; Kat's reaches Ravi.
	dir := t.TempDir()
	red := filepath.Join(dir, "red.png")
	os.WriteFile(red, testPNG(t, color.RGBA{200, 0, 0, 255}), 0o600)
	lee.run("group-picture", "Yard", red)
	syncAll()
	for _, r := range []*railcar{kat, ravi} {
		p, ok := r.e.GroupPicture(yard.ID)
		if !ok || !bytes.Equal(p.Data, testPNG(t, color.RGBA{200, 0, 0, 255})) || p.MIME != "image/png" {
			t.Fatalf("Reroute's picture didn't arrive: %v", ok)
		}
	}
	blue := testPNG(t, color.RGBA{0, 0, 200, 255})
	if err := kat.e.SetGroupPicture(ctx, yard.ID, blue); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if p, ok := ravi.e.GroupPicture(yard.ID); !ok || !bytes.Equal(p.Data, blue) {
		t.Fatal("Kat's picture didn't reach Ravi")
	}
	if err := kat.e.SetGroupPicture(ctx, yard.ID, []byte("not a picture")); err == nil {
		t.Fatal("a file that isn't a picture was taken")
	}

	// Kat, an admin, moves the hub from a.test to her homeserver: it's
	// a.test already, so Ravi's turn. Ravi isn't an admin; Kat makes him
	// one, and he moves it to b.test.
	if err := kat.e.MoveHub(ctx, yard.ID); err == nil {
		t.Fatal("moving the hub to the server it's on went through")
	}
	if err := kat.e.SetAdmin(ctx, yard.ID, ravi.e.c.ID(), true); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if err := ravi.e.MoveHub(ctx, yard.ID); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if c, _ := kat.e.Chat(yard.ID); c.Hub != "b.test" {
		t.Fatalf("Kat's view of the hub: %s", c.Hub)
	}
	lee.run("send", "Yard", "after the hub moved")
	if _, err := kat.e.SendText(ctx, yard.ID, "Kat, after the hub moved", ""); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if !ravi.has(yard.ID, "after the hub moved") || !ravi.has(yard.ID, "Kat, after the hub moved") {
		t.Fatal("messages after the hub moved didn't arrive")
	}
	// A commit through the new hub: Lee leaves, and someone commits it.
	lee.run("group-leave", "Yard")
	syncAll()
	syncAll()
	if c, _ := ravi.e.Chat(yard.ID); len(c.Members) != 2 {
		t.Fatalf("after Lee left: %v", c.Members)
	}

	// Kat re-creates the group: her history moves, and Ravi switches.
	old, _ := kat.e.Chat(yard.ID)
	nc, err := kat.e.RecreateGroup(ctx, yard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kat.e.Chat(old.ID); ok || !kat.has(nc.ID, "after the hub moved") {
		t.Fatal("Kat's history didn't move to the re-created group")
	}
	syncAll()
	var offer Chat
	for _, c := range ravi.e.Chats() {
		if c.Replaces == yard.ID {
			offer = c
		}
	}
	if offer.ID == "" {
		t.Fatal("Ravi wasn't offered the re-created group")
	}
	if err := ravi.e.SwitchGroup(ctx, offer.ID); err != nil {
		t.Fatal(err)
	}
	if !ravi.has(offer.ID, "after the hub moved") {
		t.Fatal("Ravi's history didn't move")
	}
	if p, ok := ravi.e.GroupPicture(offer.ID); !ok || !bytes.Equal(p.Data, blue) {
		t.Fatal("the re-created group lost its picture")
	}
	if _, err := ravi.e.SendText(ctx, offer.ID, "in the new group", ""); err != nil {
		t.Fatal(err)
	}
	syncAll()
	if !kat.has(nc.ID, "in the new group") {
		t.Fatal("Kat didn't hear Ravi in the new group")
	}

	// Reroute re-creates a group with Kat; she switches.
	lee.run("group-create", "Platform", katH)
	syncAll()
	plat := kat.group("Platform")
	lee.run("send", "Platform", "before the re-creation")
	syncAll()
	lee.run("group-recreate", "Platform")
	syncAll()
	var pOffer Chat
	for _, c := range kat.e.Chats() {
		if c.Replaces == plat.ID {
			pOffer = c
		}
	}
	if pOffer.ID == "" {
		t.Fatal("Kat wasn't offered Reroute's re-created group")
	}
	if err := kat.e.SwitchGroup(ctx, pOffer.ID); err != nil {
		t.Fatal(err)
	}
	if !kat.has(pOffer.ID, "before the re-creation") {
		t.Fatal("Kat's history of Platform didn't move")
	}
	lee.run("send", "Platform", "after the re-creation")
	syncAll()
	if !kat.has(pOffer.ID, "after the re-creation") {
		out := lee.run("groups")
		t.Fatalf("Kat didn't hear Lee in the re-created group; Lee's groups: %s", strings.TrimSpace(out))
	}

	// A mention from Reroute (§12).
	lee.run("send", "Platform", "@kat.a.test look at the board")
	syncAll()
	msgs, _ := kat.e.Messages(pOffer.ID)
	if i := slices.IndexFunc(msgs, func(m Message) bool { return m.Body == "@kat.a.test look at the board" }); i < 0 || !msgs[i].Mentioned {
		t.Fatal("Reroute's mention of Kat didn't count")
	}
}
