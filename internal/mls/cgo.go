package mls

// The static library comes from `make mls`, which runs `cargo build
// --release` in mls/. The system libraries after it are the ones Rust's
// standard library needs on each system, as `cargo rustc -- --print
// native-static-libs` lists them. macOS needs no frameworks. On Windows,
// cgo links with MinGW, so the library is built for the
// x86_64-pc-windows-gnu target; that build isn't tested yet.

/*
#cgo CFLAGS: -I${SRCDIR}/../../mls/include
#cgo LDFLAGS: ${SRCDIR}/../../mls/target/release/libtieroute_mls.a
#cgo darwin LDFLAGS: -liconv -lm
#cgo linux LDFLAGS: -lgcc_s -lutil -lrt -lpthread -lm -ldl -lc
#cgo windows LDFLAGS: -lkernel32 -lntdll -luserenv -lws2_32 -ldbghelp -lbcrypt
#include "tieroute_mls.h"
*/
import "C"

import (
	"encoding/json"
	"unsafe"
)

// in returns a pointer to b's bytes and its length, for passing to C. The
// library doesn't keep the pointer after the call returns.
func in(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0])), C.size_t(len(b))
}

// call runs a library function and copies its result into Go memory.
func call(f func(out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	var out C.TrmlsBuf
	code := f(&out)
	b := C.GoBytes(unsafe.Pointer(out.ptr), C.int(out.len))
	C.trmls_buf_free(out)
	if code != 0 {
		return nil, &Error{Code: ErrorCode(code), Message: string(b)}
	}
	return b, nil
}

// callJSON marshals req, runs a library function on it, and unmarshals
// the result into res when res isn't nil.
func callJSON(req any, res any, f func(p *C.uint8_t, n C.size_t, out *C.TrmlsBuf) C.int32_t) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	p, n := in(body)
	b, err := call(func(out *C.TrmlsBuf) C.int32_t { return f(p, n, out) })
	if err != nil {
		return nil, err
	}
	if res != nil {
		if err := json.Unmarshal(b, res); err != nil {
			return nil, &Error{Code: ErrInternal, Message: "decoding the library's response: " + err.Error()}
		}
	}
	return b, nil
}

func ciphersuite() uint16 {
	return uint16(C.trmls_ciphersuite())
}

func unmarshal(b []byte, res any) error {
	if err := json.Unmarshal(b, res); err != nil {
		return &Error{Code: ErrInternal, Message: "decoding the library's response: " + err.Error()}
	}
	return nil
}
