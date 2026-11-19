package client

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Stream timing (API conventions).
const (
	pingEvery   = 30 * time.Second
	silentLimit = 90 * time.Second
	dialTimeout = 30 * time.Second
	minBackoff  = time.Second
	maxBackoff  = time.Minute
	maxFrame    = 1 << 20
)

// WebSocket close codes the stream uses (API conventions).
const (
	closeProtocol        = 4400
	closeUnauthenticated = 4401
	closeNotAllowed      = 4403
	closeRateLimited     = 4429
)

// EventKind says what a stream Event carries.
type EventKind int

const (
	// EventConnected: the stream is open and authenticated ("ready").
	// Gap reports items that expired before this device acked them
	// (§10.4); the device then rejoins its groups (§11.9).
	EventConnected EventKind = iota
	// EventItem: an inbox item, stored or live.
	EventItem
	// EventPresence: a contact's presence change, as the raw frame.
	EventPresence
	// EventError: an error frame that doesn't close the stream.
	EventError
	// EventDisconnected: the connection dropped. The stream reconnects
	// with backoff; Err says why, and Retry when.
	EventDisconnected
	// EventStopped: the stream ended for good, and the channel closes
	// after it. Err is ErrDeviceRemoved after close code 4403, an
	// authentication error after 4401 that signing in again didn't fix, or
	// the context's error.
	EventStopped
)

func (k EventKind) String() string {
	return [...]string{"connected", "item", "presence", "error", "disconnected", "stopped"}[k]
}

// Event is one thing the live stream reports.
type Event struct {
	Kind     EventKind
	Item     *Item          // EventItem
	Gap      bool           // EventConnected
	Frame    jsontext.Value // EventPresence and EventError: the raw frame
	Err      error          // EventError, EventDisconnected, EventStopped
	Retry    time.Duration  // EventDisconnected: the wait before reconnecting
	Attempts int            // EventDisconnected: failed connections in a row
}

// StreamOptions adjust Stream.
type StreamOptions struct {
	// After is the cursor to replay stored items after. Empty means the
	// last cursor this device acked; the device fetches the inbox first if
	// it never acked anything.
	After string
	// Buffer is the event channel's size; 0 means 64.
	Buffer int
}

// Stream opens the live stream (GET stream, §8.2) and delivers inbox items
// and presence to the returned channel until ctx ends or the stream stops
// for good. It authenticates in the first frame, pings every 30 seconds,
// answers the server's pings, and reconnects with exponential backoff,
// replaying items after the last cursor it saw. The channel closes after
// an EventStopped.
//
// The caller acks items it has processed with Ack; the stream doesn't.
// Events are dropped only if the caller stops reading and ctx ends.
func (c *Client) Stream(ctx context.Context, opts StreamOptions) <-chan Event {
	size := opts.Buffer
	if size <= 0 {
		size = 64
	}
	ch := make(chan Event, size)
	go c.runStream(ctx, opts, ch)
	return ch
}

type streamStop struct{ err error }

func (s streamStop) Error() string { return s.err.Error() }

