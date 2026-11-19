// Package store keeps a Railcar profile on disk: one directory per
// profile, holding records encrypted at rest with AES-256-GCM under a
// 32-byte key kept in the operating system's keychain.
//
// When there's no keychain, Open and Create fail with ErrNoKeychain unless
// the caller allows a key file, which they then report as a warning: a key
// file beside the data protects nothing against someone who can read the
// directory.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name profile keys are stored under.
const Service = "railcar"

const (
	metaFile   = "profile.json"
	keyFile    = "profile.key"
	sealedExt  = ".sealed"
	format     = 1
	keySize    = 32
	nonceSize  = 12
	keychainOn = "keychain"
	keyFileOn  = "file"
)

// KeyFileWarning is the warning a profile carries when its key is in a
// file instead of the keychain.
const KeyFileWarning = "There's no system keychain, so this profile's key is stored in a file next to it. Anyone who can read the profile folder can read your keys."

var (
	// ErrNoKeychain reports that the system keychain isn't available.
	ErrNoKeychain = errors.New("store: the system keychain isn't available")
	// ErrNotFound reports a record that isn't stored.
	ErrNotFound = errors.New("store: no such record")
	// ErrExists reports a directory that already holds a profile.
	ErrExists = errors.New("store: a profile already exists there")
	// ErrNoProfile reports a directory that holds no profile.
	ErrNoProfile = errors.New("store: no profile there")
)

var recordName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// Options adjust how a profile's key is kept.
type Options struct {
	// AllowKeyFile stores the key in a file in the profile directory when
	// the keychain isn't available, instead of failing.
	AllowKeyFile bool
}

// Profile is an open profile directory.
type Profile struct {
	dir     string
	id      string
	key     []byte
	mode    string // keychainOn or keyFileOn
	Warning string // set when the key is in a file
}

type meta struct {
	Format int    `json:"format"`
	ID     string `json:"id"`
	Key    string `json:"key"` // "keychain" or "file"
}

// Create makes a new profile in dir, which must be empty or not exist yet,
// with a new random key.
func Create(dir string, opts Options) (*Profile, error) {
	if _, err := os.Stat(filepath.Join(dir, metaFile)); err == nil {
		return nil, ErrExists
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	idb := make([]byte, 16)
	key := make([]byte, keySize)
	if _, err := rand.Read(idb); err != nil {
		return nil, err
	}
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	p := &Profile{dir: dir, id: hex.EncodeToString(idb), key: key, mode: keychainOn}
	m := meta{Format: format, ID: p.id, Key: keychainOn}
	if err := keyring.Set(Service, p.account(), base64.StdEncoding.EncodeToString(key)); err != nil {
		if !opts.AllowKeyFile {
			return nil, fmt.Errorf("%w: %v", ErrNoKeychain, err)
		}
		if err := writeAtomic(filepath.Join(dir, keyFile), []byte(base64.StdEncoding.EncodeToString(key))); err != nil {
			return nil, err
		}
		m.Key, p.mode, p.Warning = keyFileOn, keyFileOn, KeyFileWarning
	}
	b, _ := json.Marshal(m)
	if err := writeAtomic(filepath.Join(dir, metaFile), b); err != nil {
		return nil, err
	}
	return p, nil
}

// Exists reports whether dir holds a profile.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, metaFile))
	return err == nil
}

// Open opens the profile in dir and gets its key.
func Open(dir string, opts Options) (*Profile, error) {
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoProfile
	}
	if err != nil {
		return nil, err
	}
	var m meta
	if err := json.Unmarshal(b, &m); err != nil || m.ID == "" {
		return nil, fmt.Errorf("store: %s: damaged profile", dir)
	}
	if m.Format != format {
		return nil, fmt.Errorf("store: %s: profile format %d isn't supported", dir, m.Format)
	}
	p := &Profile{dir: dir, id: m.ID, mode: m.Key}
	var enc string
	switch m.Key {
	case keychainOn:
		enc, err = keyring.Get(Service, p.account())
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNoKeychain, err)
		}
	case keyFileOn:
		if !opts.AllowKeyFile {
			return nil, fmt.Errorf("%w: the profile's key is in a file", ErrNoKeychain)
		}
		kb, err := os.ReadFile(filepath.Join(dir, keyFile))
		if err != nil {
			return nil, err
		}
		enc, p.Warning = string(kb), KeyFileWarning
	default:
		return nil, fmt.Errorf("store: %s: damaged profile", dir)
	}
	p.key, err = base64.StdEncoding.DecodeString(enc)
	if err != nil || len(p.key) != keySize {
		return nil, fmt.Errorf("store: %s: the profile key is damaged", dir)
	}
	return p, nil
}

func (p *Profile) account() string { return "profile-" + p.id }

// Dir returns the profile's directory.
func (p *Profile) Dir() string { return p.dir }

func (p *Profile) path(name string) (string, error) {
	if !recordName.MatchString(name) {
		return "", fmt.Errorf("store: bad record name %q", name)
	}
	return filepath.Join(p.dir, name+sealedExt), nil
}

func (p *Profile) aead() cipher.AEAD {
	block, err := aes.NewCipher(p.key)
	if err != nil {
		panic(err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return g
}

// The associated data binds a record to its profile and name, so records
// can't be swapped between names or profiles.
func (p *Profile) ad(name string) []byte {
	return []byte("railcar profile record\x00" + p.id + "\x00" + name)
}

// Save encrypts v as JSON and writes it as record name, atomically.
func (p *Profile) Save(name string, v any) error {
	path, err := p.path(name)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(v)
	if err != nil {
		return err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := p.aead().Seal(nonce, nonce, plain, p.ad(name))
	clear(plain)
	return writeAtomic(path, sealed)
}

// Load reads and decrypts record name into v.
func (p *Profile) Load(name string, v any) error {
	path, err := p.path(name)
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(sealed) < nonceSize {
		return fmt.Errorf("store: record %s is damaged", name)
	}
	plain, err := p.aead().Open(nil, sealed[:nonceSize], sealed[nonceSize:], p.ad(name))
	if err != nil {
		return fmt.Errorf("store: record %s doesn't decrypt", name)
	}
	defer clear(plain)
	return json.Unmarshal(plain, v)
}

// Delete removes record name. A record that isn't there is no error.
func (p *Profile) Delete(name string) error {
	path, err := p.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Destroy deletes the profile: its key from the keychain, and its
// directory.
func (p *Profile) Destroy() error {
	if p.mode == keychainOn {
		if err := keyring.Delete(Service, p.account()); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	clear(p.key)
	return os.RemoveAll(p.dir)
}

// writeAtomic writes data to path through a temporary file in the same
// directory, synced and renamed into place, so a crash leaves either the
// old file or the new one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort: not every platform syncs directories
		d.Close()
	}
	return nil
}
