package mls

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

// The extension types the tests list, as Tieroute's do (§7.1, §11.1).
const (
	extGroup  uint16 = 0xF0A1
	extMsgKey uint16 = 0xF0A2
)

var chat = Config{RatchetTreeInWelcome: true, MaxPastEpochs: 2}

type device struct {
	t    *testing.T
	name string
	c    *Client
	s    *Signer
}

func newDevice(t *testing.T, name string) *device {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.PublicKey(), priv.Public().(ed25519.PublicKey)) {
		t.Fatal("the signer's public key doesn't match")
	}
	c, err := Open(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &device{t: t, name: name, c: c, s: s}
}

func (d *device) leaf() Leaf {
	return Leaf{
		Identity:     []byte(d.name),
		Capabilities: Capabilities{Extensions: []uint16{extGroup, extMsgKey}},
		Extensions:   []Extension{{Type: extMsgKey, Data: []byte("msg-key of " + d.name)}},
	}
}

func (d *device) keyPackage(lastResort bool) *KeyPackage {
	d.t.Helper()
	kp, err := d.c.NewKeyPackage(d.s, KeyPackageOptions{Leaf: d.leaf(), LastResort: lastResort})
	if err != nil {
		d.t.Fatal(err)
	}
	return kp
}

func (d *device) join(welcome []byte) *Group {
	d.t.Helper()
	g, res, err := d.c.JoinWelcome(welcome, nil, chat)
	if err != nil {
		d.t.Fatal(err)
	}
	if res.SenderLeafIndex != 0 {
		d.t.Fatalf("the Welcome's sender is leaf %d", res.SenderLeafIndex)
	}
	return g
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func groupID() []byte {
	id := make([]byte, 32)
	rand.Read(id)
	return id
}

func TestCiphersuite(t *testing.T) {
	if LibraryCiphersuite() != 0x004F || Ciphersuite != 0x004F {
		t.Fatalf("the ciphersuite is %#04x", LibraryCiphersuite())
	}
	if CiphersuiteName != "MLS_128_MLKEM768X25519_AES128GCM_SHA256_Ed25519" {
		t.Fatal(CiphersuiteName)
	}
	kat := newDevice(t, "kat")
	info := must[*KeyPackageInfo](t)(InspectKeyPackage(kat.keyPackage(false).Raw))
	if info.Ciphersuite != 0x004F || len(info.Leaf.Capabilities.Ciphersuites) != 1 || info.Leaf.Capabilities.Ciphersuites[0] != 0x004F {
		t.Fatalf("the key package lists %#04x and %v", info.Ciphersuite, info.Leaf.Capabilities.Ciphersuites)
	}
}

func TestKeyPackages(t *testing.T) {
	kat := newDevice(t, "kat")
	kp := kat.keyPackage(false)
	info := must[*KeyPackageInfo](t)(InspectKeyPackage(kp.Raw))
	if !bytes.Equal(info.Leaf.Identity, []byte("kat")) || !bytes.Equal(info.Leaf.SignatureKey, kat.s.PublicKey()) {
		t.Fatal("the key package's leaf is wrong")
	}
	if info.Leaf.CredentialType != CredentialBasic || !bytes.Equal(info.HashRef, kp.HashRef) || info.LastResort {
		t.Fatalf("unexpected key package: %+v", info)
	}
	if d, ok := info.Leaf.Extension(extMsgKey); !ok || string(d) != "msg-key of kat" {
		t.Fatal("the leaf extension is missing")
	}
	if info.NotAfter <= info.NotBefore {
		t.Fatal("the lifetime is empty")
	}
	if m := must[*MessageInfo](t)(InspectMessage(kp.Message)); m.WireFormat != "key_package" {
		t.Fatal(m.WireFormat)
	}
	lr := kat.keyPackage(true)
	if !must[*KeyPackageInfo](t)(InspectKeyPackage(lr.Message)).LastResort {
		t.Fatal("not last resort")
	}
	bad := bytes.Clone(kp.Raw)
	bad[len(bad)-1] ^= 1
	if _, err := InspectKeyPackage(bad); !IsCode(err, ErrInvalid) {
		t.Fatalf("a tampered key package gave %v", err)
	}
}

func TestEndToEnd(t *testing.T) {
	kat, ravi, amara := newDevice(t, "kat"), newDevice(t, "ravi"), newDevice(t, "amara")
	id := groupID()
	exts := []Extension{RequiredCapabilities([]uint16{extGroup}, nil, nil), {Type: extGroup, Data: []byte(`{"kind":"group"}`)}}
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: id, Leaf: kat.leaf(), Extensions: exts, Config: chat}))

	// Kat adds Ravi and Amara, the last on her last-resort key package.
	res := must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Adds: [][]byte{ravi.keyPackage(false).Raw, amara.keyPackage(true).Message}}))
	if res.Epoch != 0 || res.Welcome == nil {
		t.Fatalf("commit in epoch %d, welcome %d bytes", res.Epoch, len(res.Welcome))
	}
	// The next epoch's exporter is available before the merge (§11.6).
	next := must[[]byte](t)(kg.ExportPendingSecret("tieroute hub", id, 32))
	if err := kg.MergePendingCommit(); err != nil {
		t.Fatal(err)
	}
	if now := must[[]byte](t)(kg.ExportSecret("tieroute hub", id, 32)); !bytes.Equal(now, next) {
		t.Fatal("the pending export differs from the merged one")
	}
	rg, ag := ravi.join(res.Welcome), amara.join(res.Welcome)

	// Everyone agrees on the members and the exporter.
	st := must[*State](t)(rg.State())
	if st.Epoch != 1 || len(st.Members) != 3 || st.OwnLeafIndex != 1 {
		t.Fatalf("Ravi's state: %+v", st)
	}
	if m, ok := st.Member(2); !ok || string(m.Identity) != "amara" || !bytes.Equal(m.SignatureKey, amara.s.PublicKey()) {
		t.Fatal("Amara's leaf is wrong")
	}
	if d, ok := st.Extension(extGroup); !ok || string(d) != `{"kind":"group"}` {
		t.Fatal("the group extension is missing")
	}
	secret := must[[]byte](t)(kg.ExportSecret("SFrame 1.0 Tieroute call", []byte("call-id-16-bytes"), 16))
	for _, g := range []*Group{rg, ag} {
		if s := must[[]byte](t)(g.ExportSecret("SFrame 1.0 Tieroute call", []byte("call-id-16-bytes"), 16)); !bytes.Equal(s, secret) {
			t.Fatal("exporter secrets differ")
		}
	}

	// Messages both ways.
	msg := must[[]byte](t)(kg.Encrypt(kat.s, []byte("hi"), []byte("ad")))
	if mi := must[*MessageInfo](t)(InspectMessage(msg)); mi.WireFormat != "private_message" || *mi.Epoch != 1 || mi.ContentType != "application" {
		t.Fatalf("message info: %+v", mi)
	}
	for _, g := range []*Group{rg, ag} {
		p := must[*Processed](t)(g.Process(msg))
		if p.Kind != KindApplication || string(p.Plaintext) != "hi" || string(p.Identity) != "kat" || string(p.AuthenticatedData) != "ad" || p.Sender.Index != 0 {
			t.Fatalf("processed: %+v", p)
		}
	}
	msg = must[[]byte](t)(rg.Encrypt(ravi.s, []byte("hello back"), nil))
	if p := must[*Processed](t)(kg.Process(msg)); string(p.Plaintext) != "hello back" || string(p.Identity) != "ravi" {
		t.Fatalf("processed: %+v", p)
	}
	if p := must[*Processed](t)(ag.Process(msg)); string(p.Plaintext) != "hello back" {
		t.Fatalf("processed: %+v", p)
	}

	// Ravi's state survives a snapshot and restore.
	snap := must[[]byte](t)(ravi.c.Snapshot())
	ravi.c.Close()
	ravi.c = must[*Client](t)(Open(snap))
	rg = ravi.c.Group(id)
	msg = must[[]byte](t)(ag.Encrypt(amara.s, []byte("after restore"), nil))
	if p := must[*Processed](t)(rg.Process(msg)); string(p.Plaintext) != "after restore" {
		t.Fatal("Ravi can't read after the restore")
	}

	// Kat removes Amara.
	res = must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Removes: []uint32{2}, UpdatePath: true}))
	if err := kg.MergePendingCommit(); err != nil {
		t.Fatal(err)
	}
	p := must[*Processed](t)(rg.Process(res.Commit))
	if p.Kind != KindCommit || p.Commit.NewEpoch != 2 || p.Commit.PathLeaf == nil {
		t.Fatalf("processed: %+v", p)
	}
	if len(p.Commit.Proposals) != 1 || p.Commit.Proposals[0].Kind != "remove" || *p.Commit.Proposals[0].Removed != 2 {
		t.Fatalf("proposals: %+v", p.Commit.Proposals)
	}
	if err := rg.MergeStagedCommit(); err != nil {
		t.Fatal(err)
	}
	p = must[*Processed](t)(ag.Process(res.Commit))
	if !p.Commit.SelfRemoved {
		t.Fatal("Amara isn't told she's removed")
	}
	if err := ag.MergeStagedCommit(); err != nil {
		t.Fatal(err)
	}
	if must[*State](t)(ag.State()).Active {
		t.Fatal("Amara's group is still active")
	}

	// Amara can't read what's sent after her removal.
	msg = must[[]byte](t)(kg.Encrypt(kat.s, []byte("without amara"), nil))
	if p := must[*Processed](t)(rg.Process(msg)); string(p.Plaintext) != "without amara" {
		t.Fatal("Ravi can't read")
	}
	if _, err := ag.Process(msg); err == nil {
		t.Fatal("Amara read a message sent after her removal")
	}
	if !bytes.Equal(must[[]byte](t)(kg.ExportSecret("x", nil, 32)), must[[]byte](t)(rg.ExportSecret("x", nil, 32))) {
		t.Fatal("exporter secrets differ after the removal")
	}
}

