package client

import (
	"context"
	"net/http"
)

// Relay is the homeserver's call relay: TURN credentials in the TURN REST
// API format (§15.1, client API "Call relays").
type Relay struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	TTL        uint64   `json:"ttl"` // seconds the credentials stay valid
}

// Turn gets credentials for the homeserver's call relay (GET turn). A
// homeserver without one answers 501 not-implemented.
func (c *Client) Turn(ctx context.Context) (*Relay, error) {
	var r Relay
	if err := c.authed(ctx, http.MethodGet, "turn", nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
