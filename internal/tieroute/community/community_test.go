package community

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/codes"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/internal/vectors"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

func TestHistoryVectors(t *testing.T) {
	var v struct {
		ReaderKeys []string          `json:"reader_keys_hex"`
		Devices    map[string]string `json:"msg_key_devices"`
		Cases      []struct {
			Name       string `json:"name"`
			AAD        string `json:"aad"`
			AESKey     string `json:"aes_key_hex"`
			Channel    uint64 `json:"channel"`
			Community  string `json:"community_hex"`
			CT         string `json:"ct_hex"`
			Start      uint64 `json:"start"`
			End        uint64 `json:"end"`
			Group      string `json:"group_hex"`
			HistoryKey string `json:"history_key_hex"`
			Nonce      string `json:"nonce_hex"`
			Plaintext  string `json:"plaintext"`
		} `json:"cases"`
	}
	vectors.Load(t, "v2/community-history.json", &v)
	var readerKeys [][]byte
	for _, k := range v.ReaderKeys {
		readerKeys = append(readerKeys, vectors.Hex(t, k))
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			hk, community := vectors.Hex(t, c.HistoryKey), vectors.Hex(t, c.Community)
			k, err := SegmentKey(hk)
			if err != nil || !bytes.Equal(k, vectors.Hex(t, c.AESKey)) {
				t.Fatalf("the AES key: %x %v", k, err)
			}
			if got := string(SegmentAAD(community, c.Channel, c.Start, c.End)); got != c.AAD {
				t.Fatalf("the associated data: %s", got)
			}
			ct, err := encryptSegmentNonce(hk, community, c.Channel, c.Start, c.End, []byte(c.Plaintext), vectors.Hex(t, c.Nonce))
			if err != nil || !bytes.Equal(ct, vectors.Hex(t, c.CT)) {
				t.Fatalf("the ciphertext differs: %v", err)
			}
			// A reader tries each key it holds.
			pt, ok := DecryptSegment(readerKeys, community, c.Channel, c.Start, c.End, ct)
			if !ok || string(pt) != c.Plaintext {
				t.Fatal("the reader couldn't open the segment")
			}
			if _, ok := DecryptSegment(readerKeys, community, c.Channel, c.Start, c.End+1, ct); ok {
				t.Fatal("a segment opened for another range")
			}
			items, err := ReadSegment(pt, vectors.Hex(t, c.Group), c.Start, c.End)
			if err != nil || len(items) == 0 {
				t.Fatalf("the items: %v %v", items, err)
			}
			for _, it := range items {
				if it.Msg == nil {
					continue
				}
				var mk identity.MsgKeyBody
				if err := it.MsgKey.Body(&mk); err != nil {
					t.Fatal(err)
				}
				if err := sig.VerifyMessageKey(*it.Msg, mk.Key); err != nil {
					t.Fatalf("message %d: %v", it.Seq, err)
				}
				var am struct {
					ID    string     `json:"id"`
					Group wire.Bytes `json:"group"`
				}
				it.Msg.Body(&am)
				if am.ID != mk.ID || !bytes.Equal(am.Group, vectors.Hex(t, c.Group)) {
					t.Fatalf("message %d names %s in another group", it.Seq, am.ID)
				}
				if dev := v.Devices[mk.ID]; dev != "" && !bytes.Equal(mk.Dev, vectors.Hex(t, dev)) {
					t.Fatalf("message %d's msg-key names another device", it.Seq)
				}
			}
		})
	}
}

