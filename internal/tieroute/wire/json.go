package wire

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

// MaxInt is the largest integer I-JSON carries exactly (§4.1).
const MaxInt = 1<<53 - 1

// CheckIJSON reports whether data is one I-JSON value with Tieroute's
// extra rules: valid UTF-8, no duplicate member names, and only integers
// within ±(2⁵³ − 1), with no fraction or exponent (§4.1).
func CheckIJSON(data []byte) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			if depth != 0 {
				return errors.New("wire: truncated JSON")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("wire: invalid JSON: %w", err)
		}
		switch tok.Kind() {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '0':
			if err := checkInt(tok.String()); err != nil {
				return err
			}
		}
		if depth == 0 {
			// One top-level value only.
			if _, err := dec.ReadToken(); err != io.EOF {
				return errors.New("wire: data after the JSON value")
			}
			return nil
		}
	}
}

func checkInt(s string) error {
	t := s
	if len(t) > 0 && t[0] == '-' {
		t = t[1:]
	}
	if len(t) == 0 || len(t) > 16 {
		return fmt.Errorf("wire: number out of range: %s", s)
	}
	var n int64
	for _, c := range t {
		if c < '0' || c > '9' {
			return fmt.Errorf("wire: not an integer: %s", s)
		}
		n = n*10 + int64(c-'0')
	}
	if n > MaxInt {
		return fmt.Errorf("wire: number out of range: %s", s)
	}
	return nil
}

// Unmarshal decodes I-JSON into v. It checks CheckIJSON's rules first, and
// ignores unknown members (§20.2).
func Unmarshal(data []byte, v any) error {
	if err := CheckIJSON(data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// Marshal encodes v as compact JSON. It isn't canonical; use JCS for bytes
// that are signed or hashed.
func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// JCS returns the JSON Canonicalization Scheme (RFC 8785) serialization of v
// (§1.1). v is a Go value, or raw JSON as jsontext.Value or []byte.
func JCS(v any) ([]byte, error) {
	var raw jsontext.Value
	switch x := v.(type) {
	case jsontext.Value:
		raw = bytes.Clone(x)
	case []byte:
		raw = bytes.Clone(x)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	if err := CheckIJSON(raw); err != nil {
		return nil, err
	}
	if err := raw.Canonicalize(); err != nil {
		return nil, err
	}
	return raw, nil
}

// MustJCS is JCS for values that always encode, such as fixed arrays of
// strings. It panics on error.
func MustJCS(v any) []byte {
	b, err := JCS(v)
	if err != nil {
		panic(err)
	}
	return b
}
