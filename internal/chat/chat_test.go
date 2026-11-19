package chat

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestGroupExt(t *testing.T) {
	g := &GroupExt{Kind: KindDirect, Hub: "a.example.com", Since: map[string]uint64{"tr:aaaaaaaaaaaaaaaaaaaaaaaa": 0}}
	b, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"admins":[],"hub":"a.example.com","kind":"direct","since":{"tr:aaaaaaaaaaaaaaaaaaaaaaaa":0}}`
	if string(b) != want {
		t.Fatalf("encoded %s", b)
	}
	back, err := ParseGroupExt(b)
	if err != nil || back.Hub != g.Hub || back.Since["tr:aaaaaaaaaaaaaaaaaaaaaaaa"] != 0 {
		t.Fatalf("%+v %v", back, err)
	}
	// The extension is closed, and a direct chat has no admins (§11.2).
	for _, bad := range []string{
		`{"admins":[],"hub":"a.example.com","kind":"direct","since":{},"extra":1}`,
		`{"admins":["tr:aaaaaaaaaaaaaaaaaaaaaaaa"],"hub":"a.example.com","kind":"direct","since":{}}`,
		`{"admins":[],"hub":"a.example.com","kind":"channel","since":{}}`,
		`{"admins":[],"hub":"a.example.com","kind":"direct"}`,
	} {
		if _, err := ParseGroupExt([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestBasisFor(t *testing.T) {
	dev, err := identity.GenerateDevice("laptop")
	if err != nil {
		t.Fatal(err)
	}
	g, err := identity.NewGenesis(dev, "a.example.com", "kat.a.example.com", [32]byte{1}, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	c, err := identity.Validate([]wire.Signed{g}, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		d, _ := identity.GenerateDevice("extra")
		e := c.Next(1_700_000_001+int64(i), identity.AddDevice(d.Key.Public(), d.Name))
		s, _ := dev.SignEntry(e)
		if err := c.Append(s, identity.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	b := basisFor(c)
	if b.Seq != 5 || len(b.Tail) != identity.MaxTail || b.Rec != nil {
		t.Fatalf("basis seq %d, tail %d", b.Seq, len(b.Tail))
	}
	v, _, err := identity.JudgeBasis(c.ID(), identity.Basis{Bound: boundOf(b.Ref()), Tail: b.Tail}, c.Entries, nil, identity.NoCheck, nil)
	if v != identity.Valid {
		t.Fatalf("%v %v", v, err)
	}
}

func TestAppMessage(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	group := make([]byte, 32)
	body := "hello"
	plain, err := signAppMessage(key, "tr:aaaaaaaaaaaaaaaaaaaaaaaa", group, 1_700_000_000, Text{T: CText, ID: newMsgID(), TS: 1_700_000_000, Body: &body})
	if err != nil {
		t.Fatal(err)
	}
	am, typ, err := openAppMessage(plain, key.Public().(ed25519.PublicKey))
	if err != nil || typ != CText || am.ID != "tr:aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("%+v %q %v", am, typ, err)
	}
	c, err := parseContent(typ, am.C)
	if err != nil || *c.(Text).Body != "hello" {
		t.Fatalf("%+v %v", c, err)
	}
	// Another key's signature is refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := openAppMessage(plain, other.Public().(ed25519.PublicKey)); err == nil {
		t.Error("accepted a message signed by another key")
	}
	// A composite signature isn't a message key's (§3.2.2).
	ck, _ := sig.GenerateCompositeKey()
	s, _ := sig.SignRevealing(ck, TypeAppMessage, AppMessage{ID: "tr:aaaaaaaaaaaaaaaaaaaaaaaa", Group: group, C: []byte(`{"t":"text"}`)})
	if _, _, err := openAppMessage(s.Canonical(), ck.EdPublic()); err == nil {
		t.Error("accepted a composite signature")
	}
}

func TestParseContent(t *testing.T) {
	id := newMsgID()
	for _, tc := range []struct {
		t, json string
		ok      bool
	}{
		{CText, `{"t":"text","id":"` + id + `","ts":1,"body":""}`, true},
		{CText, `{"t":"text","id":"` + id + `","ts":1}`, false}, // a text without body is malformed (§12)
		{CEdit, `{"t":"edit","id":"` + id + `","ts":1,"target":"` + id + `","body":"x"}`, true},
		{CEdit, `{"t":"edit","id":"` + id + `","ts":1,"body":"x"}`, false},
		{CReact, `{"t":"react","id":"` + id + `","ts":1,"target":"` + id + `","emoji":"x"}`, true},
		{CReact, `{"t":"react","id":"` + id + `","ts":1,"target":"` + id + `","emoji":""}`, false},
		{CTyping, `{"t":"typing","ts":1,"on":true}`, true},
		{CTyping, `{"t":"typing","ts":1}`, false},
		{CReceipt, `{"t":"receipt","ts":1,"upto":"` + id + `"}`, true},
	} {
		_, err := parseContent(tc.t, []byte(tc.json))
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.json, err)
		}
	}
}

func TestReact(t *testing.T) {
	m := &Message{}
	react(m, "a", "👍", false)
	react(m, "b", "👍", false)
	react(m, "a", "👍", false)
	if got := strings.Join(m.Reactions["👍"], ","); got != "b,a" {
		t.Errorf("reactions %q", got)
	}
	react(m, "a", "👍", true)
	react(m, "b", "👍", true)
	if len(m.Reactions) != 0 {
		t.Errorf("reactions %v", m.Reactions)
	}
}
