// Package vectors loads the shared Tieroute test vectors in
// internal/tieroute/testdata/vectors for tests.
package vectors

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Dir returns the directory that holds the vector files.
func Dir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "vectors")
}

// Load decodes the vector file name, relative to Dir, into v.
func Load(t testing.TB, name string, v any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(Dir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// Hex decodes a hex string, failing the test on error.
func Hex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Signed is a signed object as the vector files give it.
type Signed struct {
	Type         string `json:"type"`
	Body         string `json:"body"`
	SigInput     string `json:"sig_input"`
	SignatureHex string `json:"signature_hex"`
	Signed       string `json:"signed"`
	SignedSHA256 string `json:"signed_sha256"`
}

// Key is a composite key as the vector files give it.
type Key struct {
	Ed25519Public   string `json:"ed25519_public"`
	Ed25519SeedHex  string `json:"ed25519_seed_hex"`
	MLDSASeedHex    string `json:"mldsa_seed_hex"`
	RecoverySeedHex string `json:"recovery_seed_hex"`
	PublicHex       string `json:"public_hex"`
	Name            string `json:"name"`
}