func (c *Client) runStream(ctx context.Context, opts StreamOptions, ch chan<- Event) {
	defer close(ch)
	send := func(e Event) bool {
		select {
		case ch <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	after := opts.After
	if after == "" {
		after = c.Cursor()
	}
	backoff := minBackoff
	failures := 0
	reauthed := false
	for {
		connected := false
		err := c.streamOnce(ctx, &after, func(e Event) bool {
			if e.Kind == EventConnected {
				connected, failures, backoff, reauthed = true, 0, minBackoff, false
			}
			return send(e)
		})
		if ctx.Err() != nil {
			send(Event{Kind: EventStopped, Err: ctx.Err()})
			return
		}
		var stop streamStop
		if errors.As(err, &stop) {
			send(Event{Kind: EventStopped, Err: stop.err})
			return
		}
		switch websocket.CloseStatus(err) {
		case closeNotAllowed:
			send(Event{Kind: EventStopped, Err: ErrDeviceRemoved})
			return
		case closeUnauthenticated:
			// The token may have expired: sign in once, then give up if
			// the server still refuses it. No backoff for the first try.
			if reauthed {
				send(Event{Kind: EventStopped, Err: fmt.Errorf("client: the stream refused this device's session: %w", err)})
				return
			}
			reauthed = true
			if serr := c.SignIn(ctx); serr != nil {
				send(Event{Kind: EventStopped, Err: serr})
				return
			}
			continue
		}
		if !connected {
			failures++
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff)/5+1))
		if !send(Event{Kind: EventDisconnected, Err: err, Retry: wait, Attempts: failures}) {
			return
		}
		select {
		case <-ctx.Done():
			send(Event{Kind: EventStopped, Err: ctx.Err()})
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// streamURL turns the client API base into the stream's WebSocket URL.
func streamURL(base string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + "stream"
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://") + "stream"
	}
	return base + "stream"
}

type frame struct {
	T     string `json:"t"`
	N     *int64 `json:"n,omitempty"`
	Gap   bool   `json:"gap,omitempty"`
	Item  *Item  `json:"item,omitempty"`
	Error string `json:"error,omitempty"`
	Msg   string `json:"msg,omitempty"`
}

// streamOnce runs one connection until it ends. It returns the reason,
// which is a streamStop for reasons reconnecting can't fix.
func (c *Client) streamOnce(ctx context.Context, after *string, emit func(Event) bool) error {
	if err := c.connect(ctx); err != nil {
		return err
	}
	if c.token() == "" {
		if err := c.SignIn(ctx); err != nil {
			if errors.Is(err, ErrDeviceRemoved) {
				return streamStop{err}
			}
			return err
		}
	}
	_ = c.flushRecoveryWrap(ctx) // best effort; it's kept until it works

	hc := *c.opts.httpClient()
	hc.Timeout = 0 // the WebSocket library uses the context instead
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, resp, err := websocket.Dial(dctx, streamURL(c.api.base), &websocket.DialOptions{
		HTTPClient:   &hc,
		Subprotocols: []string{wire.Subprotocol},
	})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return streamStop{fmt.Errorf("client: the homeserver has no stream: %w", err)}
		}
		return err
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != wire.Subprotocol {
		conn.Close(websocket.StatusCode(closeProtocol), "subprotocol")
		return streamStop{fmt.Errorf("client: the stream didn't agree to %s", wire.Subprotocol)}
	}
	conn.SetReadLimit(maxFrame)

	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var wmu sync.Mutex
	write := func(v any) error {
		b, err := wire.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(sctx, 10*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	// The first frame authenticates (API conventions).
	auth := map[string]string{"t": "auth", "token": c.token()}
	if *after != "" {
		auth["after"] = *after
	}
	if err := write(auth); err != nil {
		return err
	}

	// Ping every 30 seconds.
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		var n int64
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				n++
				if write(map[string]any{"t": "ping", "n": n}) != nil {
					stop()
					return
				}
			}
		}
	}()

	ready := false
	for {
		// A server that sends nothing for 90 seconds, not even a pong, is
		// gone.
		rctx, cancel := context.WithTimeout(sctx, silentLimit)
		typ, data, err := conn.Read(rctx)
		timedOut := rctx.Err() == context.DeadlineExceeded
		cancel()
		if err != nil {
			if timedOut {
				return errors.New("client: the stream went silent")
			}
			return err
		}
		if typ != websocket.MessageText {
			conn.Close(websocket.StatusCode(closeProtocol), "binary frame")
			return errors.New("client: the server sent a binary frame")
		}
		var f frame
		if err := wire.Unmarshal(data, &f); err != nil {
			conn.Close(websocket.StatusCode(closeProtocol), "bad frame")
			return fmt.Errorf("client: bad stream frame: %w", err)
		}
		switch f.T {
		case "ready":
			ready = true
			if !emit(Event{Kind: EventConnected, Gap: f.Gap}) {
				return ctx.Err()
			}
		case "item":
			if !ready || f.Item == nil {
				return errors.New("client: an item before ready")
			}
			if f.Item.Cursor != "" {
				*after = f.Item.Cursor
			}
			if !emit(Event{Kind: EventItem, Item: f.Item}) {
				return ctx.Err()
			}
		case "presence":
			if !emit(Event{Kind: EventPresence, Frame: jsontext.Value(data)}) {
				return ctx.Err()
			}
		case "ping":
			if f.N == nil {
				return errors.New("client: a ping without n")
			}
			if err := write(map[string]any{"t": "pong", "n": *f.N}); err != nil {
				return err
			}
		case "pong":
		case "error":
			e := &wire.Error{Code: f.Error, Msg: f.Msg}
			if !emit(Event{Kind: EventError, Err: e, Frame: jsontext.Value(data)}) {
				return ctx.Err()
			}
		default:
			// Unknown frame types are ignored (§20.2).
		}
	}
}
