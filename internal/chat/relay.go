package chat

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/rerouteholdings/railcar/internal/tieroute/client"
)

// relayRefresh is how far into the credentials' ttl a call or voice
// channel fetches new ones (client API, "Call relays"). Tests shorten it.
var relayRefresh = func(ttl time.Duration) time.Duration { return ttl * 3 / 4 }

// relayRetry is how long to wait after a failed fetch before trying again.
const relayRetry = 30 * time.Second

// relayFetches counts fetches of new credentials during calls and voice
// channels, for tests.
var relayFetches atomic.Int64

// relayLease is when a call's or voice channel's relay credentials are
// due for renewal.
type relayLease struct {
	next time.Time // zero: they don't expire, or there's no relay
	busy bool      // a fetch is under way
}

// set starts the lease for credentials fetched now.
func (l *relayLease) set(r *client.Relay) {
	l.busy = false
	l.next = time.Time{}
	if r != nil && r.TTL > 0 {
		l.next = time.Now().Add(relayRefresh(time.Duration(r.TTL) * time.Second))
	}
}

// due reports whether new credentials are due, and if so marks a fetch as
// started.
func (l *relayLease) due(now time.Time) bool {
	if l.next.IsZero() || l.busy || now.Before(l.next) {
		return false
	}
	l.busy = true
	return true
}

// failed notes a fetch that failed, to try again later.
func (l *relayLease) failed() {
	l.busy = false
	l.next = time.Now().Add(relayRetry)
}

// iceServers are the ICE servers for a relay's credentials.
func iceServers(r *client.Relay) []webrtc.ICEServer {
	return []webrtc.ICEServer{{URLs: r.URLs, Username: r.Username, Credential: r.Credential}}
}

// renewRelay fetches new relay credentials away from the caller's
// goroutine, then gives them to done, which runs on that goroutine
// through do.
func (e *Engine) renewRelay(do func(func()), done func(*client.Relay, error)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		r, err := e.c.Turn(ctx)
		cancel()
		relayFetches.Add(1)
		do(func() { done(r, err) })
	}()
}
