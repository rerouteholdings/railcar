// Package rtc carries call and voice channel audio over WebRTC with pion:
// DTLS-SRTP between peers, and inside it every Opus frame encrypted with
// SFrame (§15.2, §16.4). It knows nothing of signaling: the caller passes
// session descriptions and ICE candidates as the JSON text §15.2 uses.
package rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/rerouteholdings/railcar/internal/media/audio"
	"github.com/rerouteholdings/railcar/internal/media/opus"
	"github.com/rerouteholdings/railcar/internal/tieroute/sframe"
)

// Options configure connections.
type Options struct {
	// ICEServers are the TURN relays, from GET turn (§15.1).
	ICEServers []webrtc.ICEServer
	// RelayOnly uses only relayed candidates, so nobody learns anybody
	// else's IP address (§15.2). It's the default for calls.
	RelayOnly bool
	// Loopback adds loopback candidates, for tests on one machine.
	Loopback bool
}

// NewAPI returns a pion API with Opus, and H.264 so cast video sections
// can be accepted (§15.3).
func NewAPI(o Options) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	s := webrtc.SettingEngine{}
	s.SetIncludeLoopbackCandidate(o.Loopback)
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(s)), nil
}

// Config returns the configuration for a connection.
func (o Options) Config() webrtc.Configuration {
	c := webrtc.Configuration{ICEServers: o.ICEServers, BundlePolicy: webrtc.BundlePolicyMaxBundle}
	if o.RelayOnly {
		c.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	return c
}

// Candidate is an ICE candidate in the shape §15.2 uses. An empty
// Candidate means the sender has no more.
type Candidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

// Description is an offer or answer in the shape §15.2 uses.
type Description struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// Keyring opens SFrame frames: it gives the key for a KID, and a name for
// the stream that KID belongs to, such as a person.
type Keyring interface {
	Key(kid uint64) (stream string, key *sframe.Key, ok bool)
}

// Media is one device's side of a call or a voice channel: the audio it
// sends, and the audio it hears from everyone.
type Media struct {
	Track *webrtc.TrackLocalStaticSample
	Mix   *audio.Mixer

	src    audio.Source
	sender atomic.Pointer[sframe.Sender]
	keys   Keyring
	muted  atomic.Bool
	level  atomic.Uint64 // the microphone's loudness, as float64 bits
	castOn atomic.Bool   // play cast audio

	mu       sync.Mutex
	decoders map[string]*opus.Decoder
	frames   atomic.Uint64 // frames decoded, for tests
}

// NewMedia returns media that sends src and plays into mix.
func NewMedia(src audio.Source, mix *audio.Mixer, keys Keyring) (*Media, error) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "voice", "railcar")
	if err != nil {
		return nil, err
	}
	m := &Media{Track: track, Mix: mix, src: src, keys: keys, decoders: map[string]*opus.Decoder{}}
	m.castOn.Store(true)
	return m, nil
}

// SetSender sets the SFrame sender for the frames that follow, such as
// after a new epoch or a new voice key.
func (m *Media) SetSender(s *sframe.Sender) { m.sender.Store(s) }

// SetMuted mutes or unmutes the microphone. A muted device sends
// silence.
func (m *Media) SetMuted(on bool) { m.muted.Store(on) }

// Muted says whether the microphone is muted.
func (m *Media) Muted() bool { return m.muted.Load() }

// SetCastAudio plays or silences other people's cast audio (§15.3).
func (m *Media) SetCastAudio(on bool) { m.castOn.Store(on) }

// Level is the microphone's recent loudness, from 0 to 1.
func (m *Media) Level() float64 {
	return float64frombits(m.level.Load())
}

// Frames is how many frames this device has decoded.
func (m *Media) Frames() uint64 { return m.frames.Load() }

