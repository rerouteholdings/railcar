package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/rerouteholdings/railcar/internal/mls"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/codes"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// communityExt is the tieroute_group of a community group, as Reroute
// writes it: kind group, the community server as hub, and no admins or
// since, which the community chain and the community server's roster
// replace (§16.5).
func communityExt(server string) *GroupExt {
	return &GroupExt{Kind: KindGroup, Hub: server, Admins: []string{}, Since: map[string]uint64{}}
}

// newCommunityGroup creates a community MLS group, and returns it with
// its registration: epoch 0's key, GroupInfo, and uploaded tree.
func (e *Engine) newCommunityGroup(ctx context.Context, conn *client.Community, exts []mls.Extension) (*mls.Group, client.GroupReg, error) {
	gid := make([]byte, 32)
	rand.Read(gid)
	mg, err := e.dev.mls.CreateGroup(e.dev.signer, mls.CreateOptions{GroupID: gid, Leaf: e.dev.communityLeaf(), Config: communityConfig, Extensions: exts})
	if err != nil {
		return nil, client.GroupReg{}, err
	}
	fail := func(err error) (*mls.Group, client.GroupReg, error) {
		mg.Delete()
		e.dev.Save()
		return nil, client.GroupReg{}, err
	}
	ek, err := e.dev.EpochKey(mg)
	if err != nil {
		return fail(err)
	}
	info, err := mg.ExportGroupInfo(e.dev.signer, false)
	if err != nil {
		return fail(err)
	}
	tree, err := mg.ExportRatchetTree()
	if err != nil {
		return fail(err)
	}
	th, err := conn.UploadTree(ctx, tree)
	if err != nil {
		return fail(err)
	}
	if err := e.dev.Save(); err != nil {
		return fail(err)
	}
	return mg, client.GroupReg{Group: gid, Epoch: 0, EpochKey: ek.Public(), Info: info, Tree: th}, nil
}

// CreateCommunity creates a community on a community server (§16.2.1,
// community server API "Create or move in"): the create entry, signed by
// this device, and the members group at epoch 0. creationCode is the
// operator's creation code, if the server needs one. It then adds a
// #general text channel that keeps history.
func (e *Engine) CreateCommunity(ctx context.Context, server, name, desc, creationCode string) (string, error) {
	name = strings.TrimSpace(name)
	if err := community.CheckName(name); err != nil {
		return "", fmt.Errorf("chat: a community's name: %w", err)
	}
	e.mu.Lock()
	if cm := e.st.Communities[server]; cm != nil && !cm.Left {
		e.mu.Unlock()
		return "", errors.New("chat: you're already in that server's community")
	}
	conn := e.c.Community(server)
	if err := conn.SignIn(ctx); err != nil {
		e.mu.Unlock()
		return "", err
	}
	op := map[string]any{"op": community.OpCreate, "server": server, "name": name, "public": false, "approval": false,
		"everyone": []string{community.PermTalk}} // talk alone by default (§16.2.3)
	if desc = strings.TrimSpace(desc); desc != "" {
		op["desc"] = desc
	}
	entry, err := e.c.SignAsDevice(community.TypeEntry, map[string]any{"seq": 0, "ts": e.c.Clock().Now().Unix(), "by": e.c.ID(), "ops": []any{op}})
	if err != nil {
		e.mu.Unlock()
		return "", err
	}
	id := entry.Hash()
	exts := []mls.Extension{mustExt(communityExt(server)), mls.RequiredCapabilities([]uint16{ExtGroup, ExtCommunityHead}, nil, nil), {Type: ExtCommunityHead, Data: id[:]}}
	mg, reg, err := e.newCommunityGroup(ctx, conn, exts)
	if err != nil {
		e.mu.Unlock()
		return "", err
	}
	got, err := conn.Create(ctx, entry, reg, creationCode)
	if err != nil {
		mg.Delete()
		e.dev.Save()
		e.mu.Unlock()
		return "", err
	}
	if !bytes.Equal(got, id[:]) {
		e.mu.Unlock()
		return "", errors.New("chat: the community server named the community by another identifier")
	}
	meta := make([]byte, 32)
	rand.Read(meta)
	gid := wire.EncodeB64(reg.Group)
	cm := &Community{Server: server, ID: id[:], MembersGroup: gid, MetaKeys: []wire.Bytes{meta},
		Groups: map[string]*CGroup{gid: {ID: gid, Epoch: 0, Members: []string{e.c.ID()}}}}
	e.st.Communities[server] = cm
	if err := e.profile().Save(chainRecord(cm.ID), []wire.Signed{entry}); err != nil {
		e.mu.Unlock()
		return "", err
	}
	delete(e.cchains, server)
	if _, err := e.communityChain(cm); err != nil {
		e.mu.Unlock()
		return "", err
	}
	e.save()
	if err := e.topUpCommunity(ctx, cm); err != nil {
		e.log.Info("chat: community key packages", "err", err)
	}
	e.mu.Unlock()
	if _, err := e.AddChannel(ctx, server, "general", "", ChannelOptions{}); err != nil {
		e.log.Info("chat: the new community's #general", "err", err)
	}
	e.startCommunity(e.runCtx, server)
	return server, nil
}