func TestChangesAndRevert(t *testing.T) {
	kat, ravi := newDevice(t, "kat"), newDevice(t, "ravi")
	id := groupID()
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: id, Leaf: kat.leaf(), Config: chat}))
	res := must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Adds: [][]byte{ravi.keyPackage(false).Raw}}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	rg := ravi.join(res.Welcome)

	// Ravi keeps his store as entries, the way a database would.
	store := map[string][]byte{}
	save := func() {
		for _, ch := range must[[]Change](t)(ravi.c.TakeChanges()) {
			if ch.Deleted {
				delete(store, string(ch.Key))
			} else {
				store[string(ch.Key)] = ch.Value
			}
		}
	}
	save()

	// Ravi decrypts a commit, doesn't accept it, and reverts, so he can
	// process it later (§11.9).
	res = must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{UpdatePath: true}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	if p := must[*Processed](t)(rg.Process(res.Commit)); p.Kind != KindCommit {
		t.Fatal(p.Kind)
	}
	if _, err := rg.Process(res.Commit); !errors.Is(err, ErrPendingCommit) {
		t.Fatalf("a second message during a staged commit gave %v", err)
	}
	if err := ravi.c.Revert(); err != nil {
		t.Fatal(err)
	}
	if e := must[uint64](t)(rg.Epoch()); e != 1 {
		t.Fatalf("epoch %d after the revert", e)
	}

	// Reopened from the saved entries, Ravi processes it after all.
	ravi.c.Close()
	ravi.c = must[*Client](t)(OpenEntries(store))
	rg = ravi.c.Group(id)
	must[*Processed](t)(rg.Process(res.Commit))
	must[struct{}](t)(struct{}{}, rg.MergeStagedCommit())
	if !bytes.Equal(must[[]byte](t)(kg.ExportSecret("x", nil, 32)), must[[]byte](t)(rg.ExportSecret("x", nil, 32))) {
		t.Fatal("exporter secrets differ")
	}
	save()
	if len(must[[]Change](t)(ravi.c.TakeChanges())) != 0 {
		t.Fatal("changes after saving everything")
	}
	snap := must[[]byte](t)(ravi.c.Snapshot())
	if !bytes.Equal(snap, EncodeSnapshot(store)) {
		t.Fatal("the saved entries differ from the snapshot")
	}
}

