package client

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// MaxFile is the largest file: 64 MiB (§14).
const MaxFile = 64 << 20

// MaxPublicMedia is the largest profile picture: 1,000,000 bytes (§13.1).
const MaxPublicMedia = 1000000

// MediaRef names an encrypted file and carries its key, inside an
// encrypted message (§14).
type MediaRef struct {
	Server string     `json:"server"`
	ID     wire.Bytes `json:"id"`     // 16 random bytes
	Key    wire.Bytes `json:"key"`    // the AES-128-GCM key
	SHA256 wire.Bytes `json:"sha256"` // of the ciphertext
	Size   uint64     `json:"size"`   // plaintext bytes
	Name   string     `json:"name,omitempty"`
	MIME   string     `json:"mime,omitempty"`
}

// Check checks a media-ref's form (§14).
func (r MediaRef) Check() error {
	if r.Server == "" || len(r.ID) != 16 || len(r.Key) != 16 || len(r.SHA256) != 32 || r.Size > MaxFile {
		return errors.New("client: a malformed media-ref")
	}
	return nil
}

// PublicMedia is an unencrypted profile picture (§13.1).
type PublicMedia struct {
	Server string     `json:"server"`
	ID     wire.Bytes `json:"id"`
	SHA256 wire.Bytes `json:"sha256"` // of the file
	Size   uint64     `json:"size"`
	MIME   string     `json:"mime"`
}

// Errors for files (§14).
var (
	// ErrFileExpired is a file past its 30 days: show "this file has
	// expired".
	ErrFileExpired = errors.New("client: this file has expired")
	// ErrFileRemoved is a file deleted after a report (§17.2): show "this
	// file was removed".
	ErrFileRemoved = errors.New("client: this file was removed")
	// ErrFileTooLarge is a file over 64 MiB.
	ErrFileTooLarge = errors.New("client: files are at most 64 MiB")
)

// zeroNonce is the fixed nonce every file uses: each file has its own
// key and is encrypted once (§14).
var zeroNonce = make([]byte, 12)

func fileAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

type mediaUploadRes struct {
	ID     wire.Bytes `json:"id"`
	SHA256 wire.Bytes `json:"sha256"`
	Size   uint64     `json:"size"`
}

// UploadFile encrypts a file with a new random key and uploads it to the
// homeserver (POST media/upload, §14), and returns its media-ref. name
// and mime are optional; they travel only inside the encrypted message.
func (c *Client) UploadFile(ctx context.Context, data []byte, name, mime string) (MediaRef, error) {
	if len(data) > MaxFile {
		return MediaRef{}, ErrFileTooLarge
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return MediaRef{}, err
	}
	g, err := fileAEAD(key)
	if err != nil {
		return MediaRef{}, err
	}
	ct := g.Seal(nil, zeroNonce, data, nil)
	sum := sha256.Sum256(ct)
	var res mediaUploadRes
	if err := c.raw(ctx, http.MethodPost, "media/upload", "application/octet-stream", ct, &res, nil); err != nil {
		return MediaRef{}, err
	}
	// The homeserver's hash has to be of what was sent.
	if !bytes.Equal(res.SHA256, sum[:]) || res.Size != uint64(len(ct)) || len(res.ID) != 16 {
		return MediaRef{}, errors.New("client: the homeserver stored something other than the file")
	}
	return MediaRef{Server: c.Server(), ID: res.ID, Key: key, SHA256: sum[:], Size: uint64(len(data)), Name: name, MIME: mime}, nil
}

// DownloadFile fetches a file through the homeserver
// (GET media/{server}/{id}), checks its hash, and decrypts it (§14). A
// file past 30 days is ErrFileExpired, and one removed after a report
// ErrFileRemoved.
func (c *Client) DownloadFile(ctx context.Context, r MediaRef) ([]byte, error) {
	if err := r.Check(); err != nil {
		return nil, err
	}
	var ct []byte
	if err := c.raw(ctx, http.MethodGet, "media/"+r.Server+"/"+wire.EncodeB64(r.ID), "", nil, nil, &ct); err != nil {
		return nil, mediaError(err)
	}
	if sum := sha256.Sum256(ct); !bytes.Equal(sum[:], r.SHA256) {
		return nil, errors.New("client: the file doesn't match its hash")
	}
	g, err := fileAEAD(r.Key)
	if err != nil {
		return nil, err
	}
	plain, err := g.Open(nil, zeroNonce, ct, nil)
	if err != nil {
		return nil, errors.New("client: the file doesn't decrypt")
	}
	if uint64(len(plain)) != r.Size {
		return nil, errors.New("client: the file isn't the size its message says")
	}
	return plain, nil
}

