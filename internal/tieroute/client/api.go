package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// maxBody bounds a response body: 1 MiB of JSON (API conventions), with
// room for base64url overhead on the larger endpoints.
const maxBody = 4 << 20

// Clock tracks the homeserver's time, which the client uses for every
// timestamp it signs (§8.4). It keeps the difference between the server's
// clock and the device's.
type Clock struct {
	mu      sync.Mutex
	offset  time.Duration
	known   bool // any reading
	precise bool // a reading from auth/challenge or clock-skew
	now     func() time.Time
}

func (c *Clock) local() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Now returns the homeserver's time, as far as the client knows it.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local().Add(c.offset)
}

// Skew returns how far the device's clock is from the homeserver's, and
// whether it's known. A client warns when it's over a minute (§8.4).
func (c *Clock) Skew() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return -c.offset, c.known
}

// observe records the server's time from auth/challenge or a clock-skew
// error, measured against the middle of the request (§8.4).
func (c *Clock) observe(server time.Time, start, end time.Time) {
	mid := start.Add(end.Sub(start) / 2)
	c.mu.Lock()
	c.offset, c.known, c.precise = server.Sub(mid), true, true
	c.mu.Unlock()
}

// observeRough records the server's time from a Date header, which has
// one-second resolution, until a precise reading arrives.
func (c *Clock) observeRough(server time.Time, start, end time.Time) {
	mid := start.Add(end.Sub(start) / 2)
	c.mu.Lock()
	if !c.precise {
		c.offset, c.known = server.Sub(mid), true
	}
	c.mu.Unlock()
}

// api calls one homeserver's client API (§8.1).
type api struct {
	base  string // the client API base URL, ending in "/"
	http  *http.Client
	clock *Clock
	log   *slog.Logger
	// token returns the bearer token; empty means unauthenticated.
	token func() string
}

// retries is how often a call is retried after 429, 503, or a network
// error, with exponential backoff (API conventions).
const retries = 3

func (a *api) url(path string, query url.Values) string {
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// call makes one request. in is the JSON body for POST, and out receives
// the response, if not nil. A non-2xx answer comes back as *wire.Error.
func (a *api) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if method == http.MethodPost {
		if in == nil {
			in = struct{}{}
		}
		b, err := wire.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		err := a.once(ctx, method, path, query, body, out)
		if err == nil || attempt >= retries {
			return err
		}
		var wait time.Duration
		var e *wire.Error
		switch {
		case errors.As(err, &e) && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable):
			if e.Code != wire.CodeRateLimited && e.Code != wire.CodeUnavailable {
				return err
			}
			if e.Code == wire.CodeRateLimited && e.Retry == 0 {
				return err // a cap, not a rate (§7.4): retrying won't help
			}
			wait = max(time.Duration(e.Retry)*time.Second, backoff)
		case errors.As(err, &e), ctx.Err() != nil:
			return err
		default:
			wait = backoff // a network error
		}
		if wait > time.Minute {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff *= 2
	}
}

func (a *api) once(ctx context.Context, method, path string, query url.Values, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url(path, query), rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", wire.MediaType)
	}
	req.Header.Set("Accept", wire.MediaType)
	if a.token != nil {
		if t := a.token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	start := time.Now()
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	end := time.Now()
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil && a.clock != nil {
		a.clock.observeRough(d, start, end)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	if a.log != nil {
		// Paths and statuses only: tokens and bodies stay out of logs.
		a.log.Debug("tieroute request", "method", method, "path", path, "status", resp.StatusCode)
	}
	if len(data) > maxBody {
		return fmt.Errorf("client: %s %s: response too large", method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &wire.Error{Status: resp.StatusCode}
		if err := wire.Unmarshal(data, e); err != nil || e.Code == "" {
			e.Code = statusCode(resp.StatusCode)
		}
		if e.Code == wire.CodeClockSkew && e.Now > 0 && a.clock != nil {
			a.clock.observe(time.Unix(e.Now, 0), start, end)
		}
		return e
	}
	if out == nil || resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, wire.MediaType) && !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("client: %s %s: unexpected content type %q", method, path, ct)
	}
	if err := wire.Unmarshal(data, out); err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	return nil
}

// statusCode names an error for a response without an error body.
func statusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return wire.CodeBadRequest
	case http.StatusUnauthorized:
		return wire.CodeUnauthenticated
	case http.StatusForbidden:
		return wire.CodeNotAllowed
	case http.StatusNotFound:
		return wire.CodeNotFound
	case http.StatusTooManyRequests:
		return wire.CodeRateLimited
	case http.StatusNotImplemented:
		return wire.CodeNotImplemented
	default:
		return wire.CodeUnavailable
	}
}
