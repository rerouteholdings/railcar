package chat

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TestRemovedAt finds when chain.json removed device 1, which the rule on
// community entries signed by removed devices compares with an entry's
// ts (§16.2.2).
func TestRemovedAt(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "tieroute", "testdata", "vectors", "v2", "chain.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Entries []struct {
			Signed struct {
				Signed string `json:"signed"`
			} `json:"signed"`
		} `json:"entries"`
		Keys struct {
			Device1 struct {
				Public string `json:"ed25519_public"`
			} `json:"device_1"`
		} `json:"keys"`
		Final struct {
			Devices []string `json:"devices"`
		} `json:"final_state"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	var entries []wire.Signed
	for _, e := range v.Entries {
		s, err := wire.ParseSigned([]byte(e.Signed.Signed))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, s)
	}
	chain, err := identity.Validate(entries, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := hex.DecodeString(v.Keys.Device1.Public)
	at, removed := removedAt(chain, d1)
	if !removed || at == 0 {
		t.Fatalf("device 1: removed %v at %d", removed, at)
	}
	cur, _ := hex.DecodeString(v.Final.Devices[0])
	if _, removed := removedAt(chain, cur); removed {
		t.Error("a current device shows as removed")
	}
}
