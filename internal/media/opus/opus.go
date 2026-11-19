// Package opus encodes and decodes Opus audio (RFC 6716) with libopus,
// for calls and voice channels (§15.2, §16.4). libopus is built as a
// static library by `make opus`, so railcar needs nothing installed.
//
// Audio is 48 kHz, 16-bit, in frames of 20 ms.
package opus

/*
#cgo CFLAGS: -I${SRCDIR}/../../../build/opus/include
#cgo LDFLAGS: ${SRCDIR}/../../../build/opus/lib/libopus.a -lm
#include <opus/opus.h>
#include <stdlib.h>

static int enc_ctl_int(OpusEncoder *e, int request, opus_int32 v) { return opus_encoder_ctl(e, request, v); }
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Audio format.
const (
	SampleRate = 48000
	// FrameSize is the samples per channel in one 20 ms frame.
	FrameSize = SampleRate / 50
	// MaxPacket is the largest encoded frame this package makes.
	MaxPacket = 1500
)

func opusError(op string, code C.int) error {
	return fmt.Errorf("opus: %s: %s", op, C.GoString(C.opus_strerror(code)))
}

// Encoder encodes 20 ms frames. It isn't safe for more than one
// goroutine.
type Encoder struct {
	mu       sync.Mutex
	p        *C.OpusEncoder
	channels int
	out      []byte
}

// NewEncoder returns an encoder for voice, or for music when music is
// true, with channels of 1 or 2.
func NewEncoder(channels int, music bool) (*Encoder, error) {
	app := C.OPUS_APPLICATION_VOIP
	if music {
		app = C.OPUS_APPLICATION_AUDIO
	}
	var code C.int
	p := C.opus_encoder_create(SampleRate, C.int(channels), C.int(app), &code)
	if code != C.OPUS_OK {
		return nil, opusError("creating an encoder", code)
	}
	e := &Encoder{p: p, channels: channels, out: make([]byte, MaxPacket)}
	// Forward error correction helps over lossy relays.
	C.enc_ctl_int(p, C.OPUS_SET_INBAND_FEC_REQUEST, 1)
	C.enc_ctl_int(p, C.OPUS_SET_PACKET_LOSS_PERC_REQUEST, 5)
	runtime.SetFinalizer(e, (*Encoder).Close)
	return e, nil
}

// SetBitrate sets the target bitrate in bits per second.
func (e *Encoder) SetBitrate(bps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.p != nil {
		C.enc_ctl_int(e.p, C.OPUS_SET_BITRATE_REQUEST, C.opus_int32(bps))
	}
}

// Encode encodes one frame of FrameSize samples per channel, interleaved,
// and returns a new slice with the packet.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.p == nil {
		return nil, errors.New("opus: the encoder is closed")
	}
	if len(pcm) != FrameSize*e.channels {
		return nil, fmt.Errorf("opus: a frame of %d samples, not %d", len(pcm), FrameSize*e.channels)
	}
	n := C.opus_encode(e.p, (*C.opus_int16)(unsafe.Pointer(&pcm[0])), FrameSize,
		(*C.uchar)(unsafe.Pointer(&e.out[0])), C.opus_int32(len(e.out)))
	if n < 0 {
		return nil, opusError("encoding", n)
	}
	return append([]byte(nil), e.out[:n]...), nil
}

// Close frees the encoder.
func (e *Encoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.p != nil {
		C.opus_encoder_destroy(e.p)
		e.p = nil
	}
}

// Decoder decodes packets into 48 kHz audio.
type Decoder struct {
	mu       sync.Mutex
	p        *C.OpusDecoder
	channels int
}

// NewDecoder returns a decoder with channels of 1 or 2.
func NewDecoder(channels int) (*Decoder, error) {
	var code C.int
	p := C.opus_decoder_create(SampleRate, C.int(channels), &code)
	if code != C.OPUS_OK {
		return nil, opusError("creating a decoder", code)
	}
	d := &Decoder{p: p, channels: channels}
	runtime.SetFinalizer(d, (*Decoder).Close)
	return d, nil
}

// Decode decodes one packet into pcm, interleaved, and returns the samples
// per channel. A nil packet conceals a lost one.
func (d *Decoder) Decode(packet []byte, pcm []int16) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p == nil {
		return 0, errors.New("opus: the decoder is closed")
	}
	if len(pcm) < d.channels {
		return 0, errors.New("opus: no room to decode into")
	}
	var data *C.uchar
	if len(packet) > 0 {
		data = (*C.uchar)(unsafe.Pointer(&packet[0]))
	}
	n := C.opus_decode(d.p, data, C.opus_int32(len(packet)),
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)/d.channels), 0)
	if n < 0 {
		return 0, opusError("decoding", n)
	}
	return int(n), nil
}

// Close frees the decoder.
func (d *Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.p != nil {
		C.opus_decoder_destroy(d.p)
		d.p = nil
	}
}
