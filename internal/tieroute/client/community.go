package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/discovery"
	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Community is this device's connection to one community server (§16.1):
// it signs in there with the device's key, and calls the community server
// API. Devices connect to community servers directly, not through their
// homeservers. Its methods are safe for concurrent use.
type Community struct {
	c      *Client
	server string
	clock  *Clock

	mu      sync.Mutex
	api     *api
	tok     string
	member  bool
	cursorV string
	signIn  sync.Mutex
}

// Community returns the connection to a community server. It doesn't
// contact the server until it's used.
func (c *Client) Community(server string) *Community {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.communities == nil {
		c.communities = map[string]*Community{}
	}
	if cm := c.communities[server]; cm != nil {
		return cm
	}
	cm := &Community{c: c, server: server, clock: &Clock{}}
	c.communities[server] = cm
	return cm
}

// Server returns the community server's name.
func (cm *Community) Server() string { return cm.server }

// connect finds the community server's API base (community server API,
// "The community API base").
func (cm *Community) connect(ctx context.Context) error {
	cm.mu.Lock()
	ready := cm.api != nil
	cm.mu.Unlock()
	if ready {
		return nil
	}
	// The device connects to the community server directly, so it
	// fetches the document there too (§16.1).
	cm.c.resolver.Direct(cm.server)
	doc, err := cm.c.resolver.ServerDoc(ctx, cm.server)
	if err != nil {
		return err
	}
	if !doc.HasRole(discovery.RoleCommunity) {
		return fmt.Errorf("client: %s isn't a community server", cm.server)
	}
	a, ok := doc.API(wire.Version)
	if !ok || a.Community == "" {
		return fmt.Errorf("client: %s lists no community API for wire version 2", cm.server)
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.api == nil {
		cm.api = &api{base: a.Community, http: cm.c.opts.httpClient(), clock: cm.clock, log: cm.c.opts.Logger, token: cm.token}
	}
	return nil
}

func (cm *Community) token() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.tok
}

// Member reports whether the community server counted this identity as a
// member at the last sign-in.
func (cm *Community) Member() bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.member
}

type communityChallengeReq struct {
	ID   string     `json:"id"`
	Key  wire.Bytes `json:"key"`
	Home string     `json:"home"`
}

type communitySessionRes struct {
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
	Member  bool   `json:"member"`
}

// SignIn signs this device in to the community server: a challenge naming
// the identity's homeserver, so the server can fetch the chain, then the
// signed nonce (§16.6 step 3).
func (cm *Community) SignIn(ctx context.Context) error {
	cm.signIn.Lock()
	defer cm.signIn.Unlock()
	if err := cm.connect(ctx); err != nil {
		return err
	}
	a := *cm.api
	a.token = nil
	id := cm.c.ID()
	home := cm.c.Chain().State.Home
	for attempt := 0; ; attempt++ {
		var ch challengeRes
		start := time.Now()
		err := a.call(ctx, http.MethodPost, "auth/challenge", nil, communityChallengeReq{ID: id, Key: cm.c.DeviceKey(), Home: home}, &ch)
		end := time.Now()
		if err != nil {
			return err
		}
		if ch.Now > 0 {
			cm.clock.observe(time.Unix(ch.Now, 0), start, end)
		}
		auth, err := sig.SignDevice(cm.c.device.Key, identity.TypeAuth, AuthBody{
			ID: id, Nonce: ch.Nonce, Server: cm.server, TS: cm.clock.Now().Unix(), Name: cm.c.device.Name,
		})
		if err != nil {
			return err
		}
		var res communitySessionRes
		err = a.call(ctx, http.MethodPost, "auth/session", nil, sessionReq{Auth: auth}, &res)
		if wire.IsCode(err, wire.CodeClockSkew) && attempt == 0 {
			continue
		}
		if err != nil {
			return err
		}
		cm.mu.Lock()
		cm.tok, cm.member = res.Token, res.Member
		cm.mu.Unlock()
		return nil
	}
}

