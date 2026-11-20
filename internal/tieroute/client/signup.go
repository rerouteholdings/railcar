package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Signup is an identity being set up on this device. It holds the new
// recovery code until Register finishes, so the interface can show the
// words and ask for some of them back before anything is registered
// (§6.1). After Register, the code is gone from memory.
type Signup struct {
	opts    Options
	server  string
	words   identity.RecoveryWords
	code    []byte
	device  *identity.Device
	profile *store.Profile
	done    bool

	genesis   *wire.Signed // the last genesis sent, for a retry
	handle    string
	genesisTS int64
}

// ErrRegistrationClosed reports a homeserver that isn't taking new
// identities.
var ErrRegistrationClosed = errors.New("client: this homeserver isn't registering new identities")

// NewSignup starts setting up a new identity on server, a homeserver's
// name such as "chat.example.com". It makes the device key and the
// recovery code, and checks that the server is a version 2 homeserver
// that registers by invite.
func NewSignup(ctx context.Context, server string, opts Options) (*Signup, error) {
	server = strings.ToLower(strings.TrimSpace(server))
	if err := identity.CheckServerName(server); err != nil {
		return nil, err
	}
	r := opts.resolver()
	doc, err := r.ServerDoc(ctx, server)
	if err != nil {
		return nil, err
	}
	if _, err := clientBase(doc); err != nil {
		return nil, err
	}
	if doc.Registration == "closed" {
		return nil, ErrRegistrationClosed
	}
	code, words, err := identity.NewRecoveryCode()
	if err != nil {
		return nil, err
	}
	dev, err := identity.GenerateDevice(opts.DeviceName)
	if err != nil {
		return nil, err
	}
	return &Signup{opts: opts, server: server, words: words, code: code, device: dev}, nil
}

// Words returns the recovery words to show the user, once (§6.1).
func (s *Signup) Words() identity.RecoveryWords { return s.words }

// Server returns the homeserver's name.
func (s *Signup) Server() string { return s.server }

// PublicInvite returns the registration code the server publishes in its
// server document, if any (§8.2).
func PublicInvite(ctx context.Context, server string, opts Options) (string, error) {
	r := opts.resolver()
	doc, err := r.ServerDoc(ctx, server)
	if err != nil {
		return "", err
	}
	return doc.Invite, nil
}

type registerReq struct {
	Genesis wire.Signed `json:"genesis"`
	Invite  string      `json:"invite,omitempty"`
}

type registerRes struct {
	ID string `json:"id"`
}