func TestLeaveAndEpochs(t *testing.T) {
	kat, ravi := newDevice(t, "kat"), newDevice(t, "ravi")
	id := groupID()
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: id, Leaf: kat.leaf(), Config: chat}))
	res := must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Adds: [][]byte{ravi.keyPackage(false).Raw}}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	rg := ravi.join(res.Welcome)

	// A message from a later epoch waits.
	res = must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{UpdatePath: true}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	msg := must[[]byte](t)(kg.Encrypt(kat.s, []byte("epoch 2"), nil))
	if _, err := rg.Process(msg); !errors.Is(err, ErrWrongEpoch) {
		t.Fatalf("got %v", err)
	}
	must[*Processed](t)(rg.Process(res.Commit))
	must[struct{}](t)(struct{}{}, rg.MergeStagedCommit())
	if p := must[*Processed](t)(rg.Process(msg)); string(p.Plaintext) != "epoch 2" {
		t.Fatal("can't read after catching up")
	}

	// Ravi leaves; Kat commits the proposal by reference (§11.1).
	prop := must[*ProposalResult](t)(rg.Leave(ravi.s))
	p := must[*Processed](t)(kg.Process(prop.Message))
	if p.Kind != KindProposal || p.Proposal.Kind != "remove" || *p.Proposal.Removed != 1 || !bytes.Equal(p.Proposal.Reference, prop.Reference) {
		t.Fatalf("proposal: %+v", p.Proposal)
	}
	if _, err := kg.Encrypt(kat.s, []byte("x"), nil); !errors.Is(err, ErrPendingProposal) {
		t.Fatalf("got %v", err)
	}
	res = must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{PendingProposals: true}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	p = must[*Processed](t)(rg.Process(res.Commit))
	if !p.Commit.SelfRemoved || !p.Commit.Proposals[0].ByReference {
		t.Fatalf("commit: %+v", p.Commit)
	}
	if n := len(must[*State](t)(kg.State()).Members); n != 1 {
		t.Fatalf("%d members", n)
	}

	// A stale-epoch commit is cleared.
	amara := newDevice(t, "amara")
	must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Adds: [][]byte{amara.keyPackage(false).Raw}}))
	if !must[*State](t)(kg.State()).PendingCommit {
		t.Fatal("no pending commit")
	}
	if _, err := kg.Commit(kat.s, CommitOptions{UpdatePath: true}); !errors.Is(err, ErrPendingCommit) {
		t.Fatalf("got %v", err)
	}
	must[struct{}](t)(struct{}{}, kg.ClearPendingCommit())
	if must[*State](t)(kg.State()).PendingCommit {
		t.Fatal("the pending commit is still there")
	}
	must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{UpdatePath: true}))
}

