package client

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// openVector reads a bundle's plaintext as OpenLinkBundle does.
func openVector(t *testing.T, plain string) *LinkBundle {
	t.Helper()
	var b LinkBundle
	if err := wire.Unmarshal([]byte(plain), &b); err != nil {
		t.Fatal(err)
	}
	h, err := bundleHash([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	b.unsigned = h
	return &b
}

// TestSignedBundleVector checks v2/link.json's signed bundle (§7.2 step 4):
// it checks out for the new device it names, with device 1 current, and
// fails when anything it binds differs.
func TestSignedBundleVector(t *testing.T) {
	var v struct {
		Device       vectors.Key `json:"device"`
		SignedBundle struct {
			HPKE      string `json:"hpke_public_hex"`
			Plaintext string `json:"plaintext"`
			Unsigned  string `json:"unsigned_sha256"`
		} `json:"signed_bundle"`
	}
	vectors.Load(t, "v2/link.json", &v)
	var ch struct {
		Entries []struct {
			Signed vectors.Signed `json:"signed"`
		} `json:"entries"`
	}
	vectors.Load(t, "v2/chain.json", &ch)
	s, err := wire.ParseSigned([]byte(ch.Entries[0].Signed.Signed))
	if err != nil {
		t.Fatal(err)
	}
	chain, err := identity.Validate([]wire.Signed{s}, identity.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dev := vectors.Hex(t, v.Device.PublicHex)
	hpkePub := vectors.Hex(t, v.SignedBundle.HPKE)

	b := openVector(t, v.SignedBundle.Plaintext)
	if hex.EncodeToString(b.unsigned) != v.SignedBundle.Unsigned {
		t.Fatalf("the unsigned hash is %x", b.unsigned)
	}
	if err := CheckLinkBundle(b, chain, dev, hpkePub); err != nil || !b.Signed {
		t.Fatalf("the signed bundle: %v", err)
	}
	if len(b.Members) != 1 {
		t.Errorf("members: %v", b.Members)
	}

	// Another HPKE key, another new device, or a changed bundle fail.
	other := append([]byte(nil), hpkePub...)
	other[0] ^= 1
	if err := CheckLinkBundle(openVector(t, v.SignedBundle.Plaintext), chain, dev, other); !errors.Is(err, ErrBundleSignature) {
		t.Errorf("another HPKE key: %v", err)
	}
	otherDev := append([]byte(nil), dev...)
	otherDev[0] ^= 1
	if err := CheckLinkBundle(openVector(t, v.SignedBundle.Plaintext), chain, otherDev, hpkePub); !errors.Is(err, ErrBundleSignature) {
		t.Errorf("another device: %v", err)
	}
	changed := strings.Replace(v.SignedBundle.Plaintext, `"contacts":[]`, `"contacts":["tr:aaaaaaaaaaaaaaaaaaaaaaaa"]`, 1)
	if err := CheckLinkBundle(openVector(t, changed), chain, dev, hpkePub); !errors.Is(err, ErrBundleSignature) {
		t.Errorf("a changed bundle: %v", err)
	}

	// An unsigned bundle is taken, as unsigned.
	unsigned := openVector(t, `{"account-key":"YWFhYWFhYWFhYWFhYWFhYQ","contacts":[],"groups":[],"id":"tr:cnshscx3enbgf6r7zjmj5w2z"}`)
	if err := CheckLinkBundle(unsigned, chain, dev, hpkePub); err != nil || unsigned.Signed {
		t.Errorf("an unsigned bundle: %v", err)
	}
}