func mustExt(g *GroupExt) mls.Extension {
	x, _ := g.Extension()
	return x
}

// JoinCommunity joins a community with a code, SERVER::SECRET (§16.6): it
// checks the chain and the code, builds an external commit to the members
// group from the GroupInfo and tree the community server serves, signs the
// join proof and the commit-request with the code's key, and submits them.
// Names and history arrive once a member sends the keys.
func (e *Engine) JoinCommunity(ctx context.Context, code string) (string, error) {
	server, secret, err := codes.ParseJoinCode(strings.TrimSpace(code))
	if err != nil {
		return "", err
	}
	conn := e.c.Community(server)
	preview, err := conn.Preview(ctx)
	if err != nil {
		return "", err
	}
	if len(preview.ID) != 32 || len(preview.Group) == 0 {
		return "", errors.New("chat: the community server's preview has no community or members group")
	}
	key, err := codes.CodeKey(secret, preview.ID)
	if err != nil {
		return "", err
	}
	hash := community.CodeHash(key.Public())
	e.mu.Lock()
	defer e.mu.Unlock()
	if cm := e.st.Communities[server]; cm != nil && !cm.Left {
		if cm.Asking {
			return server, ErrAskedToJoin
		}
		return server, nil
	}
	if err := conn.SignIn(ctx); err != nil {
		return "", err
	}
	e.joinCode[server] = hash
	defer delete(e.joinCode, server)
	cm := &Community{Server: server, ID: preview.ID, MembersGroup: wire.EncodeB64(preview.Group), Groups: map[string]*CGroup{}}
	delete(e.cchains, server)
	e.profile().Delete(chainRecord(cm.ID))
	if _, err := e.syncCommunityChain(ctx, cm, hash); err != nil {
		return "", fmt.Errorf("chat: the community's chain: %w", err)
	}
	chain, err := e.communityChain(cm)
	if err != nil {
		return "", err
	}
	st := chain.State()
	if !bytes.Equal(chain.ID(), preview.ID) {
		return "", errors.New("chat: the chain is another community's")
	}
	if !st.CodeLive(key.Public(), e.c.Clock().Now().Unix(), 0) {
		return "", errors.New("chat: that code doesn't work anymore")
	}
	if st.Banned(e.c.ID(), e.c.Server()) {
		return "", errors.New("chat: you can't join this community")
	}
	if st.Approval {
		return server, e.askToJoin(ctx, conn, cm)
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		err := e.joinOnce(ctx, conn, cm, chain, key, hash, preview.Group)
		if err == nil {
			e.st.Communities[server] = cm
			e.save()
			if err := e.topUpCommunity(ctx, cm); err != nil {
				e.log.Info("chat: community key packages", "err", err)
			}
			e.refreshChannels(cm)
			go e.startCommunity(e.runCtx, server)
			return server, nil
		}
		lastErr = err
		if !wire.IsCode(err, wire.CodeStaleEpoch) && !errors.Is(err, errBadGroupInfo) {
			break
		}
	}
	return "", lastErr
}