func TestExternalJoin(t *testing.T) {
	community := Config{AcceptPublicMessages: true, MaxPastEpochs: 2}
	kat, ravi := newDevice(t, "kat"), newDevice(t, "ravi")
	id := groupID()
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: id, Leaf: kat.leaf(), Config: community}))
	gi := must[[]byte](t)(kg.ExportGroupInfo(kat.s, false))
	tree := must[[]byte](t)(kg.ExportRatchetTree())
	sum := must[*GroupInfoSummary](t)(InspectGroupInfo(gi))
	if sum.Epoch != 0 || !bytes.Equal(sum.GroupID, id) || sum.Ciphersuite != Ciphersuite {
		t.Fatalf("summary: %+v", sum)
	}
	if _, ok := findExtension(sum.GroupInfoExtensions, ExtExternalPub); !ok {
		t.Fatal("no external_pub")
	}

	rg, res, err := ravi.c.JoinExternal(ravi.s, ExternalJoinOptions{GroupInfo: gi, RatchetTree: tree, Leaf: ravi.leaf(), Config: community, WantGroupInfo: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.GroupInfo == nil || res.Epoch != 0 {
		t.Fatalf("result: %+v", res)
	}
	p := must[*Processed](t)(kg.Process(res.Commit))
	if p.Sender.Kind != "new_member_commit" || string(p.Identity) != "ravi" {
		t.Fatalf("processed: %+v", p)
	}
	must[struct{}](t)(struct{}{}, kg.MergeStagedCommit())
	msg := must[[]byte](t)(rg.Encrypt(ravi.s, []byte("joined"), nil))
	if p := must[*Processed](t)(kg.Process(msg)); string(p.Plaintext) != "joined" {
		t.Fatal("can't read the joiner")
	}
}

func TestNewSigner(t *testing.T) {
	kat, ravi := newDevice(t, "kat"), newDevice(t, "ravi")
	id := groupID()
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: id, Leaf: kat.leaf(), Config: chat}))
	res := must[*CommitResult](t)(kg.Commit(kat.s, CommitOptions{Adds: [][]byte{ravi.keyPackage(false).Raw}}))
	must[struct{}](t)(struct{}{}, kg.MergePendingCommit())
	rg := ravi.join(res.Welcome)

	// Ravi moves to a new message key with an update path (§3.2.3).
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ns := must[*Signer](t)(NewSigner(priv))
	leaf := ravi.leaf()
	leaf.Extensions = []Extension{{Type: extMsgKey, Data: []byte("msg-key 2")}}
	res = must[*CommitResult](t)(rg.Commit(ravi.s, CommitOptions{Leaf: &leaf, NewSigner: ns}))
	must[struct{}](t)(struct{}{}, rg.MergePendingCommit())
	p := must[*Processed](t)(kg.Process(res.Commit))
	if !bytes.Equal(p.Commit.PathLeaf.SignatureKey, ns.PublicKey()) {
		t.Fatal("the new signature key isn't in the path")
	}
	must[struct{}](t)(struct{}{}, kg.MergeStagedCommit())
	msg := must[[]byte](t)(rg.Encrypt(ns, []byte("new key"), nil))
	if p := must[*Processed](t)(kg.Process(msg)); string(p.Plaintext) != "new key" {
		t.Fatal("can't read the new key's message")
	}
}

