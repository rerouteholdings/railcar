package client

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// TypeProfile is the type tag of a public profile (§13.1).
const TypeProfile = "profile"

// Profile is the body of a public profile (§13.1). It isn't end-to-end
// encrypted: anyone can see it.
type Profile struct {
	ID     string       `json:"id"`
	V      uint64       `json:"v"`
	TS     int64        `json:"ts"`
	Name   string       `json:"name,omitempty"`
	Bio    string       `json:"bio,omitempty"`
	Links  []string     `json:"links,omitempty"`
	Avatar *PublicMedia `json:"avatar,omitempty"`
	Invite string       `json:"invite,omitempty"`
}

// Limits on a profile (§13.1).
const (
	MaxProfileName = 64
	MaxProfileBio  = 500
	MaxLinks       = 5
	MaxStatus      = 128
)

// Check checks a profile's fields against §13.1's limits.
func (p Profile) Check() error {
	switch {
	case utf8.RuneCountInString(p.Name) > MaxProfileName:
		return errors.New("client: a display name is at most 64 characters")
	case utf8.RuneCountInString(p.Bio) > MaxProfileBio:
		return errors.New("client: a bio is at most 500 characters")
	case len(p.Links) > MaxLinks:
		return errors.New("client: a profile has at most 5 links")
	}
	for _, l := range p.Links {
		if !strings.HasPrefix(l, "https://") {
			return errors.New("client: profile links are https URLs")
		}
	}
	return nil
}

type profileMsg struct {
	Profile wire.Signed `json:"profile"`
}

// ErrNoProfile reports an identity that hasn't published a profile.
var ErrNoProfile = errors.New("client: no profile")

// FetchProfile fetches someone's public profile (GET profile/{id}) and
// checks it: the signature, and that the signer is a current device of id
// in the chain this device holds, fetching the chain if it has none
// (§13.1). An identity without a profile is ErrNoProfile.
func (c *Client) FetchProfile(ctx context.Context, id string) (Profile, error) {
	if !identity.ValidIdentifier(id) {
		return Profile{}, fmt.Errorf("client: %q isn't an identifier", id)
	}
	var res profileMsg
	err := c.authed(ctx, http.MethodGet, "profile/"+id, nil, nil, &res)
	if wire.IsCode(err, wire.CodeNotFound) {
		return Profile{}, ErrNoProfile
	}
	if err != nil {
		return Profile{}, err
	}
	chain, err := c.cachedChain(id)
	if err != nil {
		return Profile{}, err
	}
	p, err := checkProfile(res.Profile, id, chain)
	if err != nil && id != c.ID() {
		// A device added since this copy: check against a fresh chain.
		if chain, err = c.FetchChain(ctx, id, FetchOptions{}); err != nil {
			return Profile{}, err
		}
		p, err = checkProfile(res.Profile, id, chain)
	}
	return p, err
}

func checkProfile(s wire.Signed, id string, chain *identity.Chain) (Profile, error) {
	if s.T != TypeProfile {
		return Profile{}, errors.New("client: not a profile")
	}
	if chain == nil {
		return Profile{}, errors.New("client: no chain to check the profile against")
	}
	d, ok := chain.State.Device(s.K)
	if !ok {
		return Profile{}, errors.New("client: the profile isn't signed by a current device")
	}
	if err := sig.VerifyDevice(s, d.Pub); err != nil {
		return Profile{}, fmt.Errorf("client: the profile's signature: %w", err)
	}
	var p Profile
	if err := s.Body(&p); err != nil {
		return Profile{}, err
	}
	if p.ID != id {
		return Profile{}, errors.New("client: a profile for another identity")
	}
	return p, nil
}

// PublishProfile signs and publishes this identity's profile
// (POST profile, §13.1), with v one higher than the published one. A
// stale-version answer means another device published first: it fetches
// that one, and tries again once.
func (c *Client) PublishProfile(ctx context.Context, p Profile) (Profile, error) {
	if err := p.Check(); err != nil {
		return Profile{}, err
	}
	p.ID = c.ID()
	for attempt := 0; ; attempt++ {
		cur, err := c.FetchProfile(ctx, c.ID())
		if err != nil && !errors.Is(err, ErrNoProfile) {
			return Profile{}, err
		}
		p.V = cur.V + 1
		p.TS = c.clock.Now().Unix()
		s, err := sig.SignDevice(c.device.Key, TypeProfile, p)
		if err != nil {
			return Profile{}, err
		}
		err = c.authed(ctx, http.MethodPost, "profile", nil, profileMsg{Profile: s}, nil)
		if wire.IsCode(err, wire.CodeStaleVersion) && attempt == 0 {
			continue
		}
		return p, err
	}
}

// Presence states (§13.2).
const (
	PresenceAvailable = "available"
	PresenceOnline    = "online"
	PresenceAway      = "away"
	PresenceBusy      = "busy"
	PresenceOffline   = "offline"
)

// ValidPresence reports whether s is a presence state.
func ValidPresence(s string) bool {
	switch s {
	case PresenceAvailable, PresenceOnline, PresenceAway, PresenceBusy, PresenceOffline:
		return true
	}
	return false
}

type presenceReq struct {
	State  string `json:"state"`
	Status string `json:"status,omitempty"`
}

// SetPresence sets this identity's presence state and status message
// (POST presence, §13.2). An empty status clears it. Homeservers see
// presence, and pass it to mutual contacts.
func (c *Client) SetPresence(ctx context.Context, state, status string) error {
	if !ValidPresence(state) {
		return fmt.Errorf("client: %q isn't a presence state", state)
	}
	if utf8.RuneCountInString(status) > MaxStatus {
		return errors.New("client: a status message is at most 128 characters")
	}
	return c.authed(ctx, http.MethodPost, "presence", nil, presenceReq{State: state, Status: status}, nil)
}

// Presence is a contact's presence, from the live stream (§13.2).
type Presence struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Status string `json:"status,omitempty"`
	PV     uint64 `json:"pv"`
	TS     int64  `json:"ts"`
}

// ParsePresence reads a stream's presence frame.
func ParsePresence(frame jsontext.Value) (Presence, error) {
	var p Presence
	if err := wire.Unmarshal(frame, &p); err != nil {
		return Presence{}, err
	}
	if !identity.ValidIdentifier(p.ID) || !ValidPresence(p.State) {
		return Presence{}, errors.New("client: a malformed presence frame")
	}
	return p, nil
}
