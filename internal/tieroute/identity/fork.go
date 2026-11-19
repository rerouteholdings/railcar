package identity

import (
	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// Outcome is what comparing a cached chain with a candidate decides
// (§6.4.1, §6.5).
type Outcome int

const (
	// Extends: the candidate extends the cached chain. Take it.
	Extends Outcome = iota
	// Same: the candidate is the cached chain or a prefix of it, such as an
	// out-of-date answer. Keep the cached chain.
	Same
	// Fork: the candidate is a recovery fork that overrides device-signed
	// entries of the cached chain. Take it, and throw the overridden
	// entries out.
	Fork
	// Reject: keep the cached chain and refuse the candidate.
	Reject
)

func (o Outcome) String() string {
	return [...]string{"extends", "same", "fork", "reject"}[o]
}

// Warnings a client shows about a rejected candidate (§6.4.1).
const (
	// WarnUnverified: a fork that isn't a valid recovery fork. Treat the
	// identity as unverified until a recovery entry resolves it.
	WarnUnverified = "unverified"
	// WarnRecoveryCompromised: two recovery-signed branches, so the
	// recovery code itself may have leaked.
	WarnRecoveryCompromised = "recovery-compromised"
)

// Comparison is the result of Compare.
type Comparison struct {
	Outcome Outcome
	ForkAt  int    // with Fork or Reject of a fork: the seq where the branches part
	Warning string // with Reject: a warning to show, if any
	Chain   *Chain // the chain to keep: the candidate for Extends and Fork, else the cached one
	Err     error  // with Reject of an invalid candidate: why
}

// Compare decides whether a candidate chain replaces the cached one
// (§6.4.1, §6.5). It accepts only a candidate that extends the cache, or a
// recovery-signed entry R that forks from it where every entry R overrides
// is device-signed. Which branch stands doesn't depend on which arrived
// first. Vouching (§6.5) is a separate check.
func Compare(cached *Chain, candidate []wire.Signed, opts Options) Comparison {
	cand, err := Validate(candidate, opts)
	if err != nil {
		return Comparison{Outcome: Reject, Chain: cached, Err: err}
	}
	if cached == nil {
		return Comparison{Outcome: Extends, Chain: cand}
	}
	if cand.ID() != cached.ID() {
		return Comparison{Outcome: Reject, Chain: cached, Err: ErrInvalidChain, Warning: WarnUnverified}
	}
	p := 0
	for p < cached.Len() && p < cand.Len() && cached.Info[p].Hash == cand.Info[p].Hash {
		p++
	}
	switch {
	case p == cand.Len():
		return Comparison{Outcome: Same, Chain: cached}
	case p == cached.Len():
		return Comparison{Outcome: Extends, Chain: cand}
	}
	cachedRec := hasRecovery(cached, p)
	candRec := hasRecovery(cand, p)
	if cand.Info[p].Recovery && !cachedRec {
		return Comparison{Outcome: Fork, ForkAt: p, Chain: cand}
	}
	c := Comparison{Outcome: Reject, ForkAt: p, Chain: cached}
	switch {
	case cachedRec && candRec:
		c.Warning = WarnRecoveryCompromised
	case !cachedRec && !candRec:
		c.Warning = WarnUnverified
	}
	return c
}

// hasRecovery reports whether any entry from seq p on is recovery-signed.
func hasRecovery(c *Chain, p int) bool {
	for _, i := range c.Info[p:] {
		if i.Recovery {
			return true
		}
	}
	return false
}
