package community

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// MaxSegment is the most plaintext one history segment holds: 1 MiB
// (§16.5).
const MaxSegment = 1 << 20

// segmentOverhead is what encryption adds: a 12-byte nonce and a 16-byte
// tag (§16.5).
const segmentOverhead = 28

func segmentAAD(community []byte, channel, start, end uint64) []byte {
	return wire.MustJCS([]any{historyLabel, wire.EncodeB64(community), channel, start, end})
}

// SegmentKey returns the AES-128-GCM key a history key gives (§16.5).
func SegmentKey(historyKey []byte) ([]byte, error) {
	return hkdf.Expand(sha256.New, historyKey, historyLabel, 16)
}

// SegmentAAD returns a segment's associated data (§16.5).
func SegmentAAD(community []byte, channel, start, end uint64) []byte {
	return segmentAAD(community, channel, start, end)
}

// EncryptSegment encrypts a segment's plaintext under a history key, with
// a fresh random nonce (§16.5).
func EncryptSegment(historyKey, community []byte, channel, start, end uint64, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return encryptSegmentNonce(historyKey, community, channel, start, end, plaintext, nonce)
}

func encryptSegmentNonce(historyKey, community []byte, channel, start, end uint64, plaintext, nonce []byte) ([]byte, error) {
	if len(plaintext) > MaxSegment {
		return nil, errors.New("community: a segment over 1 MiB")
	}
	g, err := gcmFor(historyKey, historyLabel)
	if err != nil {
		return nil, err
	}
	return g.Seal(bytes.Clone(nonce), nonce, plaintext, segmentAAD(community, channel, start, end)), nil
}

// DecryptSegment tries each history key until one opens the segment
// (§16.5, "Reading a segment"). It reports false when none does; the
// segment's whole range is then a gap.
func DecryptSegment(keys [][]byte, community []byte, channel, start, end uint64, ct []byte) ([]byte, bool) {
	if len(ct) < segmentOverhead {
		return nil, false
	}
	aad := segmentAAD(community, channel, start, end)
	for _, k := range keys {
		g, err := gcmFor(k, historyLabel)
		if err != nil {
			continue
		}
		if pt, err := g.Open(nil, ct[:12], ct[12:], aad); err == nil {
			return pt, true
		}
	}
	return nil, false
}

// History item types (§7.3).
const (
	ItemKey  = "key"
	ItemMsg  = "msg"
	ItemGone = "gone"
)

// HistoryKey is a history-key line: a msg-key as its device signed it.
type HistoryKey struct {
	T   string      `json:"t"`
	Key wire.Signed `json:"key"`
}

// HistoryMsg is a history-msg line: a signed app-message, the hash of the
// msg-key that signed it, and its sequence number (§7.3).
type HistoryMsg struct {
	T    string      `json:"t"`
	Chat wire.Bytes  `json:"chat"`
	Msg  wire.Signed `json:"msg"`
	MK   wire.Bytes  `json:"mk"`
	Seq  *uint64     `json:"seq,omitempty"`
}

// HistoryGone is a history-gone line: a deleted message's place, and the
// sequence number of the delete (§7.3).
type HistoryGone struct {
	T    string     `json:"t"`
	Chat wire.Bytes `json:"chat"`
	Seq  uint64     `json:"seq"`
	By   uint64     `json:"by"`
}

// MsgKeyHash names a msg-key by the SHA-256 of JCS of its signed object
// (§7.3).
func MsgKeyHash(s wire.Signed) []byte {
	h := sha256.Sum256(s.Canonical())
	return h[:]
}

// SegmentMsg is one message to put in a segment.
type SegmentMsg struct {
	Seq    uint64
	Msg    wire.Signed // the app-message
	MsgKey wire.Signed // the msg-key that signed it
	// Gone, when not zero, makes it a deleted message's place, with the
	// sequence number of the delete.
	Gone uint64
}

// BuildSegment writes a segment's plaintext: each msg-key as a
// history-key line before the first message it signed, then the messages,
// in seq order (§16.5).
func BuildSegment(group []byte, msgs []SegmentMsg) ([]byte, error) {
	var b bytes.Buffer
	seen := map[string]bool{}
	for _, m := range msgs {
		var line []byte
		var err error
		if m.Gone != 0 {
			line, err = wire.JCS(HistoryGone{T: ItemGone, Chat: group, Seq: m.Seq, By: m.Gone})
		} else {
			mk := MsgKeyHash(m.MsgKey)
			if !seen[string(mk)] {
				seen[string(mk)] = true
				kl, err := wire.JCS(HistoryKey{T: ItemKey, Key: m.MsgKey})
				if err != nil {
					return nil, err
				}
				b.Write(kl)
				b.WriteByte('\n')
			}
			seq := m.Seq
			line, err = wire.JCS(HistoryMsg{T: ItemMsg, Chat: group, Msg: m.Msg, MK: mk, Seq: &seq})
		}
		if err != nil {
			return nil, err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if b.Len() > MaxSegment {
		return nil, errors.New("community: a segment over 1 MiB")
	}
	return b.Bytes(), nil
}

// ReadItem is one item a reader kept from a segment.
type ReadItem struct {
	Seq    uint64
	Msg    *wire.Signed // the app-message, for a message
	MsgKey *wire.Signed // the msg-key that signed it
	Gone   uint64       // for a deleted message: the delete's sequence number
}

// ReadSegment checks a segment's lines (§16.5, "Reading a segment"): only
// key, msg, and gone lines; each msg naming the group that carries the
// channel, a msg-key on an earlier line, and seq in range; the first
// item for each seq standing. It doesn't verify signatures: the caller
// checks each message against its msg-key and the sender's chain, as
// imported history is checked (§7.3). A plaintext that isn't UTF-8 gives
// nothing.
func ReadSegment(plaintext, group []byte, start, end uint64) ([]ReadItem, error) {
	if !utf8.Valid(plaintext) {
		return nil, errors.New("community: a segment that isn't UTF-8")
	}
	keys := map[string]wire.Signed{}
	seen := map[uint64]bool{}
	var out []ReadItem
	for _, line := range strings.Split(string(plaintext), "\n") {
		if line == "" {
			continue
		}
		var head struct {
			T string `json:"t"`
		}
		if wire.Unmarshal([]byte(line), &head) != nil {
			continue
		}
		switch head.T {
		case ItemKey:
			var k HistoryKey
			if wire.Unmarshal([]byte(line), &k) != nil || k.Key.T != "msg-key" {
				continue
			}
			keys[string(MsgKeyHash(k.Key))] = k.Key
		case ItemMsg:
			var m HistoryMsg
			if wire.Unmarshal([]byte(line), &m) != nil || m.Seq == nil || !bytes.Equal(m.Chat, group) {
				continue
			}
			mk, ok := keys[string(m.MK)]
			if !ok || *m.Seq < start || *m.Seq > end || seen[*m.Seq] {
				continue
			}
			seen[*m.Seq] = true
			msg := m.Msg
			out = append(out, ReadItem{Seq: *m.Seq, Msg: &msg, MsgKey: &mk})
		case ItemGone:
			var g HistoryGone
			if wire.Unmarshal([]byte(line), &g) != nil || !bytes.Equal(g.Chat, group) {
				continue
			}
			if g.Seq < start || g.Seq > end || seen[g.Seq] {
				continue
			}
			seen[g.Seq] = true
			out = append(out, ReadItem{Seq: g.Seq, Gone: g.By})
		}
	}
	return out, nil
}
