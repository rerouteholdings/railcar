// Package sframe implements SFrame (RFC 9605) with the one cipher suite
// Tieroute uses, AES_128_GCM_SHA256_128 (0x0004), and the key IDs calls
// and voice channels use (§15.2, §16.4).
package sframe

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
)

// Suite is the cipher suite: AES_128_GCM_SHA256_128 (RFC 9605 §4.5).
const Suite uint16 = 0x0004

const (
	keyLen   = 16 // Nk
	nonceLen = 12 // Nn
	tagLen   = 16 // Nt
)

// ExporterLabel is the MLS exporter label for a call's base key (§15.2).
const ExporterLabel = "SFrame 1.0 Tieroute call"

// CallKID is a call's key ID: the sender's leaf index and the low 4 bits
// of the epoch (§15.2).
func CallKID(leafIndex uint32, epoch uint64) uint64 {
	return uint64(leafIndex)<<4 + epoch%16
}

// VoiceKID is a voice channel's key ID: the visit ID followed by an 8-bit
// generation (§16.4).
func VoiceKID(visit uint32, gen uint8) uint64 {
	return uint64(visit)<<8 | uint64(gen)
}

// Key is the key and salt for one KID (RFC 9605 §4.4.2).
type Key struct {
	aead cipher.AEAD
	salt [nonceLen]byte
}

// Derive derives the SFrame key and salt for kid from a base key.
func Derive(baseKey []byte, kid uint64) (*Key, error) {
	key, salt, err := DeriveBytes(baseKey, kid)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	k := &Key{aead: aead}
	copy(k.salt[:], salt)
	return k, nil
}

// DeriveBytes returns the sframe_key and sframe_salt for kid (RFC 9605
// §4.4.2).
func DeriveBytes(baseKey []byte, kid uint64) (key, salt []byte, err error) {
	secret, err := hkdf.Extract(sha256.New, baseKey, nil)
	if err != nil {
		return nil, nil, err
	}
	var info [8 + 2]byte
	binary.BigEndian.PutUint64(info[:8], kid)
	binary.BigEndian.PutUint16(info[8:], Suite)
	key, err = hkdf.Expand(sha256.New, secret, "SFrame 1.0 Secret key "+string(info[:]), keyLen)
	if err != nil {
		return nil, nil, err
	}
	salt, err = hkdf.Expand(sha256.New, secret, "SFrame 1.0 Secret salt "+string(info[:]), nonceLen)
	return key, salt, err
}

// Header encodes an SFrame header for kid and ctr (RFC 9605 §4.3).
func Header(kid, ctr uint64) []byte {
	h := []byte{0}
	if kid < 8 {
		h[0] |= byte(kid) << 4
	} else {
		n := minLen(kid)
		h[0] |= 0x80 | byte(n-1)<<4
		h = appendUint(h, kid, n)
	}
	if ctr < 8 {
		h[0] |= byte(ctr)
	} else {
		n := minLen(ctr)
		h[0] |= 0x08 | byte(n-1)
		h = appendUint(h, ctr, n)
	}
	return h
}

func minLen(v uint64) int {
	n := 1
	for v >= 1<<(8*n) && n < 8 {
		n++
	}
	return n
}

func appendUint(b []byte, v uint64, n int) []byte {
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// ParseHeader reads the header at the start of an SFrame ciphertext, and
// returns the KID, the CTR, and the header's length.
func ParseHeader(b []byte) (kid, ctr uint64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, 0, errors.New("sframe: empty")
	}
	c := b[0]
	n = 1
	read := func(extended bool, short byte) (uint64, error) {
		if !extended {
			return uint64(short), nil
		}
		l := int(short) + 1
		if len(b) < n+l {
			return 0, errors.New("sframe: short header")
		}
		var v uint64
		for _, x := range b[n : n+l] {
			v = v<<8 | uint64(x)
		}
		n += l
		return v, nil
	}
	if kid, err = read(c&0x80 != 0, (c>>4)&0x07); err != nil {
		return
	}
	ctr, err = read(c&0x08 != 0, c&0x07)
	return
}

func (k *Key) nonce(ctr uint64) []byte {
	var n [nonceLen]byte
	binary.BigEndian.PutUint64(n[nonceLen-8:], ctr)
	for i := range n {
		n[i] ^= k.salt[i]
	}
	return n[:]
}

// Seal encrypts one frame under kid and ctr, with metadata as extra
// authenticated data, and returns the header and ciphertext (RFC 9605
// §4.4.3). The caller never uses a KID and CTR pair twice.
func (k *Key) Seal(kid, ctr uint64, metadata, plaintext []byte) []byte {
	h := Header(kid, ctr)
	aad := append(append([]byte{}, h...), metadata...)
	return k.aead.Seal(h, k.nonce(ctr), plaintext, aad)
}

// Open decrypts a ciphertext whose header said ctr, given the header's
// length.
func (k *Key) Open(ct []byte, ctr uint64, headerLen int, metadata []byte) ([]byte, error) {
	if len(ct) < headerLen+tagLen {
		return nil, errors.New("sframe: too short")
	}
	aad := append(append([]byte{}, ct[:headerLen]...), metadata...)
	return k.aead.Open(nil, k.nonce(ctr), ct[headerLen:], aad)
}

// Sender encrypts a stream of frames under one key, counting CTR from 0
// (§15.2). It's safe for more than one goroutine.
type Sender struct {
	mu  sync.Mutex
	kid uint64
	key *Key
	ctr uint64
}

// NewSender returns a sender for kid, with a key derived from baseKey.
func NewSender(baseKey []byte, kid uint64) (*Sender, error) {
	k, err := Derive(baseKey, kid)
	if err != nil {
		return nil, err
	}
	return &Sender{kid: kid, key: k}, nil
}

// Seal encrypts the next frame, with empty metadata.
func (s *Sender) Seal(frame []byte) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.key.Seal(s.kid, s.ctr, nil, frame)
	s.ctr++
	return out
}

// KID returns the sender's key ID.
func (s *Sender) KID() uint64 { return s.kid }
