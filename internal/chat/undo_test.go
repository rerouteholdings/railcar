package chat

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TestCommitUndoVector reads commit-undo.json's signed undo, and checks
// its body names what the vector says (§16.5).
func TestCommitUndoVector(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "tieroute", "testdata", "vectors", "v2", "commit-undo.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Commit struct {
			SHA256 string `json:"sha256_hex"`
		} `json:"commit"`
		Community string `json:"community_id_hex"`
		Group     string `json:"group_id_hex"`
		Undo      struct {
			Body   string `json:"body"`
			Signed string `json:"signed"`
		} `json:"undo"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	s, err := wire.ParseSigned([]byte(v.Undo.Signed))
	if err != nil {
		t.Fatal(err)
	}
	var u CommitUndo
	if err := s.Body(&u); err != nil || s.T != TypeCommitUndo {
		t.Fatalf("the undo: %v, t %q", err, s.T)
	}
	if hex.EncodeToString(u.Community) != v.Community || hex.EncodeToString(u.Group) != v.Group || hex.EncodeToString(u.Hash) != v.Commit.SHA256 || u.Epoch != 42 {
		t.Errorf("the undo's body: %+v", u)
	}
	again, err := wire.BodyText(u)
	if err != nil || again != v.Undo.Body {
		t.Errorf("the body again:\n%s\nwant\n%s", again, v.Undo.Body)
	}
}