func mediaError(err error) error {
	var e *wire.Error
	if errors.As(err, &e) && e.Status == http.StatusGone {
		if e.Code == "removed" {
			return ErrFileRemoved
		}
		return ErrFileExpired
	}
	return err
}

type mediaPublicRes struct {
	Media PublicMedia `json:"media"`
}

// UploadPublic uploads a public, unencrypted profile picture
// (POST media/upload-public, §13.1). mime is image/png, image/jpeg,
// image/webp, or image/gif.
func (c *Client) UploadPublic(ctx context.Context, data []byte, mime string) (PublicMedia, error) {
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return PublicMedia{}, errors.New("client: a profile picture is a PNG, JPEG, WebP, or GIF")
	}
	if len(data) > MaxPublicMedia {
		return PublicMedia{}, errors.New("client: a profile picture is at most 1 MB")
	}
	var res mediaPublicRes
	if err := c.raw(ctx, http.MethodPost, "media/upload-public", mime, data, &res, nil); err != nil {
		return PublicMedia{}, err
	}
	return res.Media, nil
}

// DownloadPublic fetches a profile picture and checks its hash and size.
func (c *Client) DownloadPublic(ctx context.Context, m PublicMedia) ([]byte, error) {
	if m.Server == "" || len(m.ID) != 16 || m.Size > MaxPublicMedia {
		return nil, errors.New("client: a malformed public-media")
	}
	var data []byte
	if err := c.raw(ctx, http.MethodGet, "media/"+m.Server+"/"+wire.EncodeB64(m.ID), "", nil, nil, &data); err != nil {
		return nil, mediaError(err)
	}
	if sum := sha256.Sum256(data); !bytes.Equal(sum[:], m.SHA256) || uint64(len(data)) != m.Size {
		return nil, errors.New("client: the picture doesn't match its hash")
	}
	return data, nil
}

// raw makes a call with a raw body, or that answers with raw bytes, with a
// session: an upload of contentType when body isn't nil, and a JSON answer
// into out or the raw answer into rawOut. It signs in again once after
// 401.
func (c *Client) raw(ctx context.Context, method, path, contentType string, body []byte, out any, rawOut *[]byte) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	if c.token() == "" {
		if err := c.SignIn(ctx); err != nil {
			return err
		}
	}
	err := c.rawOnce(ctx, method, path, contentType, body, out, rawOut)
	if wire.IsCode(err, wire.CodeUnauthenticated) {
		if err := c.SignIn(ctx); err != nil {
			return err
		}
		err = c.rawOnce(ctx, method, path, contentType, body, out, rawOut)
	}
	return err
}

func (c *Client) rawOnce(ctx context.Context, method, path, contentType string, body []byte, out any, rawOut *[]byte) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api.url(path, nil), rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if t := c.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	// Files take longer than JSON calls: allow 64 MiB at a slow pace.
	hc := *c.api.http
	hc.Timeout = 10 * time.Minute
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if c.api.log != nil {
		c.api.log.Debug("tieroute request", "method", method, "path", path, "status", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxFile+1024))
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &wire.Error{Status: resp.StatusCode}
		if err := wire.Unmarshal(data, e); err != nil || e.Code == "" {
			e.Code = statusCode(resp.StatusCode)
		}
		return e
	}
	if rawOut != nil {
		*rawOut = data
		return nil
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
			return fmt.Errorf("client: %s %s: unexpected content type %q", method, path, ct)
		}
		return wire.Unmarshal(data, out)
	}
	return nil
}
