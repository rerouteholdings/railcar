package client

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// ClaimedKP is one device's key package from a claim (§11.4).
type ClaimedKP struct {
	Dev        wire.Bytes   `json:"dev"`
	KP         wire.Bytes   `json:"kp"`
	Binding    *wire.Signed `json:"binding,omitempty"` // the kp-binding (§3.2.2)
	LastResort bool         `json:"last-resort"`
}

// ClaimOptions adjust ClaimKeyPackages.
type ClaimOptions struct {
	// Group names a group in the target's allowlist, for re-adding a
	// device (§11.9).
	Group string
	// Devices claims only for these current devices.
	Devices [][]byte
}

// ClaimKeyPackages claims one key package for each current device of an
// identity (POST keypackages/claim, §11.4). The caller checks each
// binding and leaf before adding the device (§3.2.2, §7.1).
func (c *Client) ClaimKeyPackages(ctx context.Context, id string, opts ClaimOptions) ([]ClaimedKP, error) {
	req := map[string]any{"id": id}
	if opts.Group != "" {
		req["group"] = opts.Group
	}
	if len(opts.Devices) > 0 {
		devs := make([]wire.Bytes, len(opts.Devices))
		for i, d := range opts.Devices {
			devs[i] = d
		}
		req["devices"] = devs
	}
	var res struct {
		Packages []ClaimedKP `json:"packages"`
	}
	err := c.authed(ctx, http.MethodPost, "keypackages/claim", nil, req, &res)
	return res.Packages, err
}

// CreateGroupOptions describe a group to register with its hub
// (POST groups/create, §11.5, §11.6).
type CreateGroupOptions struct {
	Group    string // the group ID, base64url
	Kind     string // "direct" or "group"
	EpochKey []byte // the composite public half of epoch_key_0
	Epoch    uint64 // after a hub handoff: the epoch the new hub starts at
	Hub      string // the hub, when it isn't this homeserver
}

// CreateGroup registers a new group with its hub and adds it to this
// identity's groups allowlist. A taken ID comes back as
// wire.CodeAlreadyExists; the caller picks a new one (§11.5).
func (c *Client) CreateGroup(ctx context.Context, opts CreateGroupOptions) error {
	req := map[string]any{"group": opts.Group, "kind": opts.Kind, "epoch-key": wire.Bytes(opts.EpochKey)}
	if opts.Epoch > 0 {
		req["epoch"] = opts.Epoch
	}
	if opts.Hub != "" {
		req["hub"] = opts.Hub
	}
	return c.authed(ctx, http.MethodPost, "groups/create", nil, req, nil)
}

// RegisterGroup adds a group to the groups allowlist after this device
// processes its Welcome (POST groups/register, §10.3).
func (c *Client) RegisterGroup(ctx context.Context, group string) error {
	return c.authed(ctx, http.MethodPost, "groups/register", nil, map[string]string{"group": group}, nil)
}

// UnregisterGroup removes a group from the allowlist on leaving
// (POST groups/unregister).
func (c *Client) UnregisterGroup(ctx context.Context, group string) error {
	return c.authed(ctx, http.MethodPost, "groups/unregister", nil, map[string]string{"group": group}, nil)
}

// HubCommit is a commit the hub kept (§11.6).
type HubCommit struct {
	Epoch   uint64      `json:"epoch"` // the epoch it was made in
	Data    wire.Bytes  `json:"data"`
	Receipt wire.Signed `json:"receipt"`
}

// FetchCommits fetches a group's commits made in epochs after `after`
// (GET groups/{group}/commits, §11.6). hub is the group's hub, or "" when
// it's this homeserver.
func (c *Client) FetchCommits(ctx context.Context, group string, after uint64, hub string) ([]HubCommit, error) {
	q := url.Values{"after": {strconv.FormatUint(after, 10)}}
	if hub != "" {
		q.Set("hub", hub)
	}
	var res struct {
		Items []HubCommit `json:"items"`
	}
	err := c.authed(ctx, http.MethodGet, "groups/"+group+"/commits", q, nil, &res)
	return res.Items, err
}

// RegisterEpochKey registers the key of the epoch a `reg` commit created
// (POST groups/epoch-key, §11.6).
func (c *Client) RegisterEpochKey(ctx context.Context, key wire.Signed, hub string) error {
	req := map[string]any{"key": key}
	if hub != "" {
		req["hub"] = hub
	}
	return c.authed(ctx, http.MethodPost, "groups/epoch-key", nil, req, nil)
}

// fetchServerDoc fetches another server's document through the homeserver
// (GET servers/{name}), so that server doesn't learn this device's
// address. The resolver uses it for every server but the homeserver.
func (c *Client) fetchServerDoc(ctx context.Context, name string) ([]byte, error) {
	var raw jsontext.Value
	if err := c.authed(ctx, http.MethodGet, "servers/"+name, nil, nil, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// VerifyServerSigned checks an object a server signed with a key named by
// kid, such as a hub's commit receipt (§11.6), against that server's
// document.
func (c *Client) VerifyServerSigned(ctx context.Context, server string, s wire.Signed) error {
	return c.resolver.VerifyServerSigned(ctx, server, s, c.clock.Now())
}

// VerifyServerSignedAt is VerifyServerSigned for an object signed at a
// time it gives, such as a receipt's ts: a key the server revoked by then
// doesn't count (§5.1).
func (c *Client) VerifyServerSignedAt(ctx context.Context, server string, s wire.Signed, signed int64) error {
	return c.resolver.VerifyServerSignedAt(ctx, server, s, c.clock.Now(), signed)
}
