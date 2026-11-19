package opus

import (
	"math"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	enc, err := NewEncoder(1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewDecoder(1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm := make([]int16, FrameSize)
	out := make([]int16, FrameSize)
	var in, got float64
	for f := range 50 {
		for i := range pcm {
			pcm[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(f*FrameSize+i)/SampleRate))
		}
		pkt, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		n, err := dec.Decode(pkt, out)
		if err != nil || n != FrameSize {
			t.Fatal(n, err)
		}
		if f >= 10 {
			for i := range pcm {
				in += float64(pcm[i]) * float64(pcm[i])
				got += float64(out[i]) * float64(out[i])
			}
		}
	}
	if r := got / in; r < 0.5 || r > 2 {
		t.Fatalf("energy ratio %.2f", r)
	}
	if _, err := enc.Encode(pcm[:10]); err == nil {
		t.Fatal("a short frame encoded")
	}
	if n, err := dec.Decode(nil, out); err != nil || n != FrameSize {
		t.Fatal("concealment", n, err)
	}
}
