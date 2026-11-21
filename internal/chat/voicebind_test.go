package chat

import (
	"bytes"
	"testing"
)

// TestVoiceKeyBinding checks that a voice key or mute counts only from the
// identity and device voice-state lists for the visit (§16.4).
func TestVoiceKeyBinding(t *testing.T) {
	v := &voice{keys: map[string]map[uint64]*vkey{}, states: map[string][]vstate{}, mutes: map[string]map[uint32]VoiceMute{}}
	const chat = "c:c.test:3"
	v.states[chat] = []vstate{{ID: "kat", Dev: "D1", Visit: 7}}
	key := bytes.Repeat([]byte{1}, 32)
	v.takeKey(chat, "kat", "D2", VoiceKey{Visit: 7, Gen: 0, Key: key})
	if len(v.keys[chat]) != 0 {
		t.Fatal("a key from another device of the visit's identity counted")
	}
	v.takeKey(chat, "ravi", "D1", VoiceKey{Visit: 7, Gen: 0, Key: key})
	if len(v.keys[chat]) != 0 {
		t.Fatal("a key from another identity counted")
	}
	v.takeKey(chat, "kat", "D1", VoiceKey{Visit: 7, Gen: 0, Key: key})
	if k := v.keys[chat][7<<8]; k == nil || !k.seen {
		t.Fatal("the visit's own key didn't count")
	}
	// A key for a visit nobody lists yet is held, not seen.
	v.takeKey(chat, "lee", "D3", VoiceKey{Visit: 9, Gen: 0, Key: key})
	if k := v.keys[chat][9<<8]; k == nil || k.seen {
		t.Fatal("a held key")
	}

	v.takeMute(chat, "kat", "D2", VoiceMute{Visit: 7, Muted: true, TS: 1})
	if _, ok := v.mutes[chat][7]; ok {
		t.Error("a mute from another device counted")
	}
	v.takeMute(chat, "kat", "D1", VoiceMute{Visit: 7, Muted: true, TS: 1})
	if !v.mutes[chat][7].Muted {
		t.Error("the visit's own mute didn't count")
	}
}
