// Package sig implements Tieroute's signatures: strict Ed25519 (§4.3), the
// composite ML-DSA-65 + Ed25519 signature MLDSA65-Ed25519-SHA512 (§3.2.1),
// and signing and verifying the signed envelope (§3.2.2).
package sig

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"math/big"
)

// ErrSignature reports a signature that doesn't verify.
var ErrSignature = errors.New("sig: signature doesn't verify")

// The seven small-order encodings, compared with the top bit of the last
// byte cleared (§4.3, the libsodium list).
var smallOrder = [7][32]byte{
	{},
	{0x01},
	{0x26, 0xe8, 0x95, 0x8f, 0xc2, 0xb2, 0x27, 0xb0, 0x45, 0xc3, 0xf4, 0x89, 0xf2, 0xef, 0x98, 0xf0, 0xd5, 0xdf, 0xac, 0x05, 0xd3, 0xc6, 0x33, 0x39, 0xb1, 0x38, 0x02, 0x88, 0x6d, 0x53, 0xfc, 0x05},
	{0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f, 0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f, 0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6, 0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a},
	{0xec, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xed, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	{0xee, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
}

// p = 2²⁵⁵ − 19, and L, the order of the base point.
var (
	fieldP     = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	groupOrder = func() *big.Int {
		l, _ := new(big.Int).SetString("7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)
		return l
	}()
)

// leInt reads 32 little-endian bytes, with the top bit of the last byte
// cleared when clearTop is set.
func leInt(b []byte, clearTop bool) *big.Int {
	be := make([]byte, 32)
	for i := range 32 {
		be[31-i] = b[i]
	}
	if clearTop {
		be[0] &= 0x7f
	}
	return new(big.Int).SetBytes(be)
}

// ValidPoint reports whether a 32-byte point encoding passes §4.3: it isn't
// one of the small-order encodings, and its y coordinate, with the top bit
// cleared, is less than p.
func ValidPoint(b []byte) bool {
	if len(b) != 32 {
		return false
	}
	var c [32]byte
	copy(c[:], b)
	c[31] &= 0x7f
	for _, so := range smallOrder {
		if c == so {
			return false
		}
	}
	return leInt(b, true).Cmp(fieldP) < 0
}

// ValidPublicKey reports whether pub is a valid Ed25519 public key under
// §4.3: a canonical encoding of a curve point that isn't of small order.
// A bad key is refused when it's certified, not when it first signs.
func ValidPublicKey(pub []byte) bool {
	return ValidPoint(pub) && decodesAsPoint(pub)
}

// VerifyEd25519 checks an Ed25519 signature with every rule of §4.3: S less
// than L, A and R valid points (canonical, not small order), and the
// cofactorless equation, which Go's verifier checks.
func VerifyEd25519(pub, msg, signature []byte) error {
	if len(pub) != 32 || len(signature) != 64 {
		return ErrSignature
	}
	if !ValidPoint(pub) || !ValidPoint(signature[:32]) {
		return ErrSignature
	}
	if leInt(signature[32:], false).Cmp(groupOrder) >= 0 {
		return ErrSignature
	}
	if !ed25519.Verify(pub, msg, signature) {
		return ErrSignature
	}
	return nil
}

// decodesAsPoint checks that y² is such that x exists: x² = (y² − 1) /
// (d·y² + 1) has a square root mod p.
func decodesAsPoint(b []byte) bool {
	y := leInt(b, true)
	if y.Cmp(fieldP) >= 0 {
		return false
	}
	p := fieldP
	one := big.NewInt(1)
	d := new(big.Int).Mod(new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), p)), p)
	y2 := new(big.Int).Mod(new(big.Int).Mul(y, y), p)
	u := new(big.Int).Mod(new(big.Int).Sub(y2, one), p)
	v := new(big.Int).Mod(new(big.Int).Add(new(big.Int).Mul(d, y2), one), p)
	x2 := new(big.Int).Mod(new(big.Int).Mul(u, new(big.Int).ModInverse(v, p)), p)
	if x2.Sign() == 0 {
		// x = 0 is a point only with the sign bit clear.
		return b[31]&0x80 == 0
	}
	return new(big.Int).ModSqrt(x2, p) != nil
}

// SameKey reports whether two keys are byte-for-byte equal.
func SameKey(a, b []byte) bool { return bytes.Equal(a, b) }
