package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// Device is a microphone and speakers, through miniaudio. The microphone
// is a Source, and the speakers play a Mixer.
type Device struct {
	ctx      *malgo.AllocatedContext
	capture  *malgo.Device
	playback *malgo.Device
	frames   chan []int16

	mu      sync.Mutex
	partial []int16 // captured samples short of a frame
	out     []int16 // pulled samples the speakers haven't taken
}

// Open starts the default microphone and speakers. mix is what the
// speakers play.
func Open(mix *Mixer) (*Device, error) { return OpenNamed(mix, "", "") }

// OpenNamed starts the microphone and speakers with these names, as List
// gives them. An empty name, or one that isn't connected now, is the
// system's default.
func OpenNamed(mix *Mixer, mic, speakers string) (*Device, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio: %w", err)
	}
	d := &Device{ctx: ctx, frames: make(chan []int16, 10)}

	cc := malgo.DefaultDeviceConfig(malgo.Capture)
	if id := find(ctx, malgo.Capture, mic); id != nil {
		cc.Capture.DeviceID = id
		defer cfree(id)
	}
	cc.Capture.Format = malgo.FormatS16
	cc.Capture.Channels = 1
	cc.SampleRate = SampleRate
	cc.PeriodSizeInMilliseconds = 20
	d.capture, err = malgo.InitDevice(ctx.Context, cc, malgo.DeviceCallbacks{Data: d.captured})
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: the microphone: %w", err)
	}

	pc := malgo.DefaultDeviceConfig(malgo.Playback)
	if id := find(ctx, malgo.Playback, speakers); id != nil {
		pc.Playback.DeviceID = id
		defer cfree(id)
	}
	pc.Playback.Format = malgo.FormatS16
	pc.Playback.Channels = 1
	pc.SampleRate = SampleRate
	pc.PeriodSizeInMilliseconds = 20
	d.playback, err = malgo.InitDevice(ctx.Context, pc, malgo.DeviceCallbacks{
		Data: func(out, _ []byte, n uint32) { d.play(mix, out, int(n)) },
	})
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: the speakers: %w", err)
	}
	if err := d.capture.Start(); err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: starting the microphone: %w", err)
	}
	if err := d.playback.Start(); err != nil {
		d.Close()
		return nil, fmt.Errorf("audio: starting the speakers: %w", err)
	}
	return d, nil
}

// captured runs on miniaudio's thread with the microphone's samples.
func (d *Device) captured(_, in []byte, n uint32) {
	d.mu.Lock()
	for i := 0; i+1 < len(in) && i/2 < int(n); i += 2 {
		d.partial = append(d.partial, int16(binary.LittleEndian.Uint16(in[i:])))
	}
	var ready [][]int16
	for len(d.partial) >= Frame {
		ready = append(ready, append([]int16(nil), d.partial[:Frame]...))
		d.partial = d.partial[Frame:]
	}
	d.mu.Unlock()
	for _, f := range ready {
		select {
		case d.frames <- f:
		default: // the call fell behind: drop the frame
		}
	}
}

// play runs on miniaudio's thread to fill the speakers' buffer.
func (d *Device) play(mix *Mixer, out []byte, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.out) < n {
		f := make([]int16, Frame)
		mix.Pull(f)
		d.out = append(d.out, f...)
	}
	for i := 0; i < n && 2*i+1 < len(out); i++ {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(d.out[i]))
	}
	d.out = d.out[n:]
}

// Read gives the next frame from the microphone.
func (d *Device) Read(ctx context.Context, frame []int16) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f, ok := <-d.frames:
		if !ok {
			return errors.New("audio: the microphone stopped")
		}
		copy(frame, f)
		return nil
	}
}

// Close stops the microphone and speakers.
func (d *Device) Close() {
	if d.capture != nil {
		d.capture.Uninit()
		d.capture = nil
	}
	if d.playback != nil {
		d.playback.Uninit()
		d.playback = nil
	}
	if d.ctx != nil {
		d.ctx.Uninit()
		d.ctx.Free()
		d.ctx = nil
	}
}

// Devices are the microphones and speakers connected now, by name.
type Devices struct {
	Microphones []string
	Speakers    []string
}

// List gives the microphones and speakers connected now.
func List() (Devices, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return Devices{}, fmt.Errorf("audio: %w", err)
	}
	defer func() {
		ctx.Uninit()
		ctx.Free()
	}()
	var out Devices
	for _, kind := range []malgo.DeviceType{malgo.Capture, malgo.Playback} {
		infos, err := ctx.Devices(kind)
		if err != nil {
			return Devices{}, fmt.Errorf("audio: listing devices: %w", err)
		}
		seen := map[string]bool{}
		for _, in := range infos {
			name := in.Name()
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			if kind == malgo.Capture {
				out.Microphones = append(out.Microphones, name)
			} else {
				out.Speakers = append(out.Speakers, name)
			}
		}
	}
	return out, nil
}

// find returns a C copy of the ID of the device with this name, which the
// caller frees with cfree, or nil for the default.
func find(ctx *malgo.AllocatedContext, kind malgo.DeviceType, name string) unsafe.Pointer {
	if name == "" {
		return nil
	}
	infos, err := ctx.Devices(kind)
	if err != nil {
		return nil
	}
	for _, in := range infos {
		if in.Name() == name {
			return in.ID.Pointer()
		}
	}
	return nil
}
