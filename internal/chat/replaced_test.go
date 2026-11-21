package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TestReplacedKeys uses msgkey.json's two msg-keys for one device, n = 1
// and n = 2: an item signed with the first is marked replaced when the
// export holds the second (§3.2.3).
func TestReplacedKeys(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "tieroute", "testdata", "vectors", "v2", "msgkey.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		One struct {
			Signed string `json:"signed"`
		} `json:"msg_key_1"`
		Two struct {
			Signed string `json:"signed"`
		} `json:"msg_key_2"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	one, err := wire.ParseSigned([]byte(v.One.Signed))
	if err != nil {
		t.Fatal(err)
	}
	two, err := wire.ParseSigned([]byte(v.Two.Signed))
	if err != nil {
		t.Fatal(err)
	}
	items := []imported{{MK: one}, {MK: two}}
	replacedKeys(items, map[string]wire.Signed{"1": one})
	if items[0].Replaced || items[1].Replaced {
		t.Fatal("marked with only the older key in the export")
	}
	replacedKeys(items, map[string]wire.Signed{"1": one, "2": two})
	if !items[0].Replaced || items[1].Replaced {
		t.Fatalf("the older key's item: %v, the newer's: %v", items[0].Replaced, items[1].Replaced)
	}
}
