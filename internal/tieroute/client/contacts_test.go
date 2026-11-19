package client

import (
	"errors"
	"testing"
)

func TestParseInviteCode(t *testing.T) {
	for _, tc := range []struct {
		in     string
		handle string
		code   string
		bad    bool
	}{
		{in: "@ravi.example.org::Kx82mPq1Za", handle: "ravi.example.org", code: "Kx82mPq1Za"},
		{in: "  @Ravi.Example.org::Kx82mPq1Za\n", handle: "ravi.example.org", code: "Kx82mPq1Za"},
		{in: "https://rerouteholdings.llc/@ravi.example.org::Kx82mPq1Za", handle: "ravi.example.org", code: "Kx82mPq1Za"},
		{in: "@ravi.example.org", handle: "ravi.example.org"},
		{in: "community.example.com::Kx82mPq1ZaR4vT9b", bad: true}, // a community code (§16.6)
		{in: "@ravi.example.org::short", bad: true},
		{in: "@ravi.example.org::Kx82mPq1Z!", bad: true},
		{in: "ravi@example.org", bad: true},
		{in: "", bad: true},
	} {
		got, err := ParseInviteCode(tc.in)
		if tc.bad {
			if !errors.Is(err, ErrUnknownCode) {
				t.Errorf("%q: %+v, %v", tc.in, got, err)
			}
			continue
		}
		if err != nil || got.Handle != tc.handle || got.Code != tc.code {
			t.Errorf("%q: %+v, %v", tc.in, got, err)
		}
		if tc.code != "" && got.String() != "@"+tc.handle+"::"+tc.code {
			t.Errorf("%q: String() = %q", tc.in, got.String())
		}
	}
}
