package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// KeyPackageMaker makes MLS key packages for this device (§11.4). The MLS
// layer implements it; each key package's leaf carries the device's
// credential and current msg-key (§7.1), which MLSInfo provides.
type KeyPackageMaker interface {
	// KeyPackages returns n new one-time key packages, or one last-resort
	// key package when lastResort is set, as raw MLSMessage bytes.
	KeyPackages(n int, lastResort bool) ([][]byte, error)
}

// MLSInfo is what the MLS layer needs to make this device's key packages
// and leaves (§3.2.3, §7.1).
type MLSInfo struct {
	// Credential is the BasicCredential's identity: JCS([identifier,
	// device-key]).
	Credential []byte
	// MsgKeySeed is the 32-byte Ed25519 seed of the message key, the MLS
	// signature key. It's a secret.
	MsgKeySeed []byte
	// MsgKeyPublic is the message key's public key.
	MsgKeyPublic []byte
	// MsgKeyExtension is the tieroute_msg_key leaf node extension's data
	// (type 0xF0A2): JCS of the signed msg-key.
	MsgKeyExtension []byte
}

// MLSInfo returns this device's credential and message key for the MLS
// layer.
func (c *Client) MLSInfo() (MLSInfo, error) {
	id := c.ID()
	cert, err := c.device.MsgKeyCert(id)
	if err != nil {
		return MLSInfo{}, err
	}
	pub := c.device.MsgKey.Public().(ed25519.PublicKey)
	return MLSInfo{
		Credential:      identity.MLSIdentity(id, c.device.ID()),
		MsgKeySeed:      c.device.MsgKey.Seed(),
		MsgKeyPublic:    append([]byte{}, pub...),
		MsgKeyExtension: cert.Canonical(),
	}, nil
}

// KPWithBinding is a key package and its kp-binding (§3.2.2).
type KPWithBinding struct {
	KP      wire.Bytes  `json:"kp"`
	Binding wire.Signed `json:"binding"`
}

type kpUploadReq struct {
	Packages   []KPWithBinding `json:"packages"`
	LastResort *KPWithBinding  `json:"last-resort,omitempty"`
	Replace    bool            `json:"replace,omitempty"`
}

type kpUploadRes struct {
	Count int `json:"count"`
}

// UploadOptions adjust UploadKeyPackages.
type UploadOptions struct {
	// LastResort also uploads a new last-resort key package (§11.4).
	LastResort bool
	// Replace deletes this device's unclaimed one-time packages first.
	Replace bool
}

// DefaultKeyPackages is how many one-time key packages a device keeps on
// its homeserver (§11.4).
const DefaultKeyPackages = 50

// UploadKeyPackages makes n key packages, binds each with a kp-binding
// signed by the device's composite key (§3.2.2), and uploads them
// (POST keypackages/upload). It returns how many unclaimed one-time
// packages the homeserver now holds for this device.
func (c *Client) UploadKeyPackages(ctx context.Context, maker KeyPackageMaker, n int, opts UploadOptions) (int, error) {
	if maker == nil {
		return 0, errors.New("client: no key package maker")
	}
	id := c.ID()
	bind := func(kp []byte) (KPWithBinding, error) {
		b, err := c.device.KPBinding(id, kp)
		return KPWithBinding{KP: kp, Binding: b}, err
	}
	req := kpUploadReq{Packages: []KPWithBinding{}, Replace: opts.Replace}
	if n > 0 {
		kps, err := maker.KeyPackages(n, false)
		if err != nil {
			return 0, err
		}
		for _, kp := range kps {
			p, err := bind(kp)
			if err != nil {
				return 0, err
			}
			req.Packages = append(req.Packages, p)
		}
	}
	if opts.LastResort {
		kps, err := maker.KeyPackages(1, true)
		if err != nil {
			return 0, err
		}
		if len(kps) != 1 {
			return 0, errors.New("client: the maker didn't make one last-resort key package")
		}
		p, err := bind(kps[0])
		if err != nil {
			return 0, err
		}
		req.LastResort = &p
	}
	var res kpUploadRes
	if err := c.authed(ctx, http.MethodPost, "keypackages/upload", nil, req, &res); err != nil {
		return 0, err
	}
	return res.Count, nil
}

// KeyPackageCount says how many one-time key packages the homeserver holds
// for this device, and whether it has a last-resort one.
func (c *Client) KeyPackageCount(ctx context.Context) (count int, lastResort bool, err error) {
	var res struct {
		Count      int  `json:"count"`
		LastResort bool `json:"last-resort"`
	}
	err = c.authed(ctx, http.MethodGet, "keypackages/count", nil, nil, &res)
	return res.Count, res.LastResort, err
}
