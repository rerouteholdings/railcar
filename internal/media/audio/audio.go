// Package audio moves 48 kHz, 16-bit, mono audio in frames of 20 ms
// between the machine's microphone and speakers and a call: a Source the
// call reads frames from, and a Mixer that adds up what everyone else
// sends, for the speakers or a Recorder to take.
package audio

import (
	"context"
	"math"
	"sync"
	"time"
)

// Audio format, as Opus uses it (§15.2).
const (
	SampleRate = 48000
	Frame      = SampleRate / 50 // samples in 20 ms
	FrameTime  = 20 * time.Millisecond
)

// Source gives a call its audio, one frame at a time.
type Source interface {
	// Read fills frame with the next Frame samples, waiting until they're
	// ready, or returns ctx's error.
	Read(ctx context.Context, frame []int16) error
}

// Level is a frame's loudness, from 0 to 1: its root mean square.
func Level(frame []int16) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		f := float64(s) / 32768
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// Tone is a Source that plays a sine wave in real time, for tests and
// the demo.
type Tone struct {
	Hz     float64
	Volume float64 // 0 to 1

	mu   sync.Mutex
	n    int
	next time.Time
}

// Read waits for the next 20 ms, then fills frame with the wave.
func (t *Tone) Read(ctx context.Context, frame []int16) error {
	t.mu.Lock()
	now := time.Now()
	if t.next.IsZero() || t.next.Before(now.Add(-200*time.Millisecond)) {
		t.next = now
	}
	wait := t.next.Sub(now)
	t.next = t.next.Add(FrameTime)
	n := t.n
	t.n += len(frame)
	t.mu.Unlock()
	if wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	for i := range frame {
		frame[i] = int16(t.Volume * 32767 * math.Sin(2*math.Pi*t.Hz*float64(n+i)/SampleRate))
	}
	return ctx.Err()
}

// Silence is a Source of silence in real time.
type Silence struct{ Tone }

// Mixer adds up the audio of several streams, such as each person in a
// call. Each stream has a short queue, which absorbs jitter. It's safe
// for more than one goroutine.
type Mixer struct {
	mu      sync.Mutex
	streams map[string]*stream
	muted   bool // deafened: Pull gives silence
}

type stream struct {
	queue   [][]int16
	playing bool // started: the queue filled up once
	level   float64
	heard   time.Time
}

const (
	startAfter = 2  // frames queued before a stream starts playing
	maxQueue   = 10 // 200 ms; older frames are dropped past this
)

// NewMixer returns an empty mixer.
func NewMixer() *Mixer { return &Mixer{streams: map[string]*stream{}} }

// Push queues one frame of a stream.
func (m *Mixer) Push(id string, frame []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[id]
	if s == nil {
		s = &stream{}
		m.streams[id] = s
	}
	if len(s.queue) >= maxQueue {
		s.queue = s.queue[1:]
	}
	s.queue = append(s.queue, append([]int16(nil), frame...))
	l := Level(frame)
	s.level = math.Max(l, s.level*0.7)
	s.heard = time.Now()
}

// Remove forgets a stream.
func (m *Mixer) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streams, id)
}

// SetDeafened makes Pull give silence, while streams keep queuing.
func (m *Mixer) SetDeafened(on bool) {
	m.mu.Lock()
	m.muted = on
	m.mu.Unlock()
}

// Pull fills out with the next frame of every stream, added up.
func (m *Mixer) Pull(out []int16) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := make([]int32, len(out))
	for _, s := range m.streams {
		if !s.playing {
			if len(s.queue) < startAfter {
				continue
			}
			s.playing = true
		}
		if len(s.queue) == 0 {
			s.playing = false // ran dry: wait to fill again
			continue
		}
		f := s.queue[0]
		s.queue = s.queue[1:]
		for i := range acc {
			if i < len(f) {
				acc[i] += int32(f[i])
			}
		}
	}
	for i, v := range acc {
		if m.muted {
			v = 0
		}
		out[i] = int16(max(-32768, min(32767, v)))
	}
}

// Levels returns each stream's recent loudness, from 0 to 1, for streams
// heard in the last half second.
func (m *Mixer) Levels() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]float64{}
	for id, s := range m.streams {
		if time.Since(s.heard) < 500*time.Millisecond {
			out[id] = s.level
		}
	}
	return out
}

// Recorder pulls a mixer's audio in real time and keeps it, for tests.
type Recorder struct {
	mu      sync.Mutex
	samples []int16
	stop    chan struct{}
	done    chan struct{}
}

// Record starts pulling m every 20 ms until Stop.
func Record(m *Mixer) *Recorder {
	r := &Recorder{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		t := time.NewTicker(FrameTime)
		defer t.Stop()
		frame := make([]int16, Frame)
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				m.Pull(frame)
				r.mu.Lock()
				r.samples = append(r.samples, frame...)
				r.mu.Unlock()
			}
		}
	}()
	return r
}

// Stop stops recording.
func (r *Recorder) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

// Samples returns what was recorded so far.
func (r *Recorder) Samples() []int16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int16(nil), r.samples...)
}

// Power measures how much of a frequency a recording holds, with the
// Goertzel algorithm, for tests that check who's heard.
func Power(samples []int16, hz float64) float64 {
	k := 2 * math.Cos(2*math.Pi*hz/SampleRate)
	var a, b float64
	for _, s := range samples {
		a, b = float64(s)/32768+k*a-b, a
	}
	return (a*a + b*b - k*a*b) / math.Max(1, float64(len(samples)))
}
