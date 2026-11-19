package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// A clock after every timestamp in the vectors, so the future check passes.
var later = time.Unix(1900000000, 0)

func parse(t *testing.T, texts ...string) []wire.Signed {
	t.Helper()
	out := make([]wire.Signed, len(texts))
	for i, s := range texts {
		v, err := wire.ParseSigned([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = v
	}
	return out
}

func vkey(t *testing.T, k vectors.Key) *sig.CompositeKey {
	t.Helper()
	ed := k.Ed25519SeedHex
	if ed == "" {
		ed = k.RecoverySeedHex
	}
	ck, err := sig.NewCompositeKey(vectors.Hex(t, k.MLDSASeedHex), vectors.Hex(t, ed))
	if err != nil {
		t.Fatal(err)
	}
	return ck
}

func TestRecoveryVector(t *testing.T) {
	var v struct {
		CodeHex   string      `json:"code_hex"`
		CodeWords string      `json:"code_words"`
		PRKHex    string      `json:"prk_hex"`
		Key1      vectors.Key `json:"recovery_key_1"`
		Key2      vectors.Key `json:"recovery_key_2"`
		Key3      vectors.Key `json:"recovery_key_3"`
		WrapHex   string      `json:"recovery_wrap_hex"`
	}
	vectors.Load(t, "v2/recovery.json", &v)
	var commits struct {
		K1 struct {
			Commitment string `json:"commitment_sha256"`
		} `json:"recovery_key_1"`
		K2 struct {
			Commitment string `json:"commitment_sha256"`
		} `json:"recovery_key_2"`
		K3 struct {
			Commitment string `json:"commitment_sha256"`
		} `json:"recovery_key_3"`
	}
	vectors.Load(t, "v2/recovery.json", &commits)

	code := vectors.Hex(t, v.CodeHex)
	words, err := EncodeWords(code)
	if err != nil || strings.Join(words, " ") != v.CodeWords {
		t.Fatalf("words = %v, %v", words, err)
	}
	back, err := ParseWords(strings.ToUpper(v.CodeWords) + " ")
	if err != nil || !bytes.Equal(back, code) {
		t.Fatalf("decode = %x, %v", back, err)
	}
	// A swapped word fails the checksum.
	bad := slices.Clone(words)
	bad[0], bad[1] = bad[1], bad[0]
	if _, err := DecodeWords(bad); err == nil {
		t.Error("a swapped word passed the checksum")
	}

	r, err := NewRecovery(code)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(r.prk) != v.PRKHex {
		t.Errorf("prk = %x", r.prk)
	}
	for i, k := range []struct {
		key    vectors.Key
		commit string
	}{{v.Key1, commits.K1.Commitment}, {v.Key2, commits.K2.Commitment}, {v.Key3, commits.K3.Commitment}} {
		n := uint32(i + 1)
		if got := r.expand("signing", n, 32); hex.EncodeToString(got) != k.key.RecoverySeedHex {
			t.Errorf("recovery_seed_%d = %x", n, got)
		}
		if got := r.expand("signing-mldsa", n, 32); hex.EncodeToString(got) != k.key.MLDSASeedHex {
			t.Errorf("ML-DSA seed %d = %x", n, got)
		}
		key, err := r.Key(n)
		if err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(key.Public()) != k.key.PublicHex {
			t.Errorf("recovery key %d's public key differs", n)
		}
		c, _ := r.Commitment(n)
		if hex.EncodeToString(c[:]) != k.commit {
			t.Errorf("commitment %d = %x", n, c)
		}
	}
	if hex.EncodeToString(r.WrapKey()) != v.WrapHex {
		t.Errorf("recovery_wrap = %x", r.WrapKey())
	}
}

func TestTypeBack(t *testing.T) {
	_, w, err := NewRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.String(), w.Words()[0]) {
		t.Error("String shows the words")
	}
	pos := w.PickPositions(3)
	if len(pos) != 3 || !slices.IsSorted(pos) {
		t.Fatalf("positions %v", pos)
	}
	typed := []string{}
	for _, p := range pos {
		typed = append(typed, " "+strings.ToUpper(w.Words()[p]))
	}
	if !w.CheckTyped(pos, typed) {
		t.Error("the right words didn't match")
	}
	typed[0] = "zoo"
	if w.Words()[pos[0]] != "zoo" && w.CheckTyped(pos, typed) {
		t.Error("a wrong word matched")
	}
}

type chainVector struct {
	Entries []struct {
		Signed vectors.Signed `json:"signed"`
		Signer string         `json:"signer"`
	} `json:"entries"`
	FinalState finalState `json:"final_state"`
	IdentityID string     `json:"identity_id"`
}

type finalState struct {
	Devices            []string `json:"devices"`
	Handle             *string  `json:"handle"`
	HeadSeq            uint64   `json:"head_seq"`
	HeadSHA256         string   `json:"head_sha256"`
	Home               string   `json:"home"`
	RecoveryCommitment string   `json:"recovery_commitment"`
}

func checkState(t *testing.T, name string, st State, want finalState) {
	t.Helper()
	var devs []string
	for _, d := range st.Devices {
		devs = append(devs, hex.EncodeToString(d.Key))
	}
	slices.Sort(devs)
	wd := slices.Clone(want.Devices)
	slices.Sort(wd)
	if !slices.Equal(devs, wd) {
		t.Errorf("%s: devices %v, want %v", name, devs, wd)
	}
	if want.Handle != nil && st.Handle != *want.Handle || st.Home != want.Home || st.Seq != want.HeadSeq {
		t.Errorf("%s: handle %q home %q seq %d", name, st.Handle, st.Home, st.Seq)
	}
	if hex.EncodeToString(st.Head[:]) != want.HeadSHA256 {
		t.Errorf("%s: head hash differs", name)
	}
	if hex.EncodeToString(st.RecoveryCommit[:]) != want.RecoveryCommitment {
		t.Errorf("%s: recovery commitment differs", name)
	}
}

func TestChainVector(t *testing.T) {
	var v chainVector
	vectors.Load(t, "v2/chain.json", &v)
	var texts []string
	for _, e := range v.Entries {
		texts = append(texts, e.Signed.Signed)
	}
	entries := parse(t, texts...)
	c, err := Validate(entries, Options{Now: later})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID() != v.IdentityID {
		t.Errorf("identifier %s, want %s", c.ID(), v.IdentityID)
	}
	checkState(t, "chain.json", c.State, v.FinalState)
	if !c.Info[2].Recovery || c.Info[1].Recovery || !c.Info[3].SelfRotation() || c.Info[1].SelfRotation() {
		t.Error("signer kinds differ")
	}
	// The future check: a clock before the entries refuses them.
	if _, err := Validate(entries, Options{Now: time.Unix(1794700000, 0)}); err == nil {
		t.Error("accepted entries from the future")
	}
	// Every strict prefix is a valid chain too, and dropping or reordering
	// an entry breaks it.
	for i := 1; i <= len(entries); i++ {
		if _, err := Validate(entries[:i], Options{}); err != nil {
			t.Errorf("prefix %d: %v", i, err)
		}
	}
	if _, err := Validate([]wire.Signed{entries[0], entries[2]}, Options{}); err == nil {
		t.Error("accepted a chain with a gap")
	}
	if _, err := Validate(entries[1:], Options{}); err == nil {
		t.Error("accepted a chain without genesis")
	}
}

// TestBuildChain builds a chain the way a client does and validates it: a
// genesis, an add-device, a recovery-signed entry, and a self-rotation.
func TestBuildChain(t *testing.T) {
	dev, err := GenerateDevice("Kat's Laptop")
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := NewRecoveryCode()
	r, _ := NewRecovery(code)
	c1, _ := r.Commitment(1)
	g, err := NewGenesis(dev, "chat.example.com", "kat.chat.example.com", c1, 1794700800)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Validate([]wire.Signed{g}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if i, err := r.Find(&c.State); err != nil || i != 1 {
		t.Fatalf("Find = %d, %v", i, err)
	}
	dev2, _ := GenerateDevice("Kat's Desktop")
	e1, _ := dev.SignEntry(c.Next(1794700900, AddDevice(dev2.Key.Public(), dev2.Name)))
	if err := c.Append(e1, Options{}); err != nil {
		t.Fatal(err)
	}
	// A device can't sign set-recovery.
	c2, _ := r.Commitment(2)
	if bad, _ := dev.SignEntry(c.Next(1794701000, SetRecovery(c2))); c.Append(bad, Options{}) == nil {
		t.Fatal("a device signed set-recovery")
	}
	k1, _ := r.Key(1)
	re, _ := SignWithRecovery(k1, c.Next(1794701000, RemoveDevice(dev2.ID()), SetRecovery(c2)))
	if err := c.Append(re, Options{}); err != nil {
		t.Fatal(err)
	}
	if i, err := r.Find(&c.State); err != nil || i != 2 {
		t.Fatalf("Find after use = %d, %v", i, err)
	}
	// Key 1 is spent.
	if again, _ := SignWithRecovery(k1, c.Next(1794701100, SetHandle(""), SetRecovery(c2))); c.Append(again, Options{}) == nil {
		t.Fatal("a spent recovery key signed")
	}
	dev3, _ := GenerateDevice("Kat's Laptop")
	rot, _ := dev.SignEntry(c.Next(1794701200, RemoveDevice(dev.ID()), AddDevice(dev3.Key.Public(), "")))
	if err := c.Append(rot, Options{}); err != nil {
		t.Fatal(err)
	}
	if !c.Info[3].SelfRotation() || c.State.IsDevice(dev.ID()) || !c.State.IsDevice(dev3.ID()) {
		t.Error("self-rotation state")
	}
	// The removed key can't sign anymore.
	if late, _ := dev.SignEntry(c.Next(1794701300, SetHome("evil.example.net"))); c.Append(late, Options{}) == nil {
		t.Fatal("a removed device signed")
	}
	// Eleven devices are too many.
	cc := c.Prefix(c.Len())
	for i := range MaxDevices {
		d, _ := GenerateDevice("")
		e, _ := dev3.SignEntry(cc.Next(1794701300+int64(i), AddDevice(d.Key.Public(), "")))
		err := cc.Append(e, Options{})
		if i < MaxDevices-1 && err != nil {
			t.Fatalf("device %d: %v", i+2, err)
		}
		if i == MaxDevices-1 && err == nil {
			t.Fatal("accepted an 11th device")
		}
	}
}

func TestForkVector(t *testing.T) {
	var v struct {
		Cases []struct {
			Cached     []string    `json:"cached"`
			Candidate  []string    `json:"candidate"`
			FinalState *finalState `json:"final_state"`
			ForkAt     int         `json:"fork_at"`
			Name       string      `json:"name"`
			Outcome    string      `json:"outcome"`
			Warning    string      `json:"warning"`
		} `json:"cases"`
		Entries map[string]struct {
			Signed vectors.Signed `json:"signed"`
		} `json:"entries"`
	}
	vectors.Load(t, "v2/fork.json", &v)
	pick := func(names []string) []wire.Signed {
		var texts []string
		for _, n := range names {
			texts = append(texts, v.Entries[n].Signed.Signed)
		}
		return parse(t, texts...)
	}
	for _, tc := range v.Cases {
		cached, err := Validate(pick(tc.Cached), Options{Now: later})
		if err != nil {
			t.Fatalf("%s: cached: %v", tc.Name, err)
		}
		got := Compare(cached, pick(tc.Candidate), Options{Now: later})
		if got.Outcome.String() != tc.Outcome || got.Warning != tc.Warning {
			t.Errorf("%s: %s %q, want %s %q (%v)", tc.Name, got.Outcome, got.Warning, tc.Outcome, tc.Warning, got.Err)
			continue
		}
		if tc.Outcome == "fork" && got.ForkAt != tc.ForkAt {
			t.Errorf("%s: fork at %d, want %d", tc.Name, got.ForkAt, tc.ForkAt)
		}
		if tc.FinalState != nil {
			checkState(t, tc.Name, got.Chain.State, *tc.FinalState)
		}
	}
}

func TestVouchVector(t *testing.T) {
	var v struct {
		Cases []struct {
			Accepted     any      `json:"accepted"`
			Cached       []string `json:"cached"`
			Candidate    []string `json:"candidate"`
			From         string   `json:"from"`
			Name         string   `json:"name"`
			PreviousHome struct {
				Answers any    `json:"answers"`
				Server  string `json:"server"`
			} `json:"previous_home"`
		} `json:"cases"`
	}
	vectors.Load(t, "v2/vouch.json", &v)
	for _, tc := range v.Cases {
		cand, err := Validate(parse(t, tc.Candidate...), Options{Now: later})
		if err != nil {
			t.Fatalf("%s: %v", tc.Name, err)
		}
		asked := 0
		ask := func(server string) HomeAnswer {
			asked++
			if server != tc.PreviousHome.Server {
				return HomeAnswer{Unreachable: true}
			}
			switch a := tc.PreviousHome.Answers.(type) {
			case string:
				if a == "404" {
					return HomeAnswer{Gone: true}
				}
				return HomeAnswer{Unreachable: true}
			case map[string]any:
				var texts []string
				for _, s := range a["chain"].([]any) {
					texts = append(texts, s.(string))
				}
				return HomeAnswer{Chain: parse(t, texts...)}
			}
			t.Fatalf("%s: answer %v", tc.Name, tc.PreviousHome.Answers)
			return HomeAnswer{}
		}
		var got any
		if tc.Cached == nil {
			if err := FirstFetch(cand, tc.From, ask); err != nil {
				got = "reject"
			} else {
				got = float64(cand.Len())
			}
		} else {
			cached, err := Validate(parse(t, tc.Cached...), Options{})
			if err != nil {
				t.Fatal(err)
			}
			cmp := Compare(cached, cand.Entries, Options{Now: later})
			if cmp.Outcome != Extends {
				t.Fatalf("%s: candidate doesn't extend the cache", tc.Name)
			}
			got = float64(Vouch(cand, cached.Len(), tc.From, ask))
		}
		if got != tc.Accepted {
			t.Errorf("%s: accepted %v, want %v", tc.Name, got, tc.Accepted)
		}
		if asked > 1 {
			t.Errorf("%s: asked %d times", tc.Name, asked)
		}
	}
}

func TestBasisVector(t *testing.T) {
	type bound struct {
		Hash string   `json:"hash"`
		Seq  uint64   `json:"seq"`
		Rec  *uint64  `json:"rec"`
		Tail []string `json:"tail"`
	}
	var v struct {
		Cases []struct {
			Basis bound `json:"basis"`
			Check struct {
				KeyHex string `json:"key_hex"`
				Kind   string `json:"kind"`
			} `json:"check"`
			Copy     []string `json:"copy"`
			Expected string   `json:"expected"`
			Floor    *bound   `json:"floor"`
			Name     string   `json:"name"`
		} `json:"cases"`
		IdentityID string `json:"identity_id"`
	}
	vectors.Load(t, "v2/basis.json", &v)
	toBound := func(b bound) Bound {
		h, err := wire.DecodeB64(b.Hash)
		if err != nil {
			t.Fatal(err)
		}
		return Bound{Seq: b.Seq, Hash: h, Rec: b.Rec}
	}
	for _, tc := range v.Cases {
		b := Basis{Bound: toBound(tc.Basis), Tail: parse(t, tc.Basis.Tail...)}
		var floor *Bound
		if tc.Floor != nil {
			f := toBound(*tc.Floor)
			floor = &f
		}
		kind := MustBeDevice
		if tc.Check.Kind == "not-device" {
			kind = MustNotBeDevice
		}
		var copy []wire.Signed
		if tc.Copy != nil {
			copy = parse(t, tc.Copy...)
		}
		got, _, err := JudgeBasis(v.IdentityID, b, copy, floor, kind, vectors.Hex(t, tc.Check.KeyHex))
		if got.String() != tc.Expected {
			t.Errorf("%s: %s, want %s (%v)", tc.Name, got, tc.Expected, err)
		}
	}
}

func TestMsgKeyVector(t *testing.T) {
	var v struct {
		IdentityID string `json:"identity_id"`
		KeyPackage struct {
			Bytes  string `json:"bytes_utf8"`
			SHA256 string `json:"sha256_hex"`
		} `json:"key_package"`
		KPBinding vectors.Signed `json:"kp_binding"`
		MK1       struct {
			PublicHex string `json:"public_hex"`
			SeedHex   string `json:"seed_hex"`
		} `json:"message_key_1"`
		MsgKey1 vectors.Signed `json:"msg_key_1"`
		MsgKey2 vectors.Signed `json:"msg_key_2"`
		Reject  []struct {
			Note   string `json:"note"`
			Signed string `json:"signed"`
		} `json:"reject"`
	}
	vectors.Load(t, "v2/msgkey.json", &v)
	data := map[string]any{}
	vectors.Load(t, "v2/chain.json", &data)
	keys := data["keys"].(map[string]any)
	d1 := keys["device_1"].(map[string]any)
	devKey := vkey(t, vectors.Key{MLDSASeedHex: d1["mldsa_seed_hex"].(string), Ed25519SeedHex: d1["ed25519_seed_hex"].(string)})
	pub := devKey.Public()

	for i, mk := range []vectors.Signed{v.MsgKey1, v.MsgKey2} {
		s := parse(t, mk.Signed)[0]
		b, err := VerifyMsgKey(s, v.IdentityID, pub)
		if err != nil || b.N != uint64(i+1) {
			t.Errorf("msg_key_%d: %v", i+1, err)
		}
	}
	// Deterministic signing reproduces msg_key_1.
	dev := &Device{Key: devKey, MsgKey: ed25519.NewKeyFromSeed(vectors.Hex(t, v.MK1.SeedHex)), MsgN: 1}
	body := MsgKeyBody{ID: v.IdentityID, Key: wire.Bytes(dev.MsgKey.Public().(ed25519.PublicKey)), N: 1}
	h := sha256.Sum256(pub)
	body.Dev = h[:]
	again, err := sig.SignDeviceDeterministic(devKey, TypeMsgKey, body)
	if err != nil || string(again.Canonical()) != v.MsgKey1.Signed {
		t.Error("msg_key_1 doesn't reproduce")
	}
	// Our own certificate verifies.
	cert, err := dev.MsgKeyCert(v.IdentityID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyMsgKey(cert, v.IdentityID, pub); err != nil {
		t.Error(err)
	}
	for _, r := range v.Reject {
		s, err := wire.ParseSigned([]byte(r.Signed))
		if err == nil {
			if _, err = VerifyMsgKey(s, v.IdentityID, pub); err == nil {
				t.Errorf("accepted: %s", r.Note)
			}
		}
	}

	kp := []byte(v.KeyPackage.Bytes)
	if hh := sha256.Sum256(kp); hex.EncodeToString(hh[:]) != v.KeyPackage.SHA256 {
		t.Fatal("key package hash")
	}
	bs := parse(t, v.KPBinding.Signed)[0]
	if err := VerifyKPBinding(bs, v.IdentityID, pub, kp); err != nil {
		t.Error(err)
	}
	if VerifyKPBinding(bs, v.IdentityID, pub, []byte("another")) == nil {
		t.Error("a binding verified for another key package")
	}
	own, _ := dev.KPBinding(v.IdentityID, kp)
	if err := VerifyKPBinding(own, v.IdentityID, pub, kp); err != nil {
		t.Error(err)
	}
}

func TestHandleSyntax(t *testing.T) {
	for _, h := range []string{"kat.example.com", "a.b", "x-1.example.org", "xn--bcher-kva.example"} {
		if err := CheckHandle(h); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
	long := strings.Repeat("a", 63)
	for _, h := range []string{"example", "Kat.example.com", "-a.example.com", "a-.example.com", "a..example.com", "kät.example.com", long + "a.example.com", strings.Repeat(long+".", 4) + "com", "a_b.example.com"} {
		if CheckHandle(h) == nil {
			t.Errorf("accepted %q", h)
		}
	}
	if NormalizeHandle(" @Kat.Example.COM. ") != "kat.example.com" {
		t.Error("NormalizeHandle")
	}
	if CheckServerName("127.0.0.1") == nil {
		t.Error("accepted an IP address as a server name")
	}
	if !ReservedLabel("admin") || ReservedLabel("kat") {
		t.Error("ReservedLabel")
	}
}

func TestMLSIdentity(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	got := string(MLSIdentity("tr:aaaaaaaaaaaaaaaaaaaaaaaa", key))
	if got != `["tr:aaaaaaaaaaaaaaaaaaaaaaaa","AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"]` {
		t.Error(got)
	}
}
