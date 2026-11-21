// Package discovery finds Tieroute servers and handles: the server document
// with its API base URLs (§5.1, §8.1), and handle records (§5.3).
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/sig"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// ServerPath is the one fixed path in the protocol (§5.1).
const ServerPath = "/.well-known/tieroute/server"

// HandlePath is where a handle's HTTPS document lives (§5.3).
const HandlePath = "/.well-known/tieroute/handle"

// MaxCache is the longest a reader caches a server document (§5.1).
const MaxCache = 24 * time.Hour

// defaultCache is how long a document without caching headers is kept.
const defaultCache = 5 * time.Minute

// maxDocument bounds a server document's size (1 MiB, as for any body).
const maxDocument = 1 << 20

// Roles a server document lists (§5.1).
const (
	RoleHome      = "home"
	RoleCommunity = "community"
)

// APIBase is one wire version's API locations (§5.1).
type APIBase struct {
	V         int    `json:"v"`
	Client    string `json:"client,omitempty"`
	S2S       string `json:"s2s"`
	Community string `json:"community,omitempty"`
}

// ServerKey is one of a server's signing keys (§5.1).
type ServerKey struct {
	Kid string     `json:"kid"`
	Pub wire.Bytes `json:"pub"`
	NBF int64      `json:"nbf"`
	Exp int64      `json:"exp"`
	// Revoked, when set, is the time from which nothing the key signs
	// counts (§5.1).
	Revoked int64 `json:"revoked,omitempty"`
}

// maxKeyLife is the longest a server key may last, from nbf to exp: a
// key that claims longer is ignored (§5.1).
const maxKeyLife = 400 * 24 * 60 * 60

// ServerDoc is a server document (§5.1).
type ServerDoc struct {
	Name         string      `json:"name"`
	Roles        []string    `json:"roles"`
	APIs         []APIBase   `json:"apis"`
	Keys         []ServerKey `json:"keys"`
	Contact      string      `json:"contact,omitempty"`
	Handles      string      `json:"handles,omitempty"`
	Registration string      `json:"registration,omitempty"`
	Invite       string      `json:"invite,omitempty"`

	// Date is the server's clock from the response's Date header, or zero.
	Date time.Time `json:"-"`
}

