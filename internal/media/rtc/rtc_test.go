package rtc

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/tieroute/sframe"
)

type ring struct{ base []byte }

func (r ring) Key(kid uint64) (string, *sframe.Key, bool) {
	k, err := sframe.Derive(r.base, kid)
	return "peer", k, err == nil
}

// TestCall connects two peers on this machine: one offers the three
// sections, the other answers with its voice track, and each hears the
// other's tone through SFrame.
func TestCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o := Options{Loopback: true}
	api, err := NewAPI(o)
	if err != nil {
		t.Fatal(err)
	}
	base := make([]byte, 16)
	rand.Read(base)
	mk := func(hz float64, kid uint64) *Media {
		m, err := NewMedia(&audio.Tone{Hz: hz, Volume: 0.3}, audio.NewMixer(), ring{base})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := sframe.NewSender(base, kid)
		m.SetSender(s)
		go m.Run(ctx)
		return m
	}
	ma, mb := mk(440, 0x10), mk(660, 0x21)
	var mu sync.Mutex
	var aICE, bICE []string
	var a, b *Peer
	a, err = NewPeer(api, o, func(c Candidate) {
		mu.Lock()
		defer mu.Unlock()
		if c.Candidate != "" {
			aICE = append(aICE, `{"candidate":"`+c.Candidate+`","sdpMid":"0","sdpMLineIndex":0}`)
		}
	}, func(tr *webrtc.TrackRemote, kind string) { go ma.Receive(tr, kind) })
	if err != nil {
		t.Fatal(err)
	}
	b, err = NewPeer(api, o, func(c Candidate) {
		mu.Lock()
		defer mu.Unlock()
		if c.Candidate != "" {
			bICE = append(bICE, `{"candidate":"`+c.Candidate+`","sdpMid":"0","sdpMLineIndex":0}`)
		}
	}, func(tr *webrtc.TrackRemote, kind string) { go mb.Receive(tr, kind) })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if err := a.AddSections(ma.Track); err != nil {
		t.Fatal(err)
	}
	offer, err := a.Offer()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(offer, "m=audio") != 2 || strings.Count(offer, "m=video") != 1 {
		t.Fatalf("the offer's sections: %s", offer)
	}
	answer, err := b.Remote(offer, mb.Track)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Remote(answer, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	mu.Lock()
	for _, c := range aICE {
		b.AddCandidate(c)
	}
	for _, c := range bICE {
		a.AddCandidate(c)
	}
	mu.Unlock()
	if err := a.Connected(ctx, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	ra, rb := audio.Record(ma.Mix), audio.Record(mb.Mix)
	time.Sleep(2 * time.Second)
	ra.Stop()
	rb.Stop()
	sa, sb := ra.Samples(), rb.Samples()
	if audio.Power(sa, 660) < 50*audio.Power(sa, 440) || audio.Power(sb, 440) < 50*audio.Power(sb, 660) {
		t.Fatalf("a heard %g of 660 and %g of 440; b heard %g of 440 and %g of 660",
			audio.Power(sa, 660), audio.Power(sa, 440), audio.Power(sb, 440), audio.Power(sb, 660))
	}
	if ma.Speaking()["peer"] == 0 {
		t.Fatal("no level for the peer")
	}
	mb.SetMuted(true)
	time.Sleep(500 * time.Millisecond)
	ra = audio.Record(ma.Mix)
	time.Sleep(time.Second)
	ra.Stop()
	if p := audio.Power(ra.Samples(), 660); p > 0.001 {
		t.Fatalf("muted, b still sends %g", p)
	}
}
