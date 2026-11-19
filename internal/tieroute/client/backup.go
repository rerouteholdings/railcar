package client

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strconv"

	"golang.org/x/crypto/argon2"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Account data and the message history backup (§7.4, §7.5, client API,
// Account data and backups).

// Argon2Params are the Argon2id parameters a passphrase wrap stores: m in
// KiB, t passes, p lanes (§3, §7.5).
type Argon2Params struct {
	Salt wire.Bytes `json:"salt"`
	M    uint32     `json:"m"`
	T    uint32     `json:"t"`
	P    uint8      `json:"p"`
}

// The Argon2id parameters §3 fixes: 64 MiB, 3 passes, 1 lane.
const (
	argon2M = 64 * 1024
	argon2T = 3
	argon2P = 1
)

// maxArgon2M bounds the memory a wrap from the server can make this
// device spend, at 1 GiB.
const maxArgon2M = 1 << 20

// passphraseKey derives the 16-byte key a passphrase wraps the account
// key under.
func passphraseKey(passphrase string, p Argon2Params) ([]byte, error) {
	if len(p.Salt) != 16 || p.M == 0 || p.M > maxArgon2M || p.T == 0 || p.T > 16 || p.P == 0 {
		return nil, errors.New("client: a passphrase wrap with Argon2id parameters out of range")
	}
	return argon2.IDKey([]byte(passphrase), p.Salt, p.T, p.M, p.P, 16), nil
}

// AccountKeys fetches the wrapped copies of the account key
// (GET account/keys).
func (c *Client) AccountKeys(ctx context.Context) ([]WrappedKey, error) {
	var res accountKeysRes
	if err := c.authed(ctx, http.MethodGet, "account/keys", nil, nil, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// SetPassphrase stores a copy of the account key wrapped under a backup
// passphrase, with a new salt (§7.5). The caller has checked the
// passphrase's strength.
func (c *Client) SetPassphrase(ctx context.Context, passphrase string) error {
	ak := c.AccountKey()
	if len(ak) != 16 {
		return errors.New("client: this device doesn't have the account key")
	}
	p := Argon2Params{Salt: make([]byte, 16), M: argon2M, T: argon2T, P: argon2P}
	if _, err := rand.Read(p.Salt); err != nil {
		return err
	}
	k, err := passphraseKey(passphrase, p)
	if err != nil {
		return err
	}
	ct, err := sealAESGCM(k, ak, accountKeyAD(c.ID(), viaPassphrase, nil))
	if err != nil {
		return err
	}
	return c.authed(ctx, http.MethodPost, "account/keys", nil, accountKeysSet{Set: WrappedKey{Via: viaPassphrase, CT: ct, Argon2: &p}}, nil)
}

// RemovePassphrase deletes the passphrase copy of the account key, when
// the backup is turned off.
func (c *Client) RemovePassphrase(ctx context.Context) error {
	return c.authed(ctx, http.MethodPost, "account/keys", nil, map[string]string{"remove": viaPassphrase}, nil)
}

// StoreWrappedKey stores a wrapped copy of the account key as it is, such
// as a copy taken from the old homeserver in a move (§6.6).
func (c *Client) StoreWrappedKey(ctx context.Context, w WrappedKey) error {
	return c.authed(ctx, http.MethodPost, "account/keys", nil, accountKeysSet{Set: w}, nil)
}

// ErrNoPassphraseWrap reports a homeserver without a passphrase copy of
// the account key, or one the passphrase doesn't open.
var ErrNoPassphraseWrap = errors.New("client: the passphrase doesn't open the account key on the homeserver")

// UnlockWithPassphrase gets the account key from its passphrase copy, for
// a device without it, and keeps it (§7.5).
func (c *Client) UnlockWithPassphrase(ctx context.Context, passphrase string) error {
	items, err := c.AccountKeys(ctx)
	if err != nil {
		return err
	}
	for _, w := range items {
		if w.Via != viaPassphrase || w.Argon2 == nil {
			continue
		}
		k, err := passphraseKey(passphrase, *w.Argon2)
		if err != nil {
			continue
		}
		ak, err := openAESGCM(k, w.CT, accountKeyAD(c.ID(), viaPassphrase, nil))
		if err != nil || len(ak) != 16 {
			continue
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.state.AccountKey = ak
		return c.saveLocked()
	}
	return ErrNoPassphraseWrap
}

// AccountData is the account data (§7.4). Members this client doesn't
// use are kept as they are.
type AccountData struct {
	Version uint64
	Backup  *BackupPos
	Other   map[string]jsontext.Value
}

// BackupPos names the backup device and its newest segment (§7.4, §7.5).
type BackupPos struct {
	Device wire.Bytes `json:"device"`
	Seq    uint64     `json:"seq"`
}

func accountDataAD(id string, version uint64) []byte {
	return wire.MustJCS([]any{"tieroute account data", id, version})
}

// ErrBadAccountData reports account data that doesn't open with the
// account key for its identity and version: the homeserver misbehaving,
// not data to repair (§7.4).
var ErrBadAccountData = errors.New("client: the homeserver's account data doesn't open with the account key")

// GetAccountData fetches and decrypts the account data (GET account/data).
// Version 0 means it was never written.
func (c *Client) GetAccountData(ctx context.Context) (*AccountData, error) {
	ak := c.AccountKey()
	if len(ak) != 16 {
		return nil, errors.New("client: this device doesn't have the account key")
	}
	var res struct {
		Version uint64     `json:"version"`
		CT      wire.Bytes `json:"ct"`
	}
	if err := c.authed(ctx, http.MethodGet, "account/data", nil, nil, &res); err != nil {
		return nil, err
	}
	d := &AccountData{Version: res.Version, Other: map[string]jsontext.Value{}}
	if res.Version == 0 || len(res.CT) == 0 {
		return d, nil
	}
	pt, err := openAESGCM(ak, res.CT, accountDataAD(c.ID(), res.Version))
	if err != nil {
		return nil, ErrBadAccountData
	}
	if err := wire.Unmarshal(pt, &d.Other); err != nil {
		return nil, ErrBadAccountData
	}
	if raw, ok := d.Other["backup"]; ok {
		var b BackupPos
		if wire.Unmarshal(raw, &b) == nil {
			d.Backup = &b
		}
		delete(d.Other, "backup")
	}
	return d, nil
}

// PutAccountData encrypts and stores the account data over the version
// it was read at (POST account/data). It returns wire.CodeStaleVersion
// when another device wrote first; the caller reads, merges, and tries
// again.
func (c *Client) PutAccountData(ctx context.Context, d *AccountData) error {
	ak := c.AccountKey()
	if len(ak) != 16 {
		return errors.New("client: this device doesn't have the account key")
	}
	m := map[string]any{}
	for k, v := range d.Other {
		m[k] = v
	}
	// The members account-data requires (§7.4).
	for k, v := range map[string]any{"folders": []any{}, "nicknames": map[string]any{}, "settings": map[string]any{}} {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	if d.Backup != nil {
		m["backup"] = d.Backup
	}
	pt, err := wire.Marshal(m)
	if err != nil {
		return err
	}
	ct, err := sealAESGCM(ak, pt, accountDataAD(c.ID(), d.Version+1))
	if err != nil {
		return err
	}
	var res struct {
		Version uint64 `json:"version"`
	}
	req := struct {
		CT       wire.Bytes `json:"ct"`
		Replaces uint64     `json:"replaces"`
	}{ct, d.Version}
	if err := c.authed(ctx, http.MethodPost, "account/data", nil, req, &res); err != nil {
		return err
	}
	d.Version = res.Version
	return nil
}

// backupKey is HKDF-Expand(HKDF-Extract("tieroute backup", account-key),
// "segments", 16) (§7.5).
func backupKey(accountKey []byte) ([]byte, error) {
	prk, err := hkdf.Extract(sha256.New, accountKey, []byte("tieroute backup"))
	if err != nil {
		return nil, err
	}
	return hkdf.Expand(sha256.New, prk, "segments", 16)
}

func backupAD(id string, seq uint64) []byte {
	return wire.MustJCS([]any{"tieroute backup", id, seq})
}

// MaxSegment is the most plaintext one backup segment holds: 1 MiB
// (§7.5).
const MaxSegment = 1 << 20

// BackupSegment is one stored segment, as backup/list names it.
type BackupSegment struct {
	Seq      uint64 `json:"seq"`
	Size     uint64 `json:"size"`
	Snapshot bool   `json:"snapshot,omitempty"`
}

// PutBackup encrypts a segment's plaintext and uploads it
// (POST backup/put).
func (c *Client) PutBackup(ctx context.Context, seq uint64, plaintext []byte, snapshot bool) error {
	if len(plaintext) > MaxSegment {
		return errors.New("client: a backup segment over 1 MiB")
	}
	k, err := backupKey(c.AccountKey())
	if err != nil {
		return err
	}
	ct, err := sealAESGCM(k, plaintext, backupAD(c.ID(), seq))
	if err != nil {
		return err
	}
	req := struct {
		Seq      uint64     `json:"seq"`
		CT       wire.Bytes `json:"ct"`
		Snapshot bool       `json:"snapshot,omitempty"`
	}{seq, ct, snapshot}
	return c.authed(ctx, http.MethodPost, "backup/put", nil, req, nil)
}

// ListBackup lists the stored segments, in seq order (GET backup/list).
func (c *Client) ListBackup(ctx context.Context) ([]BackupSegment, error) {
	var res struct {
		Segments []BackupSegment `json:"segments"`
	}
	if err := c.authed(ctx, http.MethodGet, "backup/list", nil, nil, &res); err != nil {
		return nil, err
	}
	return res.Segments, nil
}

// GetBackup fetches one segment and decrypts it (GET backup/get). A
// segment that doesn't open for this identity and seq is an error.
func (c *Client) GetBackup(ctx context.Context, seq uint64) ([]byte, error) {
	var res struct {
		Seq uint64     `json:"seq"`
		CT  wire.Bytes `json:"ct"`
	}
	q := map[string][]string{"seq": {strconv.FormatUint(seq, 10)}}
	if err := c.authed(ctx, http.MethodGet, "backup/get", q, nil, &res); err != nil {
		return nil, err
	}
	k, err := backupKey(c.AccountKey())
	if err != nil {
		return nil, err
	}
	pt, err := openAESGCM(k, res.CT, backupAD(c.ID(), seq))
	if err != nil {
		return nil, errors.New("client: a backup segment that doesn't open for this identity and its number")
	}
	return pt, nil
}
