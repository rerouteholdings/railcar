package codes

import (
	"encoding/hex"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestContactCodeVector(t *testing.T) {
	var v struct {
		Contact struct {
			Code           string   `json:"code"`
			CommitmentsHex []string `json:"commitments_hex"`
			DigestHex      string   `json:"digest_hex"`
			EpochAuthHex   string   `json:"epoch_authenticator_hex"`
			HeadsHex       []string `json:"heads_hex"`
			IDs            []string `json:"identity_ids"`
			RevealsHex     []string `json:"reveals_hex"`
		} `json:"contact"`
	}
	vectors.Load(t, "v2/codes.json", &v)
	c := v.Contact
	for i, r := range c.RevealsHex {
		got := Commit(vectors.Hex(t, r))
		if hex.EncodeToString(got[:]) != c.CommitmentsHex[i] {
			t.Errorf("commitment %d differs", i)
		}
	}
	a := Side{ID: c.IDs[0], Head: vectors.Hex(t, c.HeadsHex[0]), Reveal: vectors.Hex(t, c.RevealsHex[0])}
	b := Side{ID: c.IDs[1], Head: vectors.Hex(t, c.HeadsHex[1]), Reveal: vectors.Hex(t, c.RevealsHex[1])}
	ea := vectors.Hex(t, c.EpochAuthHex)
	// Both sides get the same code, whichever is first.
	for _, sides := range [][2]Side{{a, b}, {b, a}} {
		code, digest := ContactCode(sides[0], sides[1], ea)
		if code != c.Code || hex.EncodeToString(digest[:]) != c.DigestHex {
			t.Errorf("code %s, digest %x", code, digest)
		}
	}
}

func TestJoinCodeVector(t *testing.T) {
	var v struct {
		Code           string         `json:"code"`
		CodeKey        vectors.Key    `json:"code_key"`
		CommunityIDHex string         `json:"community_id_hex"`
		JoinProof      vectors.Signed `json:"join_proof"`
	}
	vectors.Load(t, "v2/code.json", &v)
	server, secret, err := ParseJoinCode(v.Code)
	if err != nil || server != "community.example.com" {
		t.Fatalf("%q %v", server, err)
	}
	k, err := CodeKey(secret, vectors.Hex(t, v.CommunityIDHex))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(k.Public()) != v.CodeKey.PublicHex {
		t.Fatal("code key differs")
	}
	s, err := wire.ParseSigned([]byte(v.JoinProof.Signed))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := sig.VerifyRevealed(s)
	if err != nil || hex.EncodeToString(pub) != v.CodeKey.PublicHex {
		t.Errorf("join proof: %v", err)
	}
	if _, _, err := ParseJoinCode("community.example.com::short"); err == nil {
		t.Error("accepted a short code")
	}
}
