package chat

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

type paddingVectors struct {
	Floor   int                         `json:"floor"`
	Member  int                         `json:"member"`
	Padme   []struct{ X, Padme uint64 } `json:"padme"`
	Targets []struct {
		Unpadded, Target, Pad int
		Note                  string
	} `json:"targets"`
	Messages []struct {
		Note           string `json:"note"`
		Unpadded       int    `json:"unpadded"`
		Target         int    `json:"target"`
		Pad            int    `json:"pad"`
		UnpaddedBody   string `json:"unpadded_body"`
		UnpaddedSigned string `json:"unpadded_signed"`
		Padded         struct {
			Body   string `json:"body"`
			Signed string `json:"signed"`
		} `json:"padded"`
	} `json:"messages"`
}

func loadVectors(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "tieroute", "testdata", "vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// TestPadme checks Padmé's rounding alone (§12, "Padding").
func TestPadme(t *testing.T) {
	var v paddingVectors
	loadVectors(t, "v2/padding.json", &v)
	if v.Floor != padFloor || v.Member != padMember {
		t.Fatalf("floor %d and member %d, not %d and %d", v.Floor, v.Member, padFloor, padMember)
	}
	for _, c := range v.Padme {
		if got := padme(c.X); got != c.Padme {
			t.Errorf("padme(%d) = %d, want %d", c.X, got, c.Padme)
		}
	}
}

// TestPadTargets checks the unpadded lengths' targets and pad lengths.
func TestPadTargets(t *testing.T) {
	var v paddingVectors
	loadVectors(t, "v2/padding.json", &v)
	for _, c := range v.Targets {
		if got := padTarget(c.Unpadded); got != c.Target || got-c.Unpadded-padMember != c.Pad {
			t.Errorf("L = %d (%s): target %d, pad %d; want %d and %d", c.Unpadded, c.Note, got, got-c.Unpadded-padMember, c.Target, c.Pad)
		}
	}
}

// TestPaddedMessages reproduces each complete app-message, unpadded and
// padded, byte for byte, with message key 1 of msgkey.json, and opens it.
func TestPaddedMessages(t *testing.T) {
	var v paddingVectors
	loadVectors(t, "v2/padding.json", &v)
	var mk struct {
		Key struct {
			Seed string `json:"seed_hex"`
		} `json:"message_key_1"`
	}
	loadVectors(t, "v2/msgkey.json", &mk)
	seed, err := hex.DecodeString(mk.Key.Seed)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(seed)
	if len(v.Messages) == 0 {
		t.Fatal("no messages")
	}
	for _, m := range v.Messages {
		var am AppMessage
		if err := wire.Unmarshal([]byte(m.UnpaddedBody), &am); err != nil {
			t.Fatalf("%s: %v", m.Note, err)
		}
		bare, err := sig.SignMessageKey(key, TypeAppMessage, am)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(bare.Canonical()); got != m.UnpaddedSigned || len(got) != m.Unpadded {
			t.Errorf("%s: the unpadded message differs:\n%s", m.Note, got)
		}
		padded, err := signPadded(key, am)
		if err != nil {
			t.Fatal(err)
		}
		got := padded.Canonical()
		if string(got) != m.Padded.Signed || len(got) != m.Target || string(padded.B) != m.Padded.Body {
			t.Errorf("%s: the padded message differs:\n%s", m.Note, got)
		}
		if am.Pad != nil {
			t.Errorf("%s: signPadded changed the caller's message", m.Note)
		}
		opened, _, err := openAppMessage(got, key.Public().(ed25519.PublicKey))
		if err != nil || opened.Pad == nil || len(*opened.Pad) != m.Pad {
			t.Errorf("%s: opening the padded message: %v", m.Note, err)
		}
	}
}

// TestAnyPad checks that a receiver ignores pad: none, odd characters,
// and a length that hits no target all open (§12).
func TestAnyPad(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	pub := key.Public().(ed25519.PublicKey)
	c, _ := wire.JCS(map[string]any{"t": "text", "id": "cXFxcXFxcXFxcXFxcXFxcQ", "ts": 1, "body": "hi"})
	for _, pad := range []*string{nil, ptr(""), ptr("xyz é \""), ptr(strings.Repeat("0", 7))} {
		s, err := sig.SignMessageKey(key, TypeAppMessage, AppMessage{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", Group: []byte("g"), TS: 1, C: c, Pad: pad})
		if err != nil {
			t.Fatal(err)
		}
		if _, typ, err := openAppMessage(s.Canonical(), pub); err != nil || typ != CText {
			t.Errorf("pad %v: %v", pad, err)
		}
	}
}