// Register registers the identity with handle and the registration code
// invite (§8.2), then creates the profile in profileDir, signs in, and
// stores the account key's recovery wrap (§7.4). handle is a full handle,
// or one label to put under the server's handle domain, such as "kat".
//
// If the handle is taken (wire.CodeHandleTaken) or the code is refused
// (wire.CodeBadInvite), nothing is registered and the caller can try again
// with another handle or code; the recovery words stay the same.
func (s *Signup) Register(ctx context.Context, profileDir, handle, invite string) (*Client, error) {
	if s.done {
		return nil, errors.New("client: this signup already registered")
	}
	// The profile holds the device key before anything is registered, so a
	// registration whose answer is lost still has its key.
	if s.profile == nil {
		p, err := store.Create(profileDir, s.opts.Store)
		if err != nil {
			return nil, err
		}
		s.profile = p
	} else if s.profile.Dir() != profileDir {
		return nil, errors.New("client: a signup keeps one profile directory")
	}
	r := s.opts.resolver()
	doc, err := r.ServerDoc(ctx, s.server)
	if err != nil {
		return nil, err
	}
	base, err := clientBase(doc)
	if err != nil {
		return nil, err
	}
	handle = identity.NormalizeHandle(handle)
	if !strings.Contains(handle, ".") {
		if doc.Handles == "" {
			return nil, fmt.Errorf("client: %s gives out no handles; enter a full handle", s.server)
		}
		if identity.ReservedLabel(handle) {
			return nil, fmt.Errorf("client: %q is reserved", handle)
		}
		handle += "." + doc.Handles
	}
	if err := identity.CheckHandle(handle); err != nil {
		return nil, err
	}
	rec, err := identity.NewRecovery(s.code)
	if err != nil {
		return nil, err
	}
	commit1, err := rec.Commitment(1)
	if err != nil {
		return nil, err
	}
	wrapKey := rec.WrapKey()
	defer clear(wrapKey)

	clock := &Clock{}
	if !doc.Date.IsZero() {
		clock.observeRough(doc.Date, time.Now(), time.Now())
	}
	a := &api{base: base, http: s.opts.httpClient(), clock: clock, log: s.opts.Logger}

	accountKey := make([]byte, 16)
	if _, err := rand.Read(accountKey); err != nil {
		return nil, err
	}
	st := savedState{Server: s.server, AccountKey: accountKey}
	if o, ok := s.opts.Origins[s.server]; ok {
		st.Origin = o
	}
	c := newClient(s.profile, s.opts)
	c.clock = clock
	c.resolver.Self = s.server
	c.device = s.device

	// Register the genesis. A clock-skew answer carries the server's time,
	// so the genesis is signed again with it once (§8.4). A new genesis is
	// a new identifier, which is fine before anything is registered.
	var genesis wire.Signed
	var id string
	for attempt := 0; ; attempt++ {
		// A retry for the same handle sends the same genesis while the
		// server would still take its ts, so a lost answer doesn't leave a
		// second identity behind.
		if g := s.genesis; attempt == 0 && g != nil && s.handle == handle && clock.Now().Unix()-s.genesisTS < 9*60 {
			genesis = *g
		} else {
			genesis, err = identity.NewGenesis(s.device, s.server, handle, commit1, clock.Now().Unix())
			if err != nil {
				return nil, err
			}
			s.genesis, s.handle, s.genesisTS = &genesis, handle, clock.Now().Unix()
		}
		id = identity.IdentifierOf(genesis)
		// Save the keys and the genesis first: if the answer is lost, the
		// same genesis registers again, since registering is idempotent.
		st.ID, st.Chain = id, []wire.Signed{genesis}
		if c.chain, err = identity.Validate(st.Chain, identity.Options{}); err != nil {
			return nil, err
		}
		c.state = st
		c.mu.Lock()
		err = c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		var res registerRes
		err = a.call(ctx, http.MethodPost, "register", nil, registerReq{Genesis: genesis, Invite: invite}, &res)
		if wire.IsCode(err, wire.CodeClockSkew) && attempt == 0 {
			continue
		}
		if wire.IsCode(err, wire.CodeRegistrationClosed) {
			return nil, ErrRegistrationClosed
		}
		if err != nil {
			return nil, err
		}
		if res.ID != id {
			return nil, fmt.Errorf("client: the server registered %s, not %s", res.ID, id)
		}
		break
	}
	// The recovery code isn't kept on the device after setup (§6.1).
	clear(s.code)
	s.code, s.done = nil, true

	// The wrap is ciphertext under recovery_wrap, so it can wait in the
	// profile until the homeserver takes it.
	wrapped, err := wrapAccountKey(accountKey, wrapKey, id, viaRecovery, commit1)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.state.PendingWrap = &wrapped
	err = c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return c, err
	}
	if err := c.SignIn(ctx); err != nil {
		return c, fmt.Errorf("client: registered, but signing in failed: %w", err)
	}
	if err := c.flushRecoveryWrap(ctx); err != nil {
		return c, fmt.Errorf("client: registered, but storing the recovery wrap failed: %w", err)
	}
	return c, nil
}

// Register signs up in one step: it makes the recovery code, registers
// the identity, and returns the client and the recovery words to show
// once. An interface that asks the user to type the words back before
// registering uses NewSignup instead.
func Register(ctx context.Context, server, handle, invite, profileDir string, opts Options) (*Client, identity.RecoveryWords, error) {
	s, err := NewSignup(ctx, server, opts)
	if err != nil {
		return nil, identity.RecoveryWords{}, err
	}
	c, err := s.Register(ctx, profileDir, handle, invite)
	if c == nil && err != nil {
		s.Abort()
	}
	return c, s.Words(), err
}

// Abort gives up on a signup that never registered, and deletes the
// profile it made.
func (s *Signup) Abort() error {
	if s.done || s.profile == nil {
		return nil
	}
	clear(s.code)
	err := s.profile.Destroy()
	s.profile = nil
	return err
}
