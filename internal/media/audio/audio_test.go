package audio

import (
	"context"
	"testing"
	"time"
)

func TestMixerAndTone(t *testing.T) {
	m := NewMixer()
	a, b := &Tone{Hz: 440, Volume: 0.3}, &Tone{Hz: 660, Volume: 0.3}
	ctx := context.Background()
	f := make([]int16, Frame)
	out := make([]int16, Frame)
	var got []int16
	for range 20 {
		a.Read(ctx, f)
		m.Push("a", f)
		b.Read(ctx, f)
		m.Push("b", f)
		m.Pull(out)
		got = append(got, out...)
	}
	if Power(got, 440) < 100*Power(got, 880) || Power(got, 660) < 100*Power(got, 880) {
		t.Fatal("both tones should be in the mix")
	}
	if l := m.Levels(); l["a"] < 0.1 || l["b"] < 0.1 {
		t.Fatal(l)
	}
	m.SetDeafened(true)
	m.Push("a", f)
	m.Pull(out)
	if Level(out) != 0 {
		t.Fatal("deafened, the mix should be silent")
	}
}

func TestTonePacing(t *testing.T) {
	tone := &Tone{Hz: 440, Volume: 0.1}
	f := make([]int16, Frame)
	start := time.Now()
	for range 10 {
		tone.Read(context.Background(), f)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("10 frames in %v", d)
	}
}