// errBadGroupInfo marks a GroupInfo that failed the joiner's checks; the
// joiner fetches again (§16.6).
var errBadGroupInfo = errors.New("chat: the community server served a GroupInfo that didn't check out")

func (e *Engine) joinOnce(ctx context.Context, conn *client.Community, cm *Community, chain *community.Chain, key *sig.CompositeKey, hash, group []byte) error {
	gi, err := conn.GroupInfo(ctx, group, hash)
	if err != nil {
		return err
	}
	sum, err := mls.InspectGroupInfo(gi.Info)
	if err != nil || sum.Epoch != gi.Epoch || !bytes.Equal(sum.GroupID, group) {
		return errBadGroupInfo
	}
	head, ok := communityHead(sum.Extensions)
	if !ok {
		return errBadGroupInfo
	}
	if _, ok := chain.Find(head); !ok {
		if _, err := e.syncCommunityChain(ctx, cm, hash); err != nil {
			return err
		}
		if chain, err = e.communityChain(cm); err != nil {
			return err
		}
		if _, ok := chain.Find(head); !ok {
			return errBadGroupInfo
		}
	}
	tree, err := conn.Tree(ctx, group, gi.Epoch, hash)
	if err != nil {
		return err
	}
	if th := sha256.Sum256(tree); !bytes.Equal(th[:], gi.Tree) {
		return errBadGroupInfo
	}
	// OpenMLS checks the GroupInfo's signature and the tree against its
	// tree_hash while it builds the commit.
	mg, res, err := e.dev.mls.JoinExternal(e.dev.signer, mls.ExternalJoinOptions{
		GroupInfo: gi.Info, RatchetTree: tree, Leaf: e.dev.communityLeaf(), Config: communityConfig, WantGroupInfo: true,
	})
	if err != nil {
		e.dev.Revert()
		return fmt.Errorf("%w: %v", errBadGroupInfo, err)
	}
	fail := func(err error) error {
		mg.Delete()
		e.dev.Revert()
		return err
	}
	next, err := e.dev.EpochKey(mg)
	if err != nil {
		return fail(err)
	}
	ch := sha256.Sum256(res.Commit)
	req, err := sig.SignRevealing(key, TypeCommitRequest, CommitRequest{Group: group, Epoch: gi.Epoch, Hash: ch[:], Next: next.Public()})
	if err != nil {
		return fail(err)
	}
	proof, err := community.SignJoinProof(key, community.JoinProof{Commit: ch[:], ID: e.c.ID(), Epoch: gi.Epoch})
	if err != nil {
		return fail(err)
	}
	newTree, err := mg.ExportRatchetTree()
	if err != nil {
		return fail(err)
	}
	th, err := conn.UploadTree(ctx, newTree)
	if err != nil {
		return fail(err)
	}
	receipt, err := conn.Join(ctx, hash, client.JoinReq{Group: group, Epoch: gi.Epoch, Data: res.Commit, Commit: req, Proof: proof, Info: res.GroupInfo, Tree: th})
	if err != nil {
		return fail(err)
	}
	if _, err := e.checkReceiptFrom(ctx, cm.Server, receipt, group, gi.Epoch, res.Commit); err != nil {
		return fail(err)
	}
	if err := e.dev.Save(); err != nil {
		return err
	}
	st, err := mg.State()
	if err != nil {
		return err
	}
	gid := wire.EncodeB64(group)
	cm.Groups[gid] = &CGroup{ID: gid, Epoch: st.Epoch, Members: members(st), Applied: map[uint64]wire.Bytes{gi.Epoch: ch[:]}}
	if cm.Uses == nil {
		cm.Uses = map[string]uint64{}
	}
	cm.Uses[fmt.Sprintf("%x", hash)]++
	return nil
}

// newJoinSecret makes a code's 16 random characters from the base64url
// alphabet: 96 bits (§16.6).
func newJoinSecret() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