// HasRole reports whether the document lists a role.
func (d *ServerDoc) HasRole(role string) bool {
	for _, r := range d.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// API returns the API base for wire version v.
func (d *ServerDoc) API(v int) (APIBase, bool) {
	for _, a := range d.APIs {
		if a.V == v {
			return a, true
		}
	}
	return APIBase{}, false
}

// ErrNoVersion2 reports a server that doesn't list wire version 2.
var ErrNoVersion2 = errors.New("discovery: the server doesn't speak wire version 2")

// Resolver fetches server documents and handle records, and caches server
// documents.
type Resolver struct {
	// HTTP is the client to use; nil means http.DefaultClient.
	HTTP *http.Client
	// NoDNS skips the DNS lookups the resolver would make itself, for a
	// client whose connections go through a proxy that resolves names.
	NoDNS bool
	// TXT, when set, looks TXT records up in place of the system's
	// resolver, such as over DNS over HTTPS through a proxy.
	TXT func(ctx context.Context, name string) ([]string, error)
	// Origins maps server names to the origins to reach them at, such as
	// "a.test" to "http://127.0.0.1:18471". It's for local test networks,
	// which are an implementation feature, not protocol (§5.1). A server
	// reached this way may use plain HTTP API base URLs.
	Origins map[string]string
	// Now returns the time; nil means time.Now.
	Now func() time.Time
	// Via, when set, fetches the documents of servers other than Self
	// through another server, such as a client's homeserver
	// (GET servers/{name}), so those servers don't learn the device's
	// address. It returns the document's bytes.
	Via func(ctx context.Context, name string) ([]byte, error)
	// Self is the server whose document is always fetched directly.
	Self string

	mu     sync.Mutex
	direct map[string]bool
	docs   map[string]cachedDoc
	keys   map[string]map[string]ServerKey // server → kid → key, kept until exp
}

type cachedDoc struct {
	doc     *ServerDoc
	expires time.Time
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

// Origin returns the origin a server name is reached at: https://name, or
// the override in Origins.
func (r *Resolver) Origin(name string) string {
	if o, ok := r.Origins[name]; ok {
		return strings.TrimSuffix(o, "/")
	}
	return "https://" + name
}

func (r *Resolver) dev(name string) bool {
	_, ok := r.Origins[name]
	return ok
}

// ServerDoc returns a server's document, from the cache when it's fresh.
func (r *Resolver) ServerDoc(ctx context.Context, name string) (*ServerDoc, error) {
	r.mu.Lock()
	if c, ok := r.docs[name]; ok && r.now().Before(c.expires) {
		r.mu.Unlock()
		return c.doc, nil
	}
	r.mu.Unlock()
	return r.Refresh(ctx, name)
}

// Refresh fetches a server's document again, bypassing the cache.
func (r *Resolver) Refresh(ctx context.Context, name string) (*ServerDoc, error) {
	if err := identity.CheckServerName(name); err != nil {
		return nil, err
	}
	doc, ttl, err := r.fetchDoc(ctx, name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.docs == nil {
		r.docs = map[string]cachedDoc{}
		r.keys = map[string]map[string]ServerKey{}
	}
	r.docs[name] = cachedDoc{doc: doc, expires: r.now().Add(ttl)}
	// Keep every key seen until its exp, so signatures made shortly before
	// a rotation still verify (§5.1).
	ks := r.keys[name]
	if ks == nil {
		ks = map[string]ServerKey{}
		r.keys[name] = ks
	}
	now := r.now().Unix()
	for kid, k := range ks {
		if k.Exp < now {
			delete(ks, kid)
		}
	}
	for _, k := range doc.Keys {
		if k.Exp-k.NBF > maxKeyLife {
			continue
		}
		if old, ok := ks[k.Kid]; ok && old.Revoked != 0 && (k.Revoked == 0 || old.Revoked < k.Revoked) {
			k.Revoked = old.Revoked // a revocation never comes undone
		}
		ks[k.Kid] = k
	}
	return doc, nil
}

// Direct makes the resolver fetch a server's document from the server
// itself, not through Via. A device that connects to a server anyway, as
// it does to a community server (§16.1), hides nothing by asking another.
func (r *Resolver) Direct(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.direct == nil {
		r.direct = map[string]bool{}
	}
	if !r.direct[name] {
		r.direct[name] = true
		delete(r.docs, name)
	}
}

func (r *Resolver) isDirect(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.direct[name]
}

func (r *Resolver) fetchDoc(ctx context.Context, name string) (*ServerDoc, time.Duration, error) {
	if r.Via != nil && name != r.Self && !r.isDirect(name) {
		body, err := r.Via(ctx, name)
		if err != nil {
			return nil, 0, fmt.Errorf("discovery: %s: %w", name, err)
		}
		// A local test network's servers list plain HTTP bases, and so do
		// the documents relayed by its homeserver.
		doc, err := ParseServerDoc(body, name, r.dev(name) || r.dev(r.Self))
		if err != nil {
			return nil, 0, err
		}
		return doc, defaultCache, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Origin(name)+ServerPath, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("discovery: %s: %w", name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("discovery: %s: server document: HTTP %d", name, resp.StatusCode)
	}
	if len(body) > maxDocument {
		return nil, 0, fmt.Errorf("discovery: %s: server document too large", name)
	}
	doc, err := ParseServerDoc(body, name, r.dev(name))
	if err != nil {
		return nil, 0, err
	}
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		doc.Date = d
	}
	return doc, cacheTTL(resp.Header), nil
}

// cacheTTL reads Cache-Control max-age, capped at 24 hours (§5.1).
func cacheTTL(h http.Header) time.Duration {
	ttl := defaultCache
	for _, part := range strings.Split(h.Get("Cache-Control"), ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "no-store" || part == "no-cache" {
			return 0
		}
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(ttl, MaxCache)
}

// ParseServerDoc decodes and checks a server document fetched for name.
// Keys that aren't composite, such as wire version 1 keys a server lists
// during the migration, are dropped. allowHTTP permits plain HTTP base URLs,
// for local test networks.
func ParseServerDoc(data []byte, name string, allowHTTP bool) (*ServerDoc, error) {
	var doc ServerDoc
	if err := wire.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("discovery: %s: server document: %w", name, err)
	}
	if doc.Name != name {
		return nil, fmt.Errorf("discovery: %s: the server document names %q", name, doc.Name)
	}
	if len(doc.APIs) == 0 {
		return nil, fmt.Errorf("discovery: %s: the server document lists no APIs", name)
	}
	for _, a := range doc.APIs {
		for _, u := range []string{a.Client, a.S2S, a.Community} {
			if u == "" {
				continue
			}
			if err := checkBase(u, allowHTTP); err != nil {
				return nil, fmt.Errorf("discovery: %s: %w", name, err)
			}
		}
	}
	var keys []ServerKey
	for _, k := range doc.Keys {
		if len(k.Pub) == sig.CompositePublicSize && k.Kid != "" {
			keys = append(keys, k)
		}
	}
	doc.Keys = keys
	return &doc, nil
}

// checkBase checks an API base URL: absolute https, ending in "/" (§8.1).
func checkBase(s string, allowHTTP bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !strings.HasSuffix(u.Path, "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("bad API base URL %q", s)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return fmt.Errorf("API base URL %q isn't https", s)
	}
	return nil
}

// Key returns the composite key kid that server name lists, valid at time
// at (§5.1, §8.3). It refetches the document once if the key is unknown.
func (r *Resolver) Key(ctx context.Context, name, kid string, at time.Time) ([]byte, error) {
	look := func() (ServerKey, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		k, ok := r.keys[name][kid]
		return k, ok
	}
	k, ok := look()
	if !ok {
		if _, err := r.Refresh(ctx, name); err != nil {
			return nil, err
		}
		if k, ok = look(); !ok {
			return nil, fmt.Errorf("discovery: %s lists no key %q", name, kid)
		}
	}
	if t := at.Unix(); t < k.NBF || t > k.Exp {
		return nil, fmt.Errorf("discovery: %s's key %q isn't valid then", name, kid)
	}
	return k.Pub, nil
}

// VerifyServerSigned checks an object a server signed with kid, such as a
// move reservation (§6.6), at time at.
func (r *Resolver) VerifyServerSigned(ctx context.Context, name string, s wire.Signed, at time.Time) error {
	return r.VerifyServerSignedAt(ctx, name, s, at, at.Unix())
}

// VerifyServerSignedAt is VerifyServerSigned for an object that says when
// it was signed, such as a commit receipt's ts: a key revoked at or before
// that time doesn't count for it (§5.1). A key the cached document doesn't
// show revoked is checked against a fresh copy once.
func (r *Resolver) VerifyServerSignedAt(ctx context.Context, name string, s wire.Signed, at time.Time, signed int64) error {
	pub, err := r.Key(ctx, name, s.Kid, at)
	if err != nil {
		return err
	}
	if r.revoked(name, s.Kid, signed) {
		return fmt.Errorf("discovery: %s revoked its key %q before this was signed", name, s.Kid)
	}
	return sig.VerifyServer(s, s.Kid, pub)
}

func (r *Resolver) revoked(name, kid string, signed int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.keys[name][kid]
	return k.Revoked != 0 && k.Revoked <= signed
}