// authed makes a call that needs a session, signing in first or again
// after 401, as the homeserver client does.
func (cm *Community) authed(ctx context.Context, method, path string, query url.Values, in, out any) error {
	if err := cm.connect(ctx); err != nil {
		return err
	}
	if cm.token() == "" {
		if err := cm.SignIn(ctx); err != nil {
			return err
		}
	}
	err := cm.api.call(ctx, method, path, query, in, out)
	if wire.IsCode(err, wire.CodeUnauthenticated) {
		if err := cm.SignIn(ctx); err != nil {
			return err
		}
		err = cm.api.call(ctx, method, path, query, in, out)
	}
	return err
}

// rawCall sends or fetches bytes, as tree uploads and downloads do.
func (cm *Community) rawCall(ctx context.Context, method, path string, query url.Values, body []byte, limit int64) ([]byte, error) {
	if err := cm.connect(ctx); err != nil {
		return nil, err
	}
	if cm.token() == "" {
		if err := cm.SignIn(ctx); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, cm.api.url(path, query), rd)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		req.Header.Set("Authorization", "Bearer "+cm.token())
		resp, err := cm.api.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("client: %s %s: %w", method, path, err)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			if err := cm.SignIn(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := &wire.Error{Status: resp.StatusCode}
			if err := wire.Unmarshal(data, e); err != nil || e.Code == "" {
				e.Code = statusCode(resp.StatusCode)
			}
			return nil, e
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("client: %s %s: response too large", method, path)
		}
		return data, nil
	}
}

func codeQuery(code []byte, q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if len(code) > 0 {
		q.Set("code", wire.EncodeB64(code))
	}
	return q
}

// Preview is a community's join preview (GET preview).
type Preview struct {
	ID       wire.Bytes `json:"id"`
	Group    wire.Bytes `json:"group,omitempty"`
	Head     wire.Bytes `json:"head"`
	Name     string     `json:"name"`
	Desc     string     `json:"desc,omitempty"`
	Settings struct {
		PublicCodes bool `json:"public-codes"`
		Approval    bool `json:"approval"`
	} `json:"settings"`
}

