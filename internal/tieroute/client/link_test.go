package client

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/codes"
	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestLinkCodeVectors(t *testing.T) {
	var v2 struct {
		Device vectors.Key `json:"device"`
		Full   struct {
			Home     string `json:"home"`
			HPKEHash string `json:"hpke_sha256_hex"`
			LinkCode string `json:"link_code"`
		} `json:"full"`
		IdentityID  string         `json:"identity_id"`
		ParkedProof vectors.Signed `json:"parked_proof"`
		Proof       vectors.Signed `json:"proof"`
	}
	vectors.Load(t, "v2/link.json", &v2)
	dev := vectors.Hex(t, v2.Device.Ed25519Public)
	hash := vectors.Hex(t, v2.Full.HPKEHash)
	code := LinkCode{Device: dev, HPKEHash: hash, Home: v2.Full.Home}
	if code.String() != v2.Full.LinkCode {
		t.Errorf("link code\n got %s\nwant %s", code, v2.Full.LinkCode)
	}
	got, err := ParseLinkCode(strings.ToUpper(strings.Replace(v2.Full.LinkCode, "-", " ", 1)))
	if err != nil || !bytes.Equal(got.Device, dev) || !bytes.Equal(got.HPKEHash, hash) || got.Home != v2.Full.Home {
		t.Errorf("parsed %+v, %v", got, err)
	}
	if !bytes.Equal(sha256Of(bytes.Repeat([]byte{0x6b}, HPKEPublicSize)), hash) {
		t.Error("the stand-in HPKE key's hash differs")
	}

	// Both proofs carry the composite key in k, and check out.
	pub := vectors.Hex(t, v2.Device.PublicHex)
	for _, pv := range []vectors.Signed{v2.ParkedProof, v2.Proof} {
		s, err := wire.ParseSigned([]byte(pv.Signed))
		if err != nil {
			t.Fatal(err)
		}
		p, err := ParseLinkProof(s)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(p.Pub, pub) || !bytes.Equal(p.Device(), dev) || p.Body.ID != v2.IdentityID {
			t.Errorf("proof %+v", p.Body)
		}
		b, _ := wire.BodyText(p.Body)
		if b != pv.Body {
			t.Errorf("body\n got %s\nwant %s", b, pv.Body)
		}
	}

	// The short form, and the spellings parsers accept, from version 1.
	var v1 struct {
		Device struct {
			PublicHex string `json:"public_hex"`
		} `json:"device"`
		LinkCode     string   `json:"link_code"`
		AlsoAccepted []string `json:"also_accepted"`
		Confirmation struct {
			Examples [][2]string `json:"examples"`
		} `json:"confirmation_normalization"`
	}
	vectors.Load(t, "link.json", &v1)
	short := LinkCode{Device: vectors.Hex(t, v1.Device.PublicHex)}
	if short.String() != v1.LinkCode || !short.Short() {
		t.Errorf("short code %s", short)
	}
	for _, s := range append(v1.AlsoAccepted, v1.LinkCode) {
		got, err := ParseLinkCode(s)
		if err != nil || !bytes.Equal(got.Device, short.Device) || !got.Short() {
			t.Errorf("%q: %+v, %v", s, got, err)
		}
	}
	for _, ex := range v1.Confirmation.Examples {
		if got := codes.NormalizeConfirmation(ex[0]); got != ex[1] {
			t.Errorf("%q normalized to %q, not %q", ex[0], got, ex[1])
		}
	}
	for _, bad := range []string{"", "link-", "approve-aaaa", "linkaif5", "link-" + strings.Repeat("a", 103), "link-aif5@"} {
		if _, err := ParseLinkCode(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func TestApprovalCode(t *testing.T) {
	// The spec's example (§7.2.1).
	want := "approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@kat.example.com@chat.example"
	ac := ApprovalCode{Secret: bytes.Repeat([]byte{0x5a}, 16), Handle: "kat.example.com", Home: "chat.example"}
	if ac.String() != want {
		t.Errorf("got %s", ac)
	}
	for _, s := range []string{want, strings.ToUpper(want), "approve " + strings.TrimPrefix(want, "approve-"), "Approve-LJNF UWS2 LJNF UWS2 LJNF UWS2 LI@Kat.Example.com@chat.example"} {
		got, err := ParseApprovalCode(s)
		if err != nil || !bytes.Equal(got.Secret, ac.Secret) || got.Handle != ac.Handle || got.Home != ac.Home {
			t.Errorf("%q: %+v, %v", s, got, err)
		}
	}
	for _, bad := range []string{
		"approve-ljnf-uws2-ljnf-uws2-ljnf-uws2@kat.example.com@chat.example", // 15 bytes
		"approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@@chat.example",
		"approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@kat.example.com@",
		"approve-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@chat.example",
		"link-ljnf-uws2-ljnf-uws2-ljnf-uws2-li@kat.example.com@chat.example",
	} {
		if _, err := ParseApprovalCode(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestApprovalTag(t *testing.T) {
	tag := codes.ApprovalTag(bytes.Repeat([]byte{1}, 16), []byte("k"), []byte("hpke"))
	if len(tag) != 32 || strings.Trim(tag, "0123456789ABCDEFGHJKMNPQRSTVWXYZ") != "" {
		t.Errorf("tag %q", tag)
	}
	if codes.Crockford([]byte{0xff, 0xff, 0xff, 0xff, 0xff}) != "ZZZZZZZZ" || codes.Crockford([]byte{0, 0, 0, 0, 0x21}) != "00000011" {
		t.Error("Crockford base32 differs")
	}
}

func TestLinkBundleRoundTrip(t *testing.T) {
	kem, _, _ := hpkeSuite()
	priv, err := kem.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.PublicKey().Bytes()
	if len(pub) != HPKEPublicSize {
		t.Fatalf("HPKE public key of %d bytes", len(pub))
	}
	b := LinkBundle{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", Contacts: []string{"tr:g6buzlilhz576dzwhcro3bf5"}, AccountKey: bytes.Repeat([]byte{7}, 16)}
	data, err := SealLinkBundle(pub, b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenLinkBundle(priv, b.ID, data)
	if err != nil || got.ID != b.ID || len(got.Contacts) != 1 || !bytes.Equal(got.AccountKey, b.AccountKey) || got.Groups == nil {
		t.Fatalf("%+v, %v", got, err)
	}
	// The identifier is the associated data: another identity's device
	// can't open it.
	if _, err := OpenLinkBundle(priv, "tr:g6buzlilhz576dzwhcro3bf5", data); err == nil {
		t.Error("opened under another identifier")
	}
	// The private key survives a save.
	raw, _ := priv.Bytes()
	priv2, err := kem.NewPrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLinkBundle(priv2, b.ID, data); err != nil {
		t.Error(err)
	}
	if _, err := OpenLinkBundle(priv, b.ID, data[:HPKEEncSize]); err == nil {
		t.Error("opened a bundle with no ciphertext")
	}
}

func TestLinkProofRefusals(t *testing.T) {
	k, err := sig.GenerateCompositeKey()
	if err != nil {
		t.Fatal(err)
	}
	ok, _ := sig.SignRevealing(k, TypeLinkProof, LinkProofBody{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", Code: "9YS5GEBK", TS: 1})
	if _, err := ParseLinkProof(ok); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]func() wire.Signed{
		"no code or key": func() wire.Signed {
			s, _ := sig.SignRevealing(k, TypeLinkProof, LinkProofBody{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", TS: 1})
			return s
		},
		"short key": func() wire.Signed {
			s, _ := sig.SignRevealing(k, TypeLinkProof, LinkProofBody{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", HPKE: make([]byte, 32), TS: 1})
			return s
		},
		"device-signed": func() wire.Signed {
			s, _ := sig.SignDevice(k, TypeLinkProof, LinkProofBody{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", Code: "9YS5GEBK", TS: 1})
			return s
		},
		"another type": func() wire.Signed {
			s, _ := sig.SignRevealing(k, "auth", LinkProofBody{ID: "tr:cnshscx3enbgf6r7zjmj5w2z", Code: "9YS5GEBK", TS: 1})
			return s
		},
	} {
		if _, err := ParseLinkProof(s()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
