package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

type record struct {
	Secret string `json:"secret"`
	N      int    `json:"n"`
}

func TestProfileRoundTrip(t *testing.T) {
	keyring.MockInit()
	dir := filepath.Join(t.TempDir(), "kat")
	p, err := Create(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Warning != "" {
		t.Error("keychain profile has a warning")
	}
	if _, err := Create(dir, Options{}); !errors.Is(err, ErrExists) {
		t.Errorf("second Create: %v", err)
	}
	if err := p.Save("state", record{Secret: "hunter2-but-longer", N: 7}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.sealed"))
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("the record is stored in the clear")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.sealed")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	q, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := q.Load("state", &r); err != nil || r.N != 7 {
		t.Fatalf("%+v %v", r, err)
	}
	if err := q.Load("missing", &r); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	// A record renamed to another name doesn't decrypt.
	os.Rename(filepath.Join(dir, "state.sealed"), filepath.Join(dir, "other.sealed"))
	if err := q.Load("other", &r); err == nil {
		t.Error("a renamed record decrypted")
	}
	if err := q.Save("../escape", r); err == nil {
		t.Error("accepted a path as a record name")
	}
	if err := q.Destroy(); err != nil {
		t.Fatal(err)
	}
	if Exists(dir) {
		t.Error("the profile is still there")
	}
}

func TestKeyFileFallback(t *testing.T) {
	keyring.MockInitWithError(errors.New("no secret service"))
	defer keyring.MockInit()
	dir := t.TempDir()
	if _, err := Create(dir, Options{}); !errors.Is(err, ErrNoKeychain) {
		t.Fatalf("Create without a keychain: %v", err)
	}
	p, err := Create(dir, Options{AllowKeyFile: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.Warning != KeyFileWarning {
		t.Error("no warning")
	}
	if err := p.Save("state", record{N: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNoKeychain) {
		t.Error("opened a key-file profile without allowing it")
	}
	q, err := Open(dir, Options{AllowKeyFile: true})
	if err != nil || q.Warning == "" {
		t.Fatal(err)
	}
	var r record
	if err := q.Load("state", &r); err != nil || r.N != 1 {
		t.Fatal(err)
	}
}
