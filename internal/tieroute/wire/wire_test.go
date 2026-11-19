package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
)

func TestDecodeB64Strict(t *testing.T) {
	good := map[string]string{"": "", "AA": "00", "AAE": "0001", "_-8": "ffef"}
	for in, want := range good {
		got, err := DecodeB64(in)
		if err != nil || hex.EncodeToString(got) != want {
			t.Errorf("DecodeB64(%q) = %x, %v; want %s", in, got, err, want)
		}
	}
	// Padding, the standard alphabet, line breaks, and nonzero unused bits
	// are all rejected (§4.1).
	for _, in := range []string{"AA==", "AA=", "+/8", "AA\nAA", "AB", "AAF", "A"} {
		if _, err := DecodeB64(in); err == nil {
			t.Errorf("DecodeB64(%q) accepted", in)
		}
	}
}

func TestCheckIJSON(t *testing.T) {
	ok := []string{`{}`, `{"a":1,"b":[1,-2,"x"]}`, ` {"a" : 9007199254740991} `, `"x"`}
	for _, s := range ok {
		if err := CheckIJSON([]byte(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := []string{`{"a":1,"a":2}`, `{"a":1.5}`, `{"a":1e3}`, `{"a":9007199254740992}`, "{\"a\":\"\xff\"}", `{} {}`, `{"a":`}
	for _, s := range bad {
		if err := CheckIJSON([]byte(s)); err == nil {
			t.Errorf("%s: accepted", s)
		}
	}
}

func TestJCS(t *testing.T) {
	got, err := JCS([]byte(`{"b": "éé", "a": [3, {"z":1, "y":"\u000f"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":[3,{"y":"\u000f","z":1}],"b":"éé"}`
	if string(got) != want {
		t.Errorf("JCS = %s, want %s", got, want)
	}
}

// TestSignedVectors checks the envelope encoding, signature input, and hash
// of every signed object in signed.json and msgkey.json (§4.3).
func TestSignedVectors(t *testing.T) {
	var sv struct {
		Signed           vectors.Signed `json:"signed"`
		MessageKeySigned vectors.Signed `json:"message_key_signed"`
		Reject           []struct {
			Note   string `json:"note"`
			Signed string `json:"signed"`
		} `json:"reject"`
	}
	vectors.Load(t, "v2/signed.json", &sv)
	var mk struct {
		MsgKey1   vectors.Signed `json:"msg_key_1"`
		MsgKey2   vectors.Signed `json:"msg_key_2"`
		KPBinding vectors.Signed `json:"kp_binding"`
	}
	vectors.Load(t, "v2/msgkey.json", &mk)
	for _, v := range []vectors.Signed{sv.Signed, sv.MessageKeySigned, mk.MsgKey1, mk.MsgKey2, mk.KPBinding} {
		s, err := ParseSigned([]byte(v.Signed))
		if err != nil {
			t.Fatalf("%s: %v", v.Type, err)
		}
		if s.T != v.Type || s.B != v.Body {
			t.Errorf("%s: t or b differs", v.Type)
		}
		if string(s.SigInput()) != v.SigInput {
			t.Errorf("%s: signature input differs", v.Type)
		}
		if !bytes.Equal(s.S, vectors.Hex(t, v.SignatureHex)) {
			t.Errorf("%s: signature differs", v.Type)
		}
		if string(s.Canonical()) != v.Signed {
			t.Errorf("%s: JCS(signed) differs", v.Type)
		}
		h := s.Hash()
		if hex.EncodeToString(h[:]) != v.SignedSHA256 {
			t.Errorf("%s: hash differs", v.Type)
		}
		if h2 := sha256.Sum256([]byte(v.Signed)); h2 != h {
			t.Errorf("%s: hash isn't over the JCS text", v.Type)
		}
	}
	// The first reject case is an envelope with an extra member.
	if _, err := ParseSigned([]byte(sv.Reject[0].Signed)); err == nil {
		t.Errorf("accepted: %s", sv.Reject[0].Note)
	}
}

func TestSignedEnvelopeShape(t *testing.T) {
	bad := []string{
		`{"t":"x","b":"{}","s":"AA"}`,
		`{"t":"x","b":"{}","k":"AA","kid":"k1","s":"AA"}`,
		`{"t":"x","b":"{}","k":"AA","s":"AA","x":1}`,
		`{"t":"x","b":"{}","k":"AA==","s":"AA"}`,
		`{"t":"x","b":"{}","k":"AA","s":"AA","s":"AA"}`,
	}
	for _, s := range bad {
		if _, err := ParseSigned([]byte(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
