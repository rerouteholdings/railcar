package chat

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rerouteholdings/railcar/internal/testnet"
	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// The backup tests (§7.5). They're skipped unless RAILCAR_REROUTED names a
// rerouted binary, and the interop one unless RAILCAR_REROUTE_CLI names a
// reroute-cli binary.

const testPassphrase = "signal lantern gravel orchid tundra wicker"

// registerWithWords registers a Railcar identity, and returns it with its
// recovery words.
func registerWithWords(t *testing.T, o map[string]string, server, handle string) (*railcar, []string) {
	t.Helper()
	ctx := context.Background()
	c, words, err := client.Register(ctx, server, handle, testnet.SignupCode, filepath.Join(t.TempDir(), handle), client.Options{Origins: o})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if err := e.EnsureKeyPackages(ctx); err != nil {
		t.Fatal(err)
	}
	return &railcar{t: t, e: e}, words.Words()
}

// TestE2EInteropBackup: Railcar turns the backup on, and so does Reroute.
// Railcar recovers the Reroute identity, opens its passphrase copy, and
// restores its backup into the chat. Then Reroute recovers the Railcar
// identity, and restores its backup with the passphrase.
func TestE2EInteropBackup(t *testing.T) {
	bin := testnet.Rerouted(t)
	lee := newRerouteCLI(t, "lee")
	o := testnet.Start(t, bin, "a.test", "b.test")
	ctx := context.Background()
	kat, words := registerWithWords(t, o, "a.test", "kat")
	leeWords := cliWords(t, lee.run("v2", "register", "lee", "--server", o["b.test"], "--invite", testnet.SignupCode))
	if _, err := kat.e.Add(ctx, lastLine(lee.run("v2", "invite"))); err != nil {
		t.Fatal(err)
	}
	settle()
	lee.run("sync")
	lee.run("accept", "kat.a.test")
	ch := kat.waitChat("lee.b.test")
	lee.run("send", "kat.a.test", "hello Kat, from Lee")
	settle()
	kat.sync()
	kat.find(ch.ID, "hello Kat, from Lee")

	// A weak passphrase is refused (§7.5).
	if err := kat.e.EnableBackup(ctx, "password123"); err == nil {
		t.Fatal("a weak passphrase turned the backup on")
	}
	if err := kat.e.EnableBackup(ctx, testPassphrase); err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.SendText(ctx, ch.ID, "after the backup was on", ""); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.BackupNow(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := kat.e.Backup(ctx)
	if err != nil || !info.On || !info.This || info.Seq != 1 || info.Stored != 2 {
		t.Fatalf("the backup: %+v %v", info, err)
	}

	// Lee turns on Reroute's backup; Railcar recovers Lee's identity and
	// restores it.
	lee.run("sync")
	lee.run("send", "kat.a.test", "before Lee's backup")
	lee.run("backup-on", "--passphrase", testPassphrase)
	lee.run("backup-now")
	r := recoverRailcar(t, o, "b.test", "lee.b.test", leeWords)
	// The passphrase opens Reroute's passphrase copy of the account key.
	if err := r.e.Client().UnlockWithPassphrase(ctx, testPassphrase); err != nil {
		t.Fatal(err)
	}
	res, err := r.e.RestoreBackup(ctx, "")
	if err != nil || res.Messages == 0 || res.Missing {
		t.Fatalf("Railcar's restore of Reroute's backup: %+v %v", res, err)
	}
	kat.syncDevices()
	rch := r.waitChat("kat.a.test", func() { kat.sync() })
	r.find(rch.ID, "before Lee's backup")
	r.find(rch.ID, "after the backup was on")

	// Reroute recovers Kat's identity and restores the backup.
	desk := newRerouteCLI(t, "kat-desk")
	desk.run("v2", "recover", "kat.a.test", "--server", o["a.test"], "--words", strings.Join(words, " "))
	out := desk.run("backup-restore", "--passphrase", testPassphrase)
	out += desk.run("history", "lee.b.test")
	for _, s := range []string{"hello Kat, from Lee", "after the backup was on"} {
		if !strings.Contains(out, s) {
			t.Errorf("Reroute's restore doesn't have %q", s)
		}
	}
}

// TestE2ERailcarBackup: a Railcar identity's backup, restored by Railcar
// after a recovery, with what came after the snapshot.
func TestE2ERailcarBackup(t *testing.T) {
	o := testnet.Start(t, testnet.Rerouted(t), "a.test", "b.test")
	ctx := context.Background()
	kat, words := registerWithWords(t, o, "a.test", "kat")
	ravi := newRailcar(t, o, "b.test", "ravi")
	code, err := ravi.e.Invite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kat.e.Add(ctx, code); err != nil {
		t.Fatal(err)
	}
	settle()
	ravi.sync()
	if _, err := ravi.e.Accept(ctx, kat.e.Client().ID()); err != nil {
		t.Fatal(err)
	}
	ch := kat.waitChat("ravi.b.test")
	first, err := kat.e.SendText(ctx, ch.ID, "in the snapshot", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := kat.e.EnableBackup(ctx, testPassphrase); err != nil {
		t.Fatal(err)
	}
	if err := kat.e.Edit(ctx, ch.ID, first.ID, "in the snapshot, edited after"); err != nil {
		t.Fatal(err)
	}
	if _, err := ravi.e.SendText(ctx, ch.ID, "from Ravi, in a segment", ""); err != nil {
		t.Fatal(err)
	}
	settle()
	kat.sync()
	if err := kat.e.BackupNow(ctx); err != nil {
		t.Fatal(err)
	}

	r := recoverRailcar(t, o, "a.test", "kat.a.test", words)
	res, err := r.e.RestoreBackup(ctx, "")
	if err != nil || res.Missing || res.Messages != 3 {
		t.Fatalf("the restore: %+v %v", res, err)
	}
	ravi.syncDevices()
	rch := r.waitChat("ravi.b.test")
	if m := r.find(rch.ID, "in the snapshot, edited after"); !m.Edited {
		t.Errorf("the edit: %+v", m)
	}
	r.find(rch.ID, "from Ravi, in a segment")
	// The recovered device turns the backup on again and takes it over.
	if err := r.e.EnableBackup(ctx, testPassphrase); err != nil {
		t.Fatal(err)
	}
	if info, err := r.e.Backup(ctx); err != nil || !info.This {
		t.Fatalf("after taking over: %+v %v", info, err)
	}
}
