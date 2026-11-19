package client

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"net/http"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Ways the account key is wrapped (§7.4).
const (
	viaRecovery     = "recovery"
	viaRecoveryNext = "recovery-next"
	viaPassphrase   = "passphrase"
)

// WrappedKey is the account key wrapped under another key (§7.4).
type WrappedKey struct {
	Via    string     `json:"via"`
	CT     wire.Bytes `json:"ct"`               // nonce || AES-128-GCM output
	Commit wire.Bytes `json:"commit,omitempty"` // for recovery wraps: the commitment to the code's recovery key 1
	// Argon2 holds a passphrase wrap's salt and parameters (§7.5).
	Argon2 *Argon2Params `json:"argon2,omitempty"`
}

// accountKeyAD is a wrapped key's associated data: JCS(["tieroute account
// key", identifier, via]), with the commit appended when there is one
// (§7.4).
func accountKeyAD(id, via string, commit []byte) []byte {
	parts := []string{"tieroute account key", id, via}
	if commit != nil {
		parts = append(parts, wire.EncodeB64(commit))
	}
	return wire.MustJCS(parts)
}

// sealAESGCM encrypts with AES-128-GCM and a random 12-byte nonce, which
// it prepends (§7.4).
func sealAESGCM(key, plain, ad []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return sealAESGCMNonce(key, nonce, plain, ad)
}

func sealAESGCMNonce(key, nonce, plain, ad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return g.Seal(append([]byte{}, nonce...), nonce, plain, ad), nil
}

func openAESGCM(key, sealed, ad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < g.NonceSize() {
		return nil, errShort
	}
	return g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], ad)
}

// wrapAccountKey wraps the account key for identity id. Recovery wraps
// carry commit, the commitment to their code's recovery key 1 (§6.3).
func wrapAccountKey(accountKey, wrapKey []byte, id, via string, commit [32]byte) (WrappedKey, error) {
	ct, err := sealAESGCM(wrapKey, accountKey, accountKeyAD(id, via, commit[:]))
	if err != nil {
		return WrappedKey{}, err
	}
	return WrappedKey{Via: via, CT: ct, Commit: commit[:]}, nil
}

// unwrapAccountKey opens a wrapped account key. A wrap served for another
// identity or purpose fails, which the caller treats as the server
// misbehaving (§7.4).
func unwrapAccountKey(w WrappedKey, wrapKey []byte, id string) ([]byte, error) {
	var commit []byte
	if len(w.Commit) > 0 {
		commit = w.Commit
	}
	return openAESGCM(wrapKey, w.CT, accountKeyAD(id, w.Via, commit))
}

type accountKeysSet struct {
	Set WrappedKey `json:"set"`
}

// flushRecoveryWrap stores a pending recovery wrap on the homeserver
// (POST account/keys), and forgets it locally once stored.
func (c *Client) flushRecoveryWrap(ctx context.Context) error {
	c.mu.Lock()
	w := c.state.PendingWrap
	c.mu.Unlock()
	if w == nil {
		return nil
	}
	if err := c.authed(ctx, http.MethodPost, "account/keys", nil, accountKeysSet{Set: *w}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.PendingWrap = nil
	return c.saveLocked()
}
