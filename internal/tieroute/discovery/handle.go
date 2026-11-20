package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/rerouteholdings/railcar/internal/tieroute/identity"
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// HandleRecord is what a handle's record says: the identity it points to,
// and a hint for its home. The chain is authoritative (§5.3).
type HandleRecord struct {
	ID   string `json:"id"`
	Home string `json:"home"`
}

// ErrNoHandle reports a handle with no record, or a record that doesn't
// resolve.
var ErrNoHandle = errors.New("discovery: the handle doesn't resolve")

func (h HandleRecord) check() error {
	if !identity.ValidIdentifier(h.ID) {
		return fmt.Errorf("%w: bad id", ErrNoHandle)
	}
	if err := identity.CheckServerName(h.Home); err != nil {
		return fmt.Errorf("%w: bad home", ErrNoHandle)
	}
	return nil
}

// ParseTXT reads _tieroute TXT records (§5.3). Each record's strings are
// joined first. Its text is space-separated key=value pairs; id and home
// are required, and unknown keys are ignored. If records with an id
// disagree, the handle doesn't resolve.
func ParseTXT(records [][]string) (HandleRecord, error) {
	var found *HandleRecord
	for _, parts := range records {
		var rec HandleRecord
		hasID := false
		for _, kv := range strings.Fields(strings.Join(parts, "")) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			switch k {
			case "id":
				rec.ID, hasID = v, true
			case "home":
				rec.Home = v
			}
		}
		if !hasID {
			continue
		}
		if found != nil && (found.ID != rec.ID || found.Home != rec.Home) {
			return HandleRecord{}, fmt.Errorf("%w: TXT records disagree", ErrNoHandle)
		}
		r := rec
		found = &r
	}
	if found == nil {
		return HandleRecord{}, ErrNoHandle
	}
	return *found, found.check()
}

// LookupHandle reads a handle's record directly: the HTTPS document if it
// can be fetched, which then decides, and otherwise the DNS TXT record at
// _tieroute.{handle} (§5.3). Clients usually resolve through their
// homeserver instead (GET resolve/{handle}); either way the chain is
// checked afterward. Go's resolver doesn't validate DNSSEC, so a handle
// published only in DNS is as safe as the network's DNS.
func (r *Resolver) LookupHandle(ctx context.Context, handle string) (HandleRecord, error) {
	if err := identity.CheckHandle(handle); err != nil {
		return HandleRecord{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Origin(handle)+HandlePath, nil)
	if err != nil {
		return HandleRecord{}, err
	}
	if resp, err := r.client().Do(req); err == nil {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if rerr == nil && resp.StatusCode == http.StatusOK {
			var rec HandleRecord
			if err := wire.Unmarshal(body, &rec); err != nil {
				return HandleRecord{}, fmt.Errorf("%w: %v", ErrNoHandle, err)
			}
			return rec, rec.check()
		}
	}
	if r.NoDNS {
		return HandleRecord{}, fmt.Errorf("%w: no HTTPS document, and no DNS lookups through a proxy", ErrNoHandle)
	}
	txts, err := net.DefaultResolver.LookupTXT(ctx, "_tieroute."+handle)
	if err != nil {
		return HandleRecord{}, fmt.Errorf("%w: %v", ErrNoHandle, err)
	}
	// Go returns each record's strings already joined.
	recs := make([][]string, len(txts))
	for i, t := range txts {
		recs[i] = []string{t}
	}
	return ParseTXT(recs)
}