func TestCodeVector(t *testing.T) {
	var v struct {
		Code      string `json:"code"`
		Community string `json:"community_id_hex"`
		CodeKey   struct {
			Public string `json:"public_hex"`
		} `json:"code_key"`
		Commit struct {
			SHA256 string `json:"sha256_hex"`
		} `json:"commit"`
		Proof struct {
			Signed string `json:"signed"`
		} `json:"join_proof"`
	}
	vectors.Load(t, "v2/code.json", &v)
	server, secret, err := codes.ParseJoinCode(v.Code)
	if err != nil || server != "community.example.com" {
		t.Fatal(server, err)
	}
	key, err := codes.CodeKey(secret, vectors.Hex(t, v.Community))
	if err != nil || !bytes.Equal(key.Public(), vectors.Hex(t, v.CodeKey.Public)) {
		t.Fatalf("the code key: %v", err)
	}
	s, err := wire.ParseSigned([]byte(v.Proof.Signed))
	if err != nil {
		t.Fatal(err)
	}
	p, pub, err := VerifyJoinProof(s)
	if err != nil || !bytes.Equal(pub, key.Public()) || !bytes.Equal(p.Commit, vectors.Hex(t, v.Commit.SHA256)) || p.Epoch != 42 {
		t.Fatalf("the join proof: %+v %v", p, err)
	}
	// One signed here verifies the same way.
	mine, err := SignJoinProof(key, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, pub2, err := VerifyJoinProof(mine); err != nil || !bytes.Equal(pub2, pub) {
		t.Fatal(err)
	}
}

// member is a test identity with one device.
type member struct {
	id  string
	key *sig.CompositeKey
}

func newMember(t *testing.T, name string) member {
	k, err := sig.GenerateCompositeKey()
	if err != nil {
		t.Fatal(err)
	}
	return member{id: "tr:" + strings.Repeat(name, 24)[:24], key: k}
}

type testChain struct {
	t       *testing.T
	members map[string]member
	c       *Chain
	now     int64
}

func (tc *testChain) checker() Checker {
	return Checker{Now: tc.now, DeviceKey: func(id string, k []byte) ([]byte, error) {
		m, ok := tc.members[id]
		if !ok || !bytes.Equal(m.key.EdPublic(), k) {
			return nil, ErrNoChain
		}
		return m.key.Public(), nil
	}}
}

func (tc *testChain) entry(by member, ops ...map[string]any) wire.Signed {
	tc.t.Helper()
	body := map[string]any{"seq": uint64(0), "ts": tc.now, "by": by.id, "ops": ops}
	if tc.c != nil && tc.c.Len() > 0 {
		body["seq"] = uint64(tc.c.Len())
		body["prev"] = wire.Bytes(tc.c.Head())
		body["community"] = wire.Bytes(tc.c.ID())
	}
	s, err := sig.SignDevice(by.key, TypeEntry, body)
	if err != nil {
		tc.t.Fatal(err)
	}
	return s
}

func (tc *testChain) append(by member, ops ...map[string]any) error {
	s := tc.entry(by, ops...)
	if tc.c == nil {
		c, err := Validate([]wire.Signed{s}, tc.checker())
		if err == nil {
			tc.c = c
		}
		return err
	}
	return tc.c.Append(s, tc.checker())
}

func (tc *testChain) must(by member, ops ...map[string]any) {
	tc.t.Helper()
	if err := tc.append(by, ops...); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *testChain) refuse(why string, by member, ops ...map[string]any) {
	tc.t.Helper()
	n := tc.c.Len()
	if err := tc.append(by, ops...); err == nil {
		tc.t.Fatalf("%s: the entry was accepted", why)
	}
	if tc.c.Len() != n {
		tc.t.Fatalf("%s: a refused entry changed the chain", why)
	}
}

func sealed(t *testing.T, key, community []byte, seq uint64, what string, id uint64, field, value string) Sealed {
	s, err := Seal(key, community, seq, what, id, field, value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestChainRules(t *testing.T) {
	kat, ravi, lee := newMember(t, "kat"), newMember(t, "ravi"), newMember(t, "lee")
	tc := &testChain{t: t, members: map[string]member{kat.id: kat, ravi.id: ravi, lee.id: lee}, now: time.Now().Unix()}
	tc.must(kat, map[string]any{"op": "create", "server": "community.example.com", "name": "Night shift",
		"public": false, "approval": false, "everyone": []string{"talk", "invite"}})
	id := tc.c.ID()
	st := tc.c.State()
	if st.Owner != kat.id || st.Name != "Night shift" || !st.Can(ravi.id, PermTalk) || st.Can(ravi.id, PermModerate) {
		t.Fatalf("the state after create: %+v", st)
	}
	meta := make([]byte, 32)
	rand.Read(meta)

	// The owner adds a role, assigns it to Ravi, and adds channels.
	mods := map[string]any{"op": "add-role", "id": 1, "rank": 10, "perms": []string{"talk", "moderate"},
		"name": sealed(t, meta, id, 1, WhatRole, 1, FieldName, "Mods")}
	assign := map[string]any{"op": "assign-roles", "member": ravi.id, "roles": []uint64{1}}
	tc.must(kat, mods, assign)
	general := map[string]any{"op": "add-channel", "id": 1, "name": sealed(t, meta, id, 2, WhatChannel, 1, FieldName, "general"),
		"see": []uint64{0}, "write": []uint64{0}, "kind": "text", "history": true, "slow": 0}
	back := map[string]any{"op": "add-channel", "id": 2, "name": sealed(t, meta, id, 2, WhatChannel, 2, FieldName, "backroom"),
		"see": []uint64{1}, "write": []uint64{1}, "kind": "text", "history": false, "slow": 0}
	lounge := map[string]any{"op": "add-channel", "id": 3, "name": sealed(t, meta, id, 2, WhatChannel, 3, FieldName, "Lounge"),
		"see": []uint64{0}, "write": []uint64{0}, "kind": "voice", "max": 25, "cast": 2}
	tc.must(kat, general, back, lounge)
	st = tc.c.State()
	if !st.Can(ravi.id, PermModerate) || !st.Outranks(ravi.id, lee.id) || st.Outranks(ravi.id, kat.id) {
		t.Fatal("ravi's role")
	}
	if !st.SeesChannel(ravi.id, st.Channels[2]) || st.SeesChannel(lee.id, st.Channels[2]) || !st.Channels[2].Restricted() {
		t.Fatal("the restricted channel")
	}
	ch := st.Channels[2]
	if name, ok := Open([][]byte{meta}, ch.Name, id, ch.NameSeq, WhatChannel, 2, FieldName); !ok || name != "backroom" {
		t.Fatalf("the sealed name: %q %v", name, ok)
	}
	if _, ok := Open([][]byte{meta}, ch.Name, id, ch.NameSeq, WhatChannel, 1, FieldName); ok {
		t.Fatal("a sealed name opened for another channel")
	}

	// Refusals.
	tc.refuse("a non-admin adding a channel", lee, map[string]any{"op": "add-channel", "id": 4, "name": sealed(t, meta, id, 3, WhatChannel, 4, FieldName, "x"),
		"see": []uint64{0}, "write": []uint64{0}, "kind": "text", "history": true, "slow": 0})
	tc.refuse("a skipped channel ID", kat, map[string]any{"op": "add-channel", "id": 9, "name": sealed(t, meta, id, 3, WhatChannel, 9, FieldName, "x"),
		"see": []uint64{0}, "write": []uint64{0}, "kind": "text", "history": true, "slow": 0})
	tc.refuse("a moderator timing out the owner", ravi, map[string]any{"op": "timeout", "member": kat.id, "until": tc.now + 60})
	tc.refuse("a timeout over 7 days", ravi, map[string]any{"op": "timeout", "member": lee.id, "until": tc.now + 700000})
	tc.refuse("an unknown member", kat, map[string]any{"op": "set-settings", "public": true, "approval": false, "extra": 1})
	tc.refuse("an unknown op", kat, map[string]any{"op": "summon"})
	tc.refuse("a role with more than the signer has", ravi, map[string]any{"op": "add-role", "id": 2, "rank": 5, "perms": []string{"administrate"},
		"name": sealed(t, meta, id, 3, WhatRole, 2, FieldName, "x")})
	tc.refuse("everyone with moderate", kat, map[string]any{"op": "edit-role", "id": 0, "perms": []string{"talk", "moderate"}})
	tc.refuse("a name with an emoji", kat, map[string]any{"op": "set-name", "name": "Night 🌙"})

	// A moderator times Lee out, and a code: Lee, with invite from
	// everyone, makes one.
	tc.must(ravi, map[string]any{"op": "timeout", "member": lee.id, "until": tc.now + 60})
	if !tc.c.State().TimedOut(lee.id, tc.now) || tc.c.State().Writes(lee.id, tc.c.State().Channels[1], tc.now) {
		t.Fatal("the timeout")
	}
	code, _ := sig.GenerateCompositeKey()
	tc.must(lee, map[string]any{"op": "add-code", "key": wire.Bytes(code.Public()), "exp": tc.now + 3600, "uses": 1})
	st = tc.c.State()
	if !st.CodeLive(code.Public(), tc.now, 0) || st.CodeLive(code.Public(), tc.now, 1) || st.CodeLive(code.Public(), tc.now+3600, 0) {
		t.Fatal("the code's liveness")
	}
	tc.refuse("a code key used twice", kat, map[string]any{"op": "add-code", "key": wire.Bytes(code.Public()), "exp": tc.now + 3600, "uses": 1})
	// Banning Lee clears the code's liveness, and her timeout.
	tc.must(kat, map[string]any{"op": "ban", "member": lee.id})
	st = tc.c.State()
	if st.CodeLive(code.Public(), tc.now, 0) || !st.Banned(lee.id, "") || st.TimedOut(lee.id, tc.now) {
		t.Fatal("the ban")
	}
	tc.refuse("an entry by a banned identity", lee, map[string]any{"op": "assign-roles", "member": lee.id, "roles": []uint64{}})
	// The state as of an earlier entry still has the code.
	before, _ := tc.c.StateAt(uint64(tc.c.Len() - 2))
	if !before.CodeLive(code.Public(), tc.now, 0) {
		t.Fatal("StateAt")
	}
	// Ravi removes his own role.
	tc.must(ravi, map[string]any{"op": "assign-roles", "member": ravi.id, "roles": []uint64{}})
	if tc.c.State().Can(ravi.id, PermModerate) {
		t.Fatal("ravi still moderates")
	}
	// A whole chain validates again from its entries.
	again, err := Validate(tc.c.Entries, tc.checker())
	if err != nil || !bytes.Equal(again.Head(), tc.c.Head()) {
		t.Fatal(err)
	}
	// An unknown signer's chain leaves the entry undecided.
	stranger := newMember(t, "zed")
	if err := tc.append(stranger, map[string]any{"op": "assign-roles", "member": stranger.id, "roles": []uint64{}}); !errors.Is(err, ErrNoChain) {
		t.Fatalf("a stranger's entry: %v", err)
	}
}

func TestNames(t *testing.T) {
	for _, n := range []string{"general", "Night shift", "café"} {
		if err := CheckName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", " lead", "trail ", "a​b", "x‮y", "🎉", strings.Repeat("a", 65), "café"} {
		if err := CheckName(n); err == nil {
			t.Errorf("%q passed", n)
		}
	}
}
