package chat

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/trustelem/zxcvbn"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
	"github.com/rerouteholdings/railcar/internal/tieroute/community"
	"github.com/rerouteholdings/railcar/internal/tieroute/store"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// The encrypted message history backup (§7.5). It's off until the user
// turns it on with a passphrase. The device that turns it on is the
// backup device, named in the account data: it uploads a snapshot of the
// history it holds, then a segment of what's new once a day, or sooner
// once 1 MiB has built up. Another device that turns it on takes over.

// recBackupPending holds what the backup hasn't got yet.
const recBackupPending = "bpending"

// backupEvery is how often the backup device uploads what's new.
const backupEvery = 24 * 60 * 60

// BackupState is the backup as this device knows it.
type BackupState struct {
	// On is true while this device is the backup device.
	On bool `json:"on"`
	// Seq is the newest segment it uploaded.
	Seq uint64 `json:"seq"`
	// Last is when it last uploaded, in Unix seconds.
	Last int64 `json:"last"`
	// Pending counts the messages waiting for the next segment.
	Pending int `json:"pending,omitempty"`
}

// pendingLine is a message the backup hasn't got yet.
type pendingLine struct {
	Kind string      `json:"kind"`
	Key  string      `json:"key"` // the other person, or the group
	Name string      `json:"name,omitempty"`
	Msg  wire.Signed `json:"msg"`
	MK   wire.Bytes  `json:"mk"`
}

// ErrWeakPassphrase is what EnableBackup returns for a passphrase the
// zxcvbn estimator scores under 4 (§7.5).
var ErrWeakPassphrase = errors.New("chat: that passphrase is too easy to guess. Use six or more random words")

// PassphraseStrong reports whether a backup passphrase is strong enough:
// a zxcvbn score of 4 (§7.5).
func PassphraseStrong(passphrase string, handle string) bool {
	return zxcvbn.PasswordStrength(passphrase, []string{handle}).Score >= 4
}

// EnableBackup turns the backup on with a passphrase, makes this device
// the backup device, and uploads a snapshot of the history it holds
// (§7.5).
func (e *Engine) EnableBackup(ctx context.Context, passphrase string) error {
	if !PassphraseStrong(passphrase, e.c.Handle()) {
		return ErrWeakPassphrase
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.c.AccountKey()) != 16 {
		return errors.New("chat: this device doesn't have the account key yet")
	}
	if err := e.c.SetPassphrase(ctx, passphrase); err != nil {
		return err
	}
	segs, err := e.c.ListBackup(ctx)
	if err != nil {
		return err
	}
	seq := uint64(0)
	if len(segs) > 0 {
		seq = segs[len(segs)-1].Seq + 1
	}
	data, err := e.exportHistory()
	if err != nil {
		return err
	}
	last, err := e.uploadSegments(ctx, seq, data, true)
	if err != nil {
		return err
	}
	e.st.Backup = &BackupState{On: true, Seq: last, Last: e.c.Clock().Now().Unix()}
	e.profile().Delete(recBackupPending)
	if err := e.save(); err != nil {
		return err
	}
	return e.recordBackup(ctx, true)
}

// uploadSegments uploads data as segments from seq on, split between
// lines at 1 MiB. With snapshot, the first is a snapshot; data then
// starts with the header (§7.5). It returns the last seq used.
func (e *Engine) uploadSegments(ctx context.Context, seq uint64, data []byte, snapshot bool) (uint64, error) {
	first := true
	last := seq
	for len(data) > 0 {
		n := len(data)
		if n > client.MaxSegment {
			n = bytes.LastIndexByte(data[:client.MaxSegment], '\n') + 1
			if n <= 0 {
				return last, errors.New("chat: a history line over 1 MiB")
			}
		}
		if err := e.c.PutBackup(ctx, seq, data[:n], snapshot && first); err != nil {
			if wire.IsCode(err, wire.CodeOverQuota) {
				return last, errors.New("chat: the backup is full on your homeserver")
			}
			return last, err
		}
		last, first = seq, false
		seq++
		data = data[n:]
	}
	return last, nil
}

