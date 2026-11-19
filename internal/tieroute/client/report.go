package client

import (
	"context"
	"net/http"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Report is a report of messages (§17.3).
type Report struct {
	Reporter string     `json:"reporter"`
	Reported string     `json:"reported"`
	Reason   string     `json:"reason"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// Evidence is one reported message: the signed app-message as decrypted,
// and the msg-key that certifies the key it was signed with (§17.2).
type Evidence struct {
	Msg wire.Signed `json:"msg"`
	MK  wire.Signed `json:"mk"`
}

// ReportAt names where a reported message was in a community.
type ReportAt struct {
	Channel uint64 `json:"channel"`
	Seq     uint64 `json:"seq"`
}

// Report sends a report to the homeserver's admins (POST report). With
// forward, the homeserver also sends it to the reported identity's home
// (§17.2).
func (c *Client) Report(ctx context.Context, r Report, forward bool) error {
	r.Reporter = c.ID()
	req := struct {
		Report  Report `json:"report"`
		Forward bool   `json:"forward,omitempty"`
	}{r, forward}
	return c.authed(ctx, http.MethodPost, "report", nil, req, nil)
}

// Report sends a report to the community's moderators (POST report). at
// says where each message was, when it came through a channel. The
// community server can read the report (community server API, Report
// messages).
func (cm *Community) Report(ctx context.Context, r Report, at []ReportAt) error {
	r.Reporter = cm.c.ID()
	req := struct {
		Report Report     `json:"report"`
		At     []ReportAt `json:"at,omitempty"`
	}{r, at}
	var res struct {
		ID wire.Bytes `json:"id"`
	}
	return cm.authed(ctx, http.MethodPost, "report", nil, req, &res)
}