// Run reads the source, and sends each frame, encoded and sealed, on the
// track, until ctx ends.
func (m *Media) Run(ctx context.Context) error {
	enc, err := opus.NewEncoder(1, false)
	if err != nil {
		return err
	}
	defer enc.Close()
	frame := make([]int16, audio.Frame)
	var lvl float64
	for {
		if err := m.src.Read(ctx, frame); err != nil {
			return err
		}
		if m.muted.Load() {
			clear(frame)
		}
		lvl = max(audio.Level(frame), lvl*0.7)
		m.level.Store(float64bits(lvl))
		s := m.sender.Load()
		if s == nil {
			continue // no key yet
		}
		pkt, err := enc.Encode(frame)
		if err != nil {
			return err
		}
		if err := m.Track.WriteSample(media.Sample{Data: s.Seal(pkt), Duration: audio.FrameTime}); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
	}
}

// Receive reads a remote track until it ends, opening each frame and
// playing it. kind names the media section: "voice", "cast", or "video".
// Video can't be shown in a terminal, so its packets are read and
// dropped.
func (m *Media) Receive(t *webrtc.TrackRemote, kind string) {
	if t.Kind() != webrtc.RTPCodecTypeAudio {
		for {
			if _, _, err := t.ReadRTP(); err != nil {
				return
			}
		}
	}
	pcm := make([]int16, 2*opus.FrameSize*6) // up to 120 ms, stereo
	mono := make([]int16, audio.Frame)
	ssrc := strconv.FormatUint(uint64(t.SSRC()), 10)
	var seen []string
	defer func() {
		for _, id := range seen {
			m.Mix.Remove(id)
		}
	}()
	for {
		p, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		kid, ctr, n, err := sframe.ParseHeader(p.Payload)
		if err != nil {
			continue
		}
		stream, key, ok := m.keys.Key(kid)
		if !ok {
			continue // no key for this KID: dropped (§15.2)
		}
		frame, err := key.Open(p.Payload, ctr, n, nil)
		if err != nil {
			continue
		}
		id := stream + "/" + kind + "/" + ssrc + "/" + strconv.FormatUint(kid, 16)
		m.mu.Lock()
		dec := m.decoders[id]
		if dec == nil {
			if dec, err = opus.NewDecoder(2); err != nil {
				m.mu.Unlock()
				return
			}
			m.decoders[id] = dec
			seen = append(seen, id)
		}
		m.mu.Unlock()
		got, err := dec.Decode(frame, pcm)
		if err != nil || got != opus.FrameSize {
			continue
		}
		m.frames.Add(1)
		if kind == "cast" && !m.castOn.Load() {
			continue
		}
		for i := range mono {
			mono[i] = int16((int32(pcm[2*i]) + int32(pcm[2*i+1])) / 2)
		}
		m.Mix.Push(id, mono)
	}
}

// Speaking returns each stream's loudness from the mixer, by the stream
// name the keyring gave.
func (m *Media) Speaking() map[string]float64 {
	out := map[string]float64{}
	for id, l := range m.Mix.Levels() {
		for i := range id {
			if id[i] == '/' {
				id = id[:i]
				break
			}
		}
		out[id] = max(out[id], l)
	}
	return out
}

// Peer is one WebRTC connection.
type Peer struct {
	PC *webrtc.PeerConnection

	mu      sync.Mutex
	pending []webrtc.ICECandidateInit // candidates before the remote description
	remote  bool
}

// NewPeer makes a connection. onICE gets each local candidate, and an
// empty one when there are no more. onTrack gets each remote track with
// the kind of its media section.
func NewPeer(api *webrtc.API, o Options, onICE func(Candidate), onTrack func(*webrtc.TrackRemote, string)) (*Peer, error) {
	pc, err := api.NewPeerConnection(o.Config())
	if err != nil {
		return nil, err
	}
	p := &Peer{PC: pc}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			onICE(Candidate{})
			return
		}
		init := c.ToJSON()
		onICE(Candidate{Candidate: init.Candidate, SDPMid: init.SDPMid, SDPMLineIndex: init.SDPMLineIndex})
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		onTrack(t, p.sectionKind(r))
	})
	return p, nil
}