// recordBackup names this device as the backup device in the account
// data, with its newest segment, or with on false, takes the backup out
// of it. It merges with what other devices wrote (§7.4). The caller
// holds e.mu.
func (e *Engine) recordBackup(ctx context.Context, on bool) error {
	for range 5 {
		d, err := e.c.GetAccountData(ctx)
		if err != nil {
			return err
		}
		if on {
			d.Backup = &client.BackupPos{Device: e.c.DeviceKey(), Seq: e.st.Backup.Seq}
		} else {
			d.Backup = nil
		}
		err = e.c.PutAccountData(ctx, d)
		if !wire.IsCode(err, wire.CodeStaleVersion) {
			return err
		}
	}
	return errors.New("chat: other devices kept changing the account data")
}

// keepForBackup notes a message for the backup's next segment, on the
// backup device. The caller holds e.mu.
func (e *Engine) keepForBackup(ch *Chat, msg wire.Signed, mk []byte) {
	if e.st.Backup == nil || !e.st.Backup.On {
		return
	}
	l := e.backupPending()
	p := pendingLine{Kind: ch.Kind, Key: ch.ID, Name: ch.Name, Msg: msg, MK: mk}
	if ch.Kind == KindDirect {
		p.Key, p.Name = ch.Peer, ""
	}
	l = append(l, p)
	e.st.Backup.Pending = len(l)
	if err := e.profile().Save(recBackupPending, l); err != nil {
		e.log.Info("chat: the backup's next segment", "err", err)
	}
}

func (e *Engine) backupPending() []pendingLine {
	var l []pendingLine
	if err := e.profile().Load(recBackupPending, &l); err != nil && !errors.Is(err, store.ErrNotFound) {
		e.log.Info("chat: the backup's next segment", "err", err)
	}
	return l
}

// BackupNow uploads what the backup hasn't got yet, on the backup
// device. If another device took the backup over, this one stops
// writing it.
func (e *Engine) BackupNow(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.backupNow(ctx)
}

func (e *Engine) backupNow(ctx context.Context) error {
	if e.st.Backup == nil || !e.st.Backup.On {
		return errors.New("chat: this device doesn't write the backup")
	}
	d, err := e.c.GetAccountData(ctx)
	if err != nil {
		return err
	}
	if d.Backup == nil || !bytes.Equal(d.Backup.Device, e.c.DeviceKey()) {
		e.st.Backup.On = false
		e.profile().Delete(recBackupPending)
		e.save()
		return errors.New("chat: another device writes the backup now")
	}
	pending := e.backupPending()
	if len(pending) == 0 {
		e.st.Backup.Last = e.c.Clock().Now().Unix()
		return e.save()
	}
	data, err := e.segmentLines(pending)
	if err != nil {
		return err
	}
	last, err := e.uploadSegments(ctx, e.st.Backup.Seq+1, data, false)
	if err != nil {
		return err
	}
	e.st.Backup.Seq, e.st.Backup.Last, e.st.Backup.Pending = last, e.c.Clock().Now().Unix(), 0
	e.profile().Delete(recBackupPending)
	if err := e.save(); err != nil {
		return err
	}
	return e.recordBackup(ctx, true)
}

