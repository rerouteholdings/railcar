package community

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Labels of the community keys' derivations (§16.2.4, §16.5).
const (
	metaLabel    = "tieroute community metadata"
	historyLabel = "tieroute community history"
)

// What a sealed value belongs to, in its associated data (§16.2.4).
const (
	WhatRole    = "role"
	WhatChannel = "channel"
)

// Fields of a sealed value (§16.2.4).
const (
	FieldName  = "name"
	FieldTopic = "topic"
	FieldColor = "color"
)

// KeyHash names a metadata key or history key by its SHA-256.
func KeyHash(key []byte) []byte {
	h := sha256.Sum256(key)
	return h[:]
}

// gcmFor derives an AES-128-GCM from a 32-byte key with HKDF-Expand,
// using the key itself as the pseudorandom key (§16.2.4, §16.5).
func gcmFor(key []byte, label string) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("community: a key that isn't 32 bytes")
	}
	k, err := hkdf.Expand(sha256.New, key, label, 16)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func sealAAD(community []byte, seq uint64, what string, id uint64, field string) []byte {
	return wire.MustJCS([]any{metaLabel, wire.EncodeB64(community), seq, what, id, field})
}

// Seal encrypts a name, topic, or color under a metadata key, for the
// entry at seq (§16.2.4).
func Seal(key, community []byte, seq uint64, what string, id uint64, field, value string) (Sealed, error) {
	g, err := gcmFor(key, metaLabel)
	if err != nil {
		return Sealed{}, err
	}
	nonce := make([]byte, 12)
	rand.Read(nonce)
	ct := g.Seal(nonce, nonce, []byte(value), sealAAD(community, seq, what, id, field))
	if len(ct) > MaxSealedCT {
		return Sealed{}, errors.New("community: the value is too long to seal")
	}
	return Sealed{Key: KeyHash(key), CT: ct}, nil
}

// Open decrypts a sealed value with whichever of keys it names. It
// reports false when none is named, or the value doesn't decrypt; the
// client then shows no name (§16.2.4).
func Open(keys [][]byte, s *Sealed, community []byte, seq uint64, what string, id uint64, field string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, k := range keys {
		if !bytes.Equal(KeyHash(k), s.Key) {
			continue
		}
		g, err := gcmFor(k, metaLabel)
		if err != nil || len(s.CT) < 12+16 {
			return "", false
		}
		pt, err := g.Open(nil, s.CT[:12], s.CT[12:], sealAAD(community, seq, what, id, field))
		if err != nil {
			return "", false
		}
		return string(pt), true
	}
	return "", false
}