func TestErrors(t *testing.T) {
	kat := newDevice(t, "kat")
	if _, err := kat.c.Group([]byte("missing")).State(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := NewSigner([]byte("short")); !IsCode(err, ErrBadInput) {
		t.Fatalf("got %v", err)
	}
	if _, err := Open([]byte("not a snapshot")); !IsCode(err, ErrStorage) {
		t.Fatalf("got %v", err)
	}
	kat.c.Close()
	if _, err := kat.c.Snapshot(); err == nil {
		t.Fatal("a closed client worked")
	}
}

// TestSetConfig changes a group's padding after it was made: messages
// padded to 256 bytes come out shorter once the group pads nothing.
func TestSetConfig(t *testing.T) {
	kat := newDevice(t, "kat")
	padded := chat
	padded.Padding = 256
	kg := must[*Group](t)(kat.c.CreateGroup(kat.s, CreateOptions{GroupID: groupID(), Leaf: kat.leaf(), Config: padded}))
	before := must[[]byte](t)(kg.Encrypt(kat.s, []byte("ok"), nil))
	if err := kg.SetConfig(chat); err != nil {
		t.Fatal(err)
	}
	after := must[[]byte](t)(kg.Encrypt(kat.s, []byte("ok"), nil))
	if len(after)+100 > len(before) {
		t.Fatalf("%d bytes with padding, %d without", len(before), len(after))
	}
	if err := (&Group{c: kat.c, id: groupID()}).SetConfig(chat); err == nil {
		t.Error("set the configuration of a group that isn't there")
	}
}

// TestRequiredExtensions reads back what RequiredCapabilities writes.
func TestRequiredExtensions(t *testing.T) {
	want := []uint16{0xF0A1, 0xF0A3, 0xF0A5}
	got := RequiredExtensions(RequiredCapabilities(want, nil, nil).Data)
	if len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Fatalf("got %x", got)
	}
	if RequiredExtensions([]byte{0x05}) != nil {
		t.Error("an odd length parsed")
	}
}
