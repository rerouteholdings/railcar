package client

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Delivery is a delivery as a recipient's inbox holds it (§10.1).
type Delivery struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"` // "mls" or "link"
	From      string       `json:"from"`
	To        []string     `json:"to"`
	Group     string       `json:"group,omitempty"`
	Epoch     *uint64      `json:"epoch,omitempty"`
	MLS       string       `json:"mls,omitempty"` // "app", "proposal", "commit", or "welcome"
	Receipt   *wire.Signed `json:"receipt,omitempty"`
	Ephemeral bool         `json:"ephemeral,omitempty"`
	Channel   *uint64      `json:"channel,omitempty"`
	Seq       *uint64      `json:"seq,omitempty"`
	Proof     *wire.Signed `json:"proof,omitempty"`
	Data      wire.Bytes   `json:"data"`
}

// Notice types the homeserver itself puts in the inbox (client API).
const (
	NoticeContactRequest  = "contact-request"
	NoticeContactDeclined = "contact-declined"
	NoticeChainAlert      = "chain-alert"
	NoticeChainUpdate     = "chain-update"
	NoticeDeliveryFailed  = "delivery-failed"
)

// Notice is a notice from the homeserver. Fields are set by type; clients
// ignore types they don't know (§20.2), and Raw keeps the whole notice.
type Notice struct {
	T       string        `json:"t"`
	Request *wire.Signed  `json:"request,omitempty"` // contact-request
	Chain   []wire.Signed `json:"chain,omitempty"`   // contact-request: the requester's chain
	Decline *wire.Signed  `json:"decline,omitempty"` // contact-declined
	Level   string        `json:"level,omitempty"`   // chain-alert: "alarm" or "notice"
	Entry   *wire.Signed  `json:"entry,omitempty"`   // chain-alert
	ID      string        `json:"id,omitempty"`      // chain-update: the identity; delivery-failed: the delivery
	To      []string      `json:"to,omitempty"`      // delivery-failed
	Reason  string        `json:"reason,omitempty"`  // delivery-failed: an error code

	Raw jsontext.Value `json:"-"`
}

// Item is one inbox item: a delivery from another user, or a notice from
// the homeserver.
type Item struct {
	Cursor   string    // absent only for ephemeral items on the stream
	TS       int64     // when the server received it
	Delivery *Delivery // set for a delivery
	Notice   *Notice   // set for a notice
}

type itemJSON struct {
	Cursor   string         `json:"cursor,omitempty"`
	TS       int64          `json:"ts"`
	Delivery *Delivery      `json:"delivery,omitempty"`
	Notice   jsontext.Value `json:"notice,omitempty"`
}

// UnmarshalJSON reads an item. A notice whose known members don't decode
// is kept with only T and Raw, so one odd notice doesn't stop the inbox.
func (it *Item) UnmarshalJSON(data []byte) error {
	var j itemJSON
	if err := wire.Unmarshal(data, &j); err != nil {
		return err
	}
	*it = Item{Cursor: j.Cursor, TS: j.TS, Delivery: j.Delivery}
	if len(j.Notice) > 0 {
		var n Notice
		if err := wire.Unmarshal(j.Notice, &n); err != nil {
			var t struct {
				T string `json:"t"`
			}
			_ = wire.Unmarshal(j.Notice, &t)
			n = Notice{T: t.T}
		}
		n.Raw = j.Notice
		it.Notice = &n
	}
	if it.Delivery == nil && it.Notice == nil {
		return errors.New("client: an inbox item without a delivery or notice")
	}
	return nil
}

// Page is one page of the inbox.
type Page struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
	Gap   bool   `json:"gap"` // items expired before this device acked them (§10.4)
}

// Inbox fetches inbox items after a cursor (GET inbox). An empty cursor
// starts at the beginning, and limit 0 uses the server's default.
func (c *Client) Inbox(ctx context.Context, after string, limit int) (*Page, error) {
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var p Page
	if err := c.authed(ctx, http.MethodGet, "inbox", q, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Cursor returns the last cursor this device acked, which a new stream or
// inbox fetch starts after.
func (c *Client) Cursor() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Cursor
}

// Ack marks every item up to cursor as processed by this device
// (POST inbox/ack). An item is deleted once every current device has acked
// past it (§10.4).
func (c *Client) Ack(ctx context.Context, cursor string) error {
	if cursor == "" {
		return errors.New("client: no cursor to ack")
	}
	if err := c.authed(ctx, http.MethodPost, "inbox/ack", nil, map[string]string{"upto": cursor}, nil); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state.Cursor = cursor
	return c.saveLocked()
}

// Outgoing is one delivery to send (POST send). The homeserver fills in
// from.
type Outgoing struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	To        []string     `json:"to"`
	Group     string       `json:"group,omitempty"`
	Epoch     *uint64      `json:"epoch,omitempty"`
	MLS       string       `json:"mls,omitempty"`
	Ephemeral bool         `json:"ephemeral,omitempty"`
	Data      wire.Bytes   `json:"data"`
	Commit    *wire.Signed `json:"commit,omitempty"`
	Hub       string       `json:"hub,omitempty"`
	Welcomes  []Welcome    `json:"welcomes,omitempty"`
}

// Welcome is a Welcome sent with a commit, delivered only if the hub
// accepts the commit (§11.6).
type Welcome struct {
	ID   string     `json:"id"`
	To   []string   `json:"to"`
	Data wire.Bytes `json:"data"`
}

// SendResult is the homeserver's answer for one outgoing delivery.
type SendResult struct {
	ID      string       `json:"id"`
	Status  string       `json:"status"` // "sent" or "rejected"
	Error   string       `json:"error,omitempty"`
	Receipt *wire.Signed `json:"receipt,omitempty"`
}

// NewMsgID returns a random 16-byte delivery ID (§4.2).
func NewMsgID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return wire.EncodeB64(b)
}

// Send submits deliveries (POST send). Resubmitting an ID is safe: the
// server returns the original result.
func (c *Client) Send(ctx context.Context, out ...Outgoing) ([]SendResult, error) {
	var res struct {
		Results []SendResult `json:"results"`
	}
	if err := c.authed(ctx, http.MethodPost, "send", nil, map[string]any{"deliveries": out}, &res); err != nil {
		return nil, err
	}
	if len(res.Results) != len(out) {
		return nil, fmt.Errorf("client: %d results for %d deliveries", len(res.Results), len(out))
	}
	return res.Results, nil
}

// MaxLinkData is the largest link delivery (client API).
const MaxLinkData = 256 << 10

// SendLink sends a link delivery to the identity's own inbox: an encrypted
// link bundle for a new device (§7.2, §10.1).
func (c *Client) SendLink(ctx context.Context, data []byte) (SendResult, error) {
	if len(data) == 0 || len(data) > MaxLinkData {
		return SendResult{}, errors.New("client: a link delivery holds 1 byte to 256 KiB")
	}
	res, err := c.Send(ctx, Outgoing{ID: NewMsgID(), Kind: "link", To: []string{c.ID()}, Data: data})
	if err != nil {
		return SendResult{}, err
	}
	return res[0], nil
}