// Preview fetches the join preview. It needs no session. Until the chain
// is checked, what it says is the server's claim.
func (cm *Community) Preview(ctx context.Context) (*Preview, error) {
	if err := cm.connect(ctx); err != nil {
		return nil, err
	}
	a := *cm.api
	a.token = nil
	var p Preview
	if err := a.call(ctx, http.MethodGet, "preview", nil, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// GroupReg registers a group with the community server as hub (POST
// create, POST groups/create).
type GroupReg struct {
	Group    wire.Bytes `json:"group"`
	Epoch    uint64     `json:"epoch"`
	EpochKey wire.Bytes `json:"epoch-key"`
	Info     wire.Bytes `json:"info"`
	Tree     wire.Bytes `json:"tree"`
}

// Create creates the community on this server from its create entry,
// with its members group at epoch 0 (POST create). code is the operator's
// creation code, if the server needs one.
func (cm *Community) Create(ctx context.Context, entry wire.Signed, group GroupReg, code string) ([]byte, error) {
	req := map[string]any{"entry": entry, "group": group}
	if code != "" {
		req["code"] = code
	}
	var res struct {
		ID wire.Bytes `json:"id"`
	}
	if err := cm.authed(ctx, http.MethodPost, "create", nil, req, &res); err != nil {
		return nil, err
	}
	cm.mu.Lock()
	cm.member = true
	cm.mu.Unlock()
	return res.ID, nil
}

// ChainPage is the community chain's entries after a seq (GET chain).
type ChainPage struct {
	Entries []wire.Signed `json:"entries"`
	Head    uint64        `json:"head"`
}

// Chain fetches the community chain, every entry or those after after.
// code is the hash of a live code's key, for a joiner who isn't a member
// yet (community server API, "Code access").
func (cm *Community) Chain(ctx context.Context, after *uint64, code []byte) (*ChainPage, error) {
	q := codeQuery(code, nil)
	if after != nil {
		q.Set("after", strconv.FormatUint(*after, 10))
	}
	var p ChainPage
	err := cm.authed(ctx, http.MethodGet, "chain", q, nil, &p)
	return &p, err
}

// AppendResult is what POST chain/append answers.
type AppendResult struct {
	Seq  uint64       `json:"seq"`
	Head wire.Bytes   `json:"head"`
	Seqs []ChannelSeq `json:"seqs,omitempty"`
}

// ChannelSeq is a text channel's last sequence number.
type ChannelSeq struct {
	Channel uint64 `json:"channel"`
	Seq     uint64 `json:"seq"`
}

// Append appends a signed community entry (POST chain/append).
func (cm *Community) Append(ctx context.Context, entry wire.Signed) (*AppendResult, error) {
	var r AppendResult
	err := cm.authed(ctx, http.MethodPost, "chain/append", nil, map[string]any{"entry": entry}, &r)
	return &r, err
}

// AppendSetHome appends a set-home naming another community server, with
// that server's reservation (POST chain/append, §16.6). The answer holds
// each text channel's last sequence number.
func (cm *Community) AppendSetHome(ctx context.Context, entry, reservation wire.Signed) (*AppendResult, error) {
	var r AppendResult
	err := cm.authed(ctx, http.MethodPost, "chain/append", nil, map[string]any{"entry": entry, "reservation": reservation}, &r)
	return &r, err
}

// Move describes a community moving to this server (POST create with
// chain, §16.6): the whole chain, ending in the set-home that names this
// server, the homes of its signers, and the owner's signed member list.
type Move struct {
	Chain   []wire.Signed
	Homes   map[string]string
	Members wire.Signed
	Seqs    []ChannelSeq // from the set-home's append; none for a check
	Code    string       // the operator's creation code, if the server needs one
}

func (m Move) req() map[string]any {
	req := map[string]any{"chain": m.Chain, "members": m.Members}
	if len(m.Homes) > 0 {
		req["homes"] = m.Homes
	}
	if len(m.Seqs) > 0 {
		req["seqs"] = m.Seqs
	}
	if m.Code != "" {
		req["code"] = m.Code
	}
	return req
}

// CheckMove asks this server whether it would take a community moving in,
// and returns its signed reservation (POST create with check, §16.6).
// It stores nothing but a hold.
func (cm *Community) CheckMove(ctx context.Context, m Move) (wire.Signed, error) {
	req := m.req()
	delete(req, "seqs")
	req["check"] = true
	var res struct {
		Reservation *wire.Signed `json:"reservation"`
	}
	if err := cm.authed(ctx, http.MethodPost, "create", nil, req, &res); err != nil {
		return wire.Signed{}, err
	}
	if res.Reservation == nil {
		return wire.Signed{}, errors.New("client: the community server checked the move without a reservation")
	}
	return *res.Reservation, nil
}

// MoveIn moves a community to this server (POST create with chain). The
// owner then hands each group's hub over (§11.11).
func (cm *Community) MoveIn(ctx context.Context, m Move) error {
	if err := cm.authed(ctx, http.MethodPost, "create", nil, m.req(), nil); err != nil {
		return err
	}
	cm.mu.Lock()
	cm.member = true
	cm.mu.Unlock()
	return nil
}

// MemberChain fetches a member's or former member's identity chain as the
// community server last fetched it (GET members/{id}/chain). The caller
// validates it, as any chain.
func (cm *Community) MemberChain(ctx context.Context, id string, code []byte) ([]wire.Signed, error) {
	var res ChainPage
	err := cm.authed(ctx, http.MethodGet, "members/"+id+"/chain", codeQuery(code, nil), nil, &res)
	return res.Entries, err
}

// MemberHome is one member on the server's list (GET members).
type MemberHome struct {
	ID   string `json:"id"`
	Home string `json:"home"`
}

// Members lists the server's members. It needs administrate.
func (cm *Community) Members(ctx context.Context) ([]MemberHome, error) {
	var res struct {
		Items []MemberHome `json:"items"`
	}
	err := cm.authed(ctx, http.MethodGet, "members", nil, nil, &res)
	return res.Items, err
}

// UploadKeyPackages makes key packages, binds each with a kp-binding, and
// uploads them to the community's pool (POST keypackages/upload).
func (cm *Community) UploadKeyPackages(ctx context.Context, maker KeyPackageMaker, n int, opts UploadOptions) (int, error) {
	id := cm.c.ID()
	bind := func(kp []byte) (KPWithBinding, error) {
		b, err := cm.c.device.KPBinding(id, kp)
		return KPWithBinding{KP: kp, Binding: b}, err
	}
	req := kpUploadReq{Packages: []KPWithBinding{}, Replace: opts.Replace}
	if n > 0 {
		kps, err := maker.KeyPackages(n, false)
		if err != nil {
			return 0, err
		}
		for _, kp := range kps {
			b, err := bind(kp)
			if err != nil {
				return 0, err
			}
			req.Packages = append(req.Packages, b)
		}
	}
	if opts.LastResort {
		kps, err := maker.KeyPackages(1, true)
		if err != nil {
			return 0, err
		}
		b, err := bind(kps[0])
		if err != nil {
			return 0, err
		}
		req.LastResort = &b
	}
	var res struct {
		Count int `json:"count"`
	}
	err := cm.authed(ctx, http.MethodPost, "keypackages/upload", nil, req, &res)
	return res.Count, err
}

// KeyPackageCount returns how many of this device's key packages are left
// in the pool, and whether it has a last-resort one.
func (cm *Community) KeyPackageCount(ctx context.Context) (int, bool, error) {
	var res struct {
		Count      int  `json:"count"`
		LastResort bool `json:"last-resort"`
	}
	err := cm.authed(ctx, http.MethodGet, "keypackages/count", nil, nil, &res)
	return res.Count, res.LastResort, err
}

// ClaimKeyPackages claims one key package per current device of a member,
// or of only some devices (POST keypackages/claim).
func (cm *Community) ClaimKeyPackages(ctx context.Context, id string, devices [][]byte) ([]ClaimedKP, error) {
	req := map[string]any{"id": id}
	if len(devices) > 0 {
		ds := make([]wire.Bytes, len(devices))
		for i, d := range devices {
			ds[i] = d
		}
		req["devices"] = ds
	}
	var res struct {
		Packages []ClaimedKP `json:"packages"`
	}
	err := cm.authed(ctx, http.MethodPost, "keypackages/claim", nil, req, &res)
	return res.Packages, err
}

// CreateGroup registers a restricted channel's group, with its see list,
// or a group that replaces one (POST groups/create).
func (cm *Community) CreateGroup(ctx context.Context, reg GroupReg, see []uint64, replaces []byte) error {
	req := map[string]any{"group": reg.Group, "epoch": reg.Epoch, "epoch-key": reg.EpochKey, "info": reg.Info, "tree": reg.Tree}
	if see != nil {
		req["see"] = see
	}
	if len(replaces) > 0 {
		req["replaces"] = wire.Bytes(replaces)
	}
	return cm.authed(ctx, http.MethodPost, "groups/create", nil, req, nil)
}

// RegisterGroup and UnregisterGroup start and stop delivery of a group's
// traffic to this device.
func (cm *Community) RegisterGroup(ctx context.Context, group []byte) error {
	return cm.authed(ctx, http.MethodPost, "groups/register", nil, map[string]any{"group": wire.Bytes(group)}, nil)
}

func (cm *Community) UnregisterGroup(ctx context.Context, group []byte) error {
	return cm.authed(ctx, http.MethodPost, "groups/unregister", nil, map[string]any{"group": wire.Bytes(group)}, nil)
}

// CommunityCommit is a commit the community server keeps, with its join
// proof for an external commit.
type CommunityCommit struct {
	Epoch   uint64       `json:"epoch"`
	Data    wire.Bytes   `json:"data"`
	Receipt wire.Signed  `json:"receipt"`
	Proof   *wire.Signed `json:"proof,omitempty"`
}

// Commits fetches a group's commits made after epoch after.
func (cm *Community) Commits(ctx context.Context, group []byte, after uint64) ([]CommunityCommit, error) {
	var res struct {
		Items []CommunityCommit `json:"items"`
	}
	q := url.Values{"after": {strconv.FormatUint(after, 10)}}
	err := cm.authed(ctx, http.MethodGet, "groups/"+wire.EncodeB64(group)+"/commits", q, nil, &res)
	return res.Items, err
}

// Undos fetches the commit-undos the community server holds for a group's
// epochs after after (GET groups/{group}/commits, undos).
func (cm *Community) Undos(ctx context.Context, group []byte, after uint64) ([]wire.Signed, error) {
	var res struct {
		Undos []wire.Signed `json:"undos"`
	}
	q := url.Values{"after": {strconv.FormatUint(after, 10)}}
	err := cm.authed(ctx, http.MethodGet, "groups/"+wire.EncodeB64(group)+"/commits", q, nil, &res)
	return res.Undos, err
}

// Undo asks the community server to undo a commit every member refused,
// with a signed commit-undo (POST groups/undo, §16.5). A server written
// before undoing answers 404.
func (cm *Community) Undo(ctx context.Context, undo wire.Signed) error {
	return cm.authed(ctx, http.MethodPost, "groups/undo", nil, map[string]any{"undo": undo}, nil)
}

// GroupInfoRes is a group's current GroupInfo, its epoch, and its tree's
// hash and size (GET groups/{group}/info).
type GroupInfoRes struct {
	Epoch uint64     `json:"epoch"`
	Info  wire.Bytes `json:"info"`
	Tree  wire.Bytes `json:"tree"`
	Size  uint64     `json:"size"`
}

// GroupInfo fetches a group's current GroupInfo.
func (cm *Community) GroupInfo(ctx context.Context, group, code []byte) (*GroupInfoRes, error) {
	var res GroupInfoRes
	err := cm.authed(ctx, http.MethodGet, "groups/"+wire.EncodeB64(group)+"/info", codeQuery(code, nil), nil, &res)
	return &res, err
}

// maxTree bounds a ratchet tree download: 32 MiB (community server API).
const maxTree = 32 << 20

// Tree downloads a group's ratchet tree for an epoch. The caller checks it
// against the GroupInfo before using it.
func (cm *Community) Tree(ctx context.Context, group []byte, epoch uint64, code []byte) ([]byte, error) {
	q := codeQuery(code, url.Values{"epoch": {strconv.FormatUint(epoch, 10)}})
	return cm.rawCall(ctx, http.MethodGet, "groups/"+wire.EncodeB64(group)+"/tree", q, nil, maxTree)
}

// UploadTree uploads a ratchet tree, and returns its SHA-256, which
// commits and joins name (POST tree/upload).
func (cm *Community) UploadTree(ctx context.Context, tree []byte) ([]byte, error) {
	data, err := cm.rawCall(ctx, http.MethodPost, "tree/upload", nil, tree, 1<<16)
	if err != nil {
		return nil, err
	}
	var res struct {
		Hash wire.Bytes `json:"hash"`
	}
	if err := wire.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	if h := sha256.Sum256(tree); !bytes.Equal(h[:], res.Hash) {
		return nil, errors.New("client: the community server named the tree by another hash")
	}
	return res.Hash, nil
}

// JoinReq is a code join: the external commit, its commit-request and
// join proof signed with the code key, and the new epoch's GroupInfo and
// tree (POST join).
type JoinReq struct {
	Group  wire.Bytes  `json:"group"`
	Epoch  uint64      `json:"epoch"`
	Data   wire.Bytes  `json:"data"`
	Commit wire.Signed `json:"commit"`
	Proof  wire.Signed `json:"proof"`
	Info   wire.Bytes  `json:"info"`
	Tree   wire.Bytes  `json:"tree"`
}

// Join submits a code join, and returns the hub's receipt. code is the
// hash of the code's composite public key.
func (cm *Community) Join(ctx context.Context, code []byte, req JoinReq) (wire.Signed, error) {
	var res struct {
		Receipt wire.Signed `json:"receipt"`
	}
	err := cm.authed(ctx, http.MethodPost, "join", codeQuery(code, nil), req, &res)
	if err == nil {
		cm.mu.Lock()
		cm.member = true
		cm.mu.Unlock()
	}
	return res.Receipt, err
}

// Leave ends this identity's membership (POST leave).
func (cm *Community) Leave(ctx context.Context) error {
	return cm.authed(ctx, http.MethodPost, "leave", nil, struct{}{}, nil)
}

// CommunityOutgoing is one delivery to a community's groups (POST send).
type CommunityOutgoing struct {
	ID        string             `json:"id"`
	Kind      string             `json:"kind"`
	Group     wire.Bytes         `json:"group"`
	Epoch     uint64             `json:"epoch"`
	MLS       string             `json:"mls"`
	Channel   *uint64            `json:"channel,omitempty"`
	Ephemeral bool               `json:"ephemeral,omitempty"`
	Data      wire.Bytes         `json:"data"`
	Commit    *wire.Signed       `json:"commit,omitempty"`
	Info      wire.Bytes         `json:"info,omitempty"`
	Tree      wire.Bytes         `json:"tree,omitempty"`
	Welcomes  []CommunityWelcome `json:"welcomes,omitempty"`
	To        []string           `json:"to,omitempty"`
	Devs      []wire.Bytes       `json:"devs,omitempty"`
}

// CommunityWelcome is a Welcome sent with a commit, delivered only if the
// hub accepts it.
type CommunityWelcome struct {
	ID   string       `json:"id"`
	To   []string     `json:"to"`
	Devs []wire.Bytes `json:"devs"`
	Data wire.Bytes   `json:"data"`
}

// CommunitySendResult is one delivery's result.
type CommunitySendResult struct {
	ID      string       `json:"id"`
	Status  string       `json:"status"`
	Error   string       `json:"error,omitempty"`
	Receipt *wire.Signed `json:"receipt,omitempty"`
	Seq     *uint64      `json:"seq,omitempty"`
	Group   wire.Bytes   `json:"group,omitempty"`
	Retry   int64        `json:"retry,omitempty"`
}

// Send submits deliveries, and returns one result for each.
func (cm *Community) Send(ctx context.Context, out ...CommunityOutgoing) ([]CommunitySendResult, error) {
	var res struct {
		Results []CommunitySendResult `json:"results"`
	}
	if err := cm.authed(ctx, http.MethodPost, "send", nil, map[string]any{"deliveries": out}, &res); err != nil {
		return nil, err
	}
	if len(res.Results) != len(out) {
		return nil, errors.New("client: the community server answered for another number of deliveries")
	}
	return res.Results, nil
}

// Inbox fetches the community inbox after a cursor.
func (cm *Community) Inbox(ctx context.Context, after string, limit int) (*Page, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var p Page
	err := cm.authed(ctx, http.MethodGet, "inbox", q, nil, &p)
	return &p, err
}

// SetCursor sets the cursor the stream replays after, as the caller
// saved it.
func (cm *Community) SetCursor(cursor string) {
	cm.mu.Lock()
	cm.cursorV = cursor
	cm.mu.Unlock()
}

// Cursor returns the last cursor acked.
func (cm *Community) Cursor() string {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.cursorV
}

// Ack marks inbox items processed up to a cursor.
func (cm *Community) Ack(ctx context.Context, cursor string) error {
	if err := cm.authed(ctx, http.MethodPost, "inbox/ack", nil, map[string]string{"upto": cursor}, nil); err != nil {
		return err
	}
	cm.SetCursor(cursor)
	return nil
}

// Stream opens the community server's live stream, which reconnects on
// its own, as the homeserver's does. Voice signaling arrives as
// EventVoice.
func (cm *Community) Stream(ctx context.Context, opts StreamOptions) <-chan Event {
	size := opts.Buffer
	if size <= 0 {
		size = 64
	}
	ch := make(chan Event, size)
	go runStream(ctx, communityStream{cm}, opts, ch)
	return ch
}

type communityStream struct{ cm *Community }

func (s communityStream) prepare(ctx context.Context) (string, error) {
	if err := s.cm.connect(ctx); err != nil {
		return "", err
	}
	if s.cm.token() == "" {
		if err := s.cm.SignIn(ctx); err != nil {
			if wire.IsCode(err, wire.CodeNotAllowed) {
				return "", streamStop{err}
			}
			return "", err
		}
	}
	return s.cm.api.base, nil
}

func (s communityStream) token() string                    { return s.cm.token() }
func (s communityStream) signIn(ctx context.Context) error { return s.cm.SignIn(ctx) }
func (s communityStream) cursor() string                   { return s.cm.Cursor() }
func (s communityStream) http() *http.Client               { return s.cm.c.opts.httpClient() }
func (s communityStream) what() string                     { return "community server" }
func (s communityStream) restarted() <-chan struct{}       { return nil }
func (s communityStream) rotating() bool                   { return false }

// ChannelItem is one stored channel message, or a deleted one's place.
type ChannelItem struct {
	Seq      uint64    `json:"seq"`
	Delivery *Delivery `json:"delivery,omitempty"`
	Deleted  uint64    `json:"deleted,omitempty"`
}

// ChannelMessages fetches a text channel's stored messages from a
// sequence number on (GET channels/{channel}/messages).
func (cm *Community) ChannelMessages(ctx context.Context, channel, from uint64, limit int) ([]ChannelItem, string, error) {
	q := url.Values{"from": {strconv.FormatUint(from, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var res struct {
		Items []ChannelItem `json:"items"`
		Next  string        `json:"next,omitempty"`
	}
	err := cm.authed(ctx, http.MethodGet, "channels/"+strconv.FormatUint(channel, 10)+"/messages", q, nil, &res)
	return res.Items, res.Next, err
}

// Segment is a history segment's range, as GET history/list gives it.
type Segment struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
	Size  uint64 `json:"size"`
	TS    int64  `json:"ts"`
}

// PutHistory uploads a history segment (POST history/put).
func (cm *Community) PutHistory(ctx context.Context, channel, start, end uint64, ct []byte, replace bool) error {
	req := map[string]any{"channel": channel, "start": start, "end": end, "ct": wire.Bytes(ct)}
	if replace {
		req["replace"] = true
	}
	return cm.authed(ctx, http.MethodPost, "history/put", nil, req, nil)
}

// ListHistory lists a channel's history segments, every page of them.
func (cm *Community) ListHistory(ctx context.Context, channel uint64) ([]Segment, error) {
	var out []Segment
	after := ""
	for range 1000 {
		q := url.Values{"channel": {strconv.FormatUint(channel, 10)}}
		if after != "" {
			q.Set("after", after)
		}
		var res struct {
			Items []Segment `json:"items"`
			Next  string    `json:"next,omitempty"`
		}
		if err := cm.authed(ctx, http.MethodGet, "history/list", q, nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.Next == "" {
			break
		}
		after = res.Next
	}
	return out, nil
}

// GetHistory fetches one segment's ciphertext.
func (cm *Community) GetHistory(ctx context.Context, channel, start uint64) ([]byte, error) {
	q := url.Values{"channel": {strconv.FormatUint(channel, 10)}, "start": {strconv.FormatUint(start, 10)}}
	var res struct {
		CT wire.Bytes `json:"ct"`
	}
	err := cm.authed(ctx, http.MethodGet, "history/get", q, nil, &res)
	return res.CT, err
}

// PutRead sends how far the member has read a text channel (POST read).
func (cm *Community) PutRead(ctx context.Context, channel, seq uint64) error {
	return cm.authed(ctx, http.MethodPost, "read", nil, map[string]uint64{"channel": channel, "seq": seq}, nil)
}

// ReadPositions fetches the member's own read positions (GET read).
func (cm *Community) ReadPositions(ctx context.Context) (map[uint64]uint64, error) {
	var res struct {
		Items []ChannelSeq `json:"items"`
	}
	if err := cm.authed(ctx, http.MethodGet, "read", nil, nil, &res); err != nil {
		return nil, err
	}
	out := map[uint64]uint64{}
	for _, it := range res.Items {
		out[it.Channel] = it.Seq
	}
	return out, nil
}

// DeleteMessage tells the server to drop a deleted message's ciphertext
// (POST messages/delete). by is the seq of the signed delete.
func (cm *Community) DeleteMessage(ctx context.Context, channel, seq, by uint64) error {
	return cm.authed(ctx, http.MethodPost, "messages/delete", nil, map[string]uint64{"channel": channel, "seq": seq, "by": by}, nil)
}

// Kick removes someone from the community (POST members/kick).
func (cm *Community) Kick(ctx context.Context, id string) error {
	return cm.authed(ctx, http.MethodPost, "members/kick", nil, map[string]string{"id": id}, nil)
}

// RegisterEpochKey registers an epoch key after a reg commit.
func (cm *Community) RegisterEpochKey(ctx context.Context, key wire.Signed) error {
	return cm.authed(ctx, http.MethodPost, "groups/epoch-key", nil, map[string]any{"key": key}, nil)
}

// VerifySigned checks an object the community server signed with a kid,
// such as a commit receipt, against its server document.
func (cm *Community) VerifySigned(ctx context.Context, s wire.Signed) error {
	return cm.c.resolver.VerifyServerSigned(ctx, cm.server, s, cm.c.clock.Now())
}

// JoinRequest is someone asking to join a community that needs approval.
type JoinRequest struct {
	ID   string `json:"id"`
	Home string `json:"home"`
	TS   int64  `json:"ts"`
}

// RequestJoin asks to join a community that needs approval
// (POST join/request). Upload key packages first: a moderator can add
// only devices that have some (§16.6).
func (cm *Community) RequestJoin(ctx context.Context) error {
	return cm.authed(ctx, http.MethodPost, "join/request", nil, struct{}{}, nil)
}

// JoinRequests lists the pending join requests, every page of them
// (GET join/requests). It needs moderate.
func (cm *Community) JoinRequests(ctx context.Context) ([]JoinRequest, error) {
	var out []JoinRequest
	after := ""
	for range 100 {
		var res struct {
			Items []JoinRequest `json:"items"`
			Next  string        `json:"next"`
		}
		var q map[string][]string
		if after != "" {
			q = map[string][]string{"after": {after}}
		}
		if err := cm.authed(ctx, http.MethodGet, "join/requests", q, nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.Next == "" {
			break
		}
		after = res.Next
	}
	return out, nil
}

// ApproveJoin approves a pending request, and claims one key package for
// each of the joiner's devices (POST join/approve). The approving member
// then commits the Add with its approval, and sends the Welcome.
func (cm *Community) ApproveJoin(ctx context.Context, id string) ([]ClaimedKP, error) {
	var res struct {
		Packages []ClaimedKP `json:"packages"`
	}
	err := cm.authed(ctx, http.MethodPost, "join/approve", nil, map[string]string{"id": id}, &res)
	return res.Packages, err
}

// DeclineJoin declines a pending request (POST join/decline).
func (cm *Community) DeclineJoin(ctx context.Context, id string) error {
	return cm.authed(ctx, http.MethodPost, "join/decline", nil, map[string]string{"id": id}, nil)
}