// sectionKind says which of §15.3's sections a receiver is in: the
// first is the voice, the second the cast's audio, the third its video.
// Later sections, which a community server adds for other people's
// streams, are voice when they carry audio.
func (p *Peer) sectionKind(r *webrtc.RTPReceiver) string {
	for i, t := range p.PC.GetTransceivers() {
		if t.Receiver() != r {
			continue
		}
		switch {
		case t.Kind() == webrtc.RTPCodecTypeVideo:
			return "video"
		case i == 1:
			return "cast"
		}
		return "voice"
	}
	return "voice"
}

// AddSections adds §15.3's three sections, in order: the voice, sending
// track, then the cast's audio and video, which this device never sends
// on but receives.
func (p *Peer) AddSections(track webrtc.TrackLocal) error {
	if _, err := p.PC.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		return err
	}
	if _, err := p.PC.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv}); err != nil {
		return err
	}
	_, err := p.PC.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	return err
}

// Offer makes an offer and returns it as JSON text.
func (p *Peer) Offer() (string, error) {
	o, err := p.PC.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err := p.PC.SetLocalDescription(o); err != nil {
		return "", err
	}
	return marshalDesc(o)
}

func marshalDesc(d webrtc.SessionDescription) (string, error) {
	b, err := json.Marshal(Description{Type: d.Type.String(), SDP: d.SDP})
	return string(b), err
}

// Remote applies a remote offer or answer. For a first offer, track is
// sent on its first audio section, the voice. The answer to an offer
// comes back as JSON text.
func (p *Peer) Remote(data string, track webrtc.TrackLocal) (string, error) {
	var d Description
	if err := json.Unmarshal([]byte(data), &d); err != nil || d.SDP == "" {
		return "", fmt.Errorf("rtc: a malformed session description")
	}
	var typ webrtc.SDPType
	switch d.Type {
	case "offer":
		typ = webrtc.SDPTypeOffer
	case "answer":
		typ = webrtc.SDPTypeAnswer
	default:
		return "", fmt.Errorf("rtc: a session description of type %q", d.Type)
	}
	if typ == webrtc.SDPTypeOffer && p.PC.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
		// Our own offer crossed theirs: roll it back first.
		if err := p.PC.SetLocalDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeRollback}); err != nil {
			return "", err
		}
	}
	if typ == webrtc.SDPTypeOffer && track != nil && len(p.PC.GetTransceivers()) == 0 {
		// The track goes on the offer's first audio section, the voice.
		if _, err := p.PC.AddTrack(track); err != nil {
			return "", err
		}
	}
	if err := p.PC.SetRemoteDescription(webrtc.SessionDescription{Type: typ, SDP: d.SDP}); err != nil {
		return "", err
	}
	p.mu.Lock()
	p.remote = true
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	for _, c := range pending {
		p.PC.AddICECandidate(c)
	}
	if typ == webrtc.SDPTypeAnswer {
		return "", nil
	}
	a, err := p.PC.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err := p.PC.SetLocalDescription(a); err != nil {
		return "", err
	}
	return marshalDesc(a)
}

// AddCandidate adds a remote ICE candidate, given as JSON text. An empty
// candidate means the remote side has no more.
func (p *Peer) AddCandidate(data string) error {
	var c Candidate
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return fmt.Errorf("rtc: a malformed ICE candidate")
	}
	if c.Candidate == "" {
		return nil
	}
	init := webrtc.ICECandidateInit{Candidate: c.Candidate, SDPMid: c.SDPMid, SDPMLineIndex: c.SDPMLineIndex}
	p.mu.Lock()
	if !p.remote {
		p.pending = append(p.pending, init)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.PC.AddICECandidate(init)
}

// Connected waits until the connection is up, fails, or timeout passes.
func (p *Peer) Connected(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		switch p.PC.ConnectionState() {
		case webrtc.PeerConnectionStateConnected:
			return nil
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			return errors.New("rtc: the connection failed")
		}
		select {
		case <-ctx.Done():
			return errors.New("rtc: the connection timed out")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close closes the connection.
func (p *Peer) Close() error { return p.PC.Close() }
