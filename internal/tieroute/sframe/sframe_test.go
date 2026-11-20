package sframe

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type vectorFile struct {
	Headers []struct {
		KID    uint64 `json:"kid"`
		CTR    uint64 `json:"ctr"`
		Header string `json:"header_hex"`
	} `json:"headers"`
	RFC9605 struct {
		BaseKey    string `json:"base_key_hex"`
		KID        uint64 `json:"kid"`
		CTR        uint64 `json:"ctr"`
		Metadata   string `json:"metadata_hex"`
		Plaintext  string `json:"plaintext_hex"`
		Ciphertext string `json:"ciphertext_hex"`
		Key        string `json:"sframe_key_hex"`
		Salt       string `json:"sframe_salt_hex"`
	} `json:"rfc9605"`
	Tieroute struct {
		BaseKey string `json:"base_key_hex"`
		Cases   []struct {
			Ciphertext string `json:"ciphertext_hex"`
			CTR        uint64 `json:"ctr"`
			Epoch      uint64 `json:"epoch"`
			Header     string `json:"header_hex"`
			KID        uint64 `json:"kid"`
			Leaf       uint32 `json:"leaf_index"`
			Plaintext  string `json:"plaintext"`
			Key        string `json:"sframe_key_hex"`
			Salt       string `json:"sframe_salt_hex"`
		} `json:"cases"`
	} `json:"tieroute"`
	Voice struct {
		BaseKey string `json:"base_key_hex"`
		Cases   []struct {
			Visit      uint32 `json:"visit"`
			Gen        uint8  `json:"generation"`
			Stream     uint8  `json:"stream"`
			KID        uint64 `json:"kid"`
			CTR        uint64 `json:"ctr"`
			Header     string `json:"header_hex"`
			Plaintext  string `json:"plaintext"`
			Ciphertext string `json:"ciphertext_hex"`
			Key        string `json:"sframe_key_hex"`
			Salt       string `json:"sframe_salt_hex"`
		} `json:"cases"`
	} `json:"voice"`
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func load(t *testing.T) vectorFile {
	b, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "sframe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVectorHeaders(t *testing.T) {
	for _, h := range load(t).Headers {
		got := Header(h.KID, h.CTR)
		if hex.EncodeToString(got) != h.Header {
			t.Errorf("kid %d ctr %d: %x, want %s", h.KID, h.CTR, got, h.Header)
		}
		kid, ctr, n, err := ParseHeader(append(got, 0xAA))
		if err != nil || kid != h.KID || ctr != h.CTR || n != len(got) {
			t.Errorf("parsing %x: %d %d %d %v", got, kid, ctr, n, err)
		}
	}
}

func TestVectorRFC9605(t *testing.T) {
	v := load(t).RFC9605
	key, salt, err := DeriveBytes(unhex(t, v.BaseKey), v.KID)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(key) != v.Key || hex.EncodeToString(salt) != v.Salt {
		t.Fatalf("key %x salt %x", key, salt)
	}
	k, _ := Derive(unhex(t, v.BaseKey), v.KID)
	ct := k.Seal(v.KID, v.CTR, unhex(t, v.Metadata), unhex(t, v.Plaintext))
	if hex.EncodeToString(ct) != v.Ciphertext {
		t.Fatalf("ciphertext %x", ct)
	}
	kid, ctr, n, err := ParseHeader(ct)
	if err != nil || kid != v.KID || ctr != v.CTR {
		t.Fatal(kid, ctr, err)
	}
	pt, err := k.Open(ct, ctr, n, unhex(t, v.Metadata))
	if err != nil || !bytes.Equal(pt, unhex(t, v.Plaintext)) {
		t.Fatal(pt, err)
	}
}

func TestVectorTieroute(t *testing.T) {
	v := load(t).Tieroute
	base := unhex(t, v.BaseKey)
	for _, c := range v.Cases {
		if kid := CallKID(c.Leaf, c.Epoch); kid != c.KID {
			t.Errorf("kid %d, want %d", kid, c.KID)
		}
		key, salt, _ := DeriveBytes(base, c.KID)
		if hex.EncodeToString(key) != c.Key || hex.EncodeToString(salt) != c.Salt {
			t.Errorf("kid %d: key %x salt %x", c.KID, key, salt)
		}
		k, _ := Derive(base, c.KID)
		ct := k.Seal(c.KID, c.CTR, nil, []byte(c.Plaintext))
		if hex.EncodeToString(ct) != c.Ciphertext {
			t.Errorf("kid %d: ciphertext %x", c.KID, ct)
		}
		_, ctr, n, _ := ParseHeader(ct)
		if pt, err := k.Open(ct, ctr, n, nil); err != nil || string(pt) != c.Plaintext {
			t.Errorf("kid %d: open: %v", c.KID, err)
		}
		ct[len(ct)-1] ^= 1
		if _, err := k.Open(ct, ctr, n, nil); err == nil {
			t.Errorf("kid %d: a changed frame opened", c.KID)
		}
	}
}

func TestVectorVoice(t *testing.T) {
	v := load(t).Voice
	if len(v.Cases) == 0 {
		t.Fatal("no voice channel cases")
	}
	base := unhex(t, v.BaseKey)
	for _, c := range v.Cases {
		kid := VoiceKID(c.Visit, c.Gen, c.Stream)
		if kid != c.KID {
			t.Errorf("kid %x, want %x", kid, c.KID)
		}
		if vi, g, st := SplitVoiceKID(kid); vi != c.Visit || g != c.Gen || st != c.Stream {
			t.Errorf("kid %x splits into %x %d %d", kid, vi, g, st)
		}
		key, salt, _ := DeriveBytes(base, kid)
		if hex.EncodeToString(key) != c.Key || hex.EncodeToString(salt) != c.Salt {
			t.Errorf("kid %x: key %x salt %x", kid, key, salt)
		}
		k, _ := Derive(base, kid)
		ct := k.Seal(kid, c.CTR, nil, []byte(c.Plaintext))
		if hex.EncodeToString(ct) != c.Ciphertext || !bytes.HasPrefix(ct, unhex(t, c.Header)) {
			t.Errorf("kid %x: ciphertext %x", kid, ct)
		}
	}
}