// segmentLines writes pending messages as history lines: each chat and
// msg-key before the first message that needs it (§7.3).
func (e *Engine) segmentLines(pending []pendingLine) ([]byte, error) {
	var b bytes.Buffer
	line := func(v any) error {
		j, err := wire.JCS(v)
		if err != nil {
			return err
		}
		b.Write(j)
		b.WriteByte('\n')
		return nil
	}
	keys := e.msgKeys()
	chats, seen := map[string]bool{}, map[string]bool{}
	for _, p := range pending {
		mk, ok := keys[hex.EncodeToString(p.MK)]
		if !ok {
			continue
		}
		if !chats[p.Key] {
			chats[p.Key] = true
			if err := line(historyChat{T: "chat", Chat: p.Key, Kind: p.Kind, Name: p.Name}); err != nil {
				return nil, err
			}
		}
		if k := hex.EncodeToString(p.MK); !seen[k] {
			seen[k] = true
			if err := line(community.HistoryKey{T: community.ItemKey, Key: mk}); err != nil {
				return nil, err
			}
		}
		if err := line(historyMsg{T: community.ItemMsg, Chat: p.Key, Msg: p.Msg, MK: p.MK}); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// maybeBackup uploads what's new once a day, or sooner once 1 MiB has
// built up. The caller holds e.mu.
func (e *Engine) maybeBackup(ctx context.Context) {
	if e.st.Backup == nil || !e.st.Backup.On {
		return
	}
	due := e.c.Clock().Now().Unix()-e.st.Backup.Last >= backupEvery
	if !due {
		// A message line is a few hundred bytes at least, so 1 MiB
		// needs about a thousand.
		if e.st.Backup.Pending < 1000 {
			return
		}
		if data, err := e.segmentLines(e.backupPending()); err != nil || len(data) < client.MaxSegment {
			return
		}
	}
	if err := e.backupNow(ctx); err != nil {
		e.log.Info("chat: the backup", "err", err)
		// Try again in an hour.
		e.st.Backup.Last = e.c.Clock().Now().Unix() - backupEvery + 3600
	}
}

// DisableBackup turns the backup off: it deletes the passphrase copy of
// the account key, and takes the backup out of the account data. The
// segments stay on the homeserver until another device turns the backup
// on and compacts them.
func (e *Engine) DisableBackup(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.c.RemovePassphrase(ctx); err != nil {
		return err
	}
	if e.st.Backup != nil {
		e.st.Backup.On = false
	}
	e.profile().Delete(recBackupPending)
	if err := e.save(); err != nil {
		return err
	}
	return e.recordBackup(ctx, false)
}

// BackupInfo describes the backup.
type BackupInfo struct {
	// On is true when the account data names a backup device, and This
	// when it's this device.
	On, This bool
	// Seq is the newest segment the backup device recorded, and Stored
	// how many segments the homeserver lists.
	Seq    uint64
	Stored int
}

// Backup reports the backup's state, from the account data and the
// homeserver's list.
func (e *Engine) Backup(ctx context.Context) (BackupInfo, error) {
	var info BackupInfo
	d, err := e.c.GetAccountData(ctx)
	if err != nil {
		return info, err
	}
	if d.Backup != nil {
		info.On, info.Seq = true, d.Backup.Seq
		info.This = bytes.Equal(d.Backup.Device, e.c.DeviceKey())
	}
	segs, err := e.c.ListBackup(ctx)
	if err != nil {
		return info, err
	}
	info.Stored = len(segs)
	return info, nil
}

// RestoreResult is what a restore found.
type RestoreResult struct {
	// Messages is how many messages passed the checks.
	Messages int
	// Missing is true when the homeserver lists fewer segments than the
	// account data recorded: it lost or withheld some (§7.5).
	Missing bool
}

// RestoreBackup reads the backup: the newest snapshot and every segment
// after it. It verifies each message as imported history, and puts the
// ones that pass in their chats, now or when this device joins them
// (§7.3, §7.5). A device without the account key opens its passphrase
// copy first.
func (e *Engine) RestoreBackup(ctx context.Context, passphrase string) (RestoreResult, error) {
	var res RestoreResult
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.c.AccountKey()) != 16 {
		if passphrase == "" {
			return res, errors.New("chat: enter the backup passphrase")
		}
		if err := e.c.UnlockWithPassphrase(ctx, passphrase); err != nil {
			return res, err
		}
	}
	d, err := e.c.GetAccountData(ctx)
	if err != nil {
		return res, err
	}
	segs, err := e.c.ListBackup(ctx)
	if err != nil {
		return res, err
	}
	from := 0
	for i, s := range segs {
		if s.Snapshot || s.Seq == 0 {
			from = i
		}
	}
	var data []byte
	newest := int64(-1)
	for _, s := range segs[from:] {
		pt, err := e.c.GetBackup(ctx, s.Seq)
		if err != nil {
			return res, err
		}
		data = append(data, pt...)
		if len(pt) > 0 && pt[len(pt)-1] != '\n' {
			data = append(data, '\n')
		}
		newest = int64(s.Seq)
	}
	if d.Backup != nil && newest < int64(d.Backup.Seq) {
		res.Missing = true
	}
	items := e.readExport(ctx, data)
	res.Messages = len(items)
	pending := append(e.loadImported(), items...)
	if err := e.profile().Save(recImported, pending); err != nil {
		return res, err
	}
	for _, ch := range e.st.Chats {
		e.applyImported(ctx, ch)
	}
	return res, nil
}
