package identity

import (
	"errors"
	"fmt"

	"github.com/rerouteholdings/railcar/internal/tieroute/wire"
)

// HomeAnswer is what a previous home answers when asked for an identity's
// chain (S2S identity/fetch, §6.5).
type HomeAnswer struct {
	Chain       []wire.Signed // the chain it returned
	Gone        bool          // it answered 404 or 410: it never knew the identity
	Unreachable bool          // it couldn't be reached, or gave no usable answer
}

// Asker asks a server for an identity's chain.
type Asker func(server string) HomeAnswer

// memo asks each server at most once.
func memo(ask Asker) Asker {
	seen := map[string]HomeAnswer{}
	return func(server string) HomeAnswer {
		if a, ok := seen[server]; ok {
			return a
		}
		a := ask(server)
		seen[server] = a
		return a
	}
}

// contains reports whether a chain holds an entry with this hash at seq.
func contains(chain []wire.Signed, seq uint64, hash [32]byte) bool {
	return seq < uint64(len(chain)) && chain[seq].Hash() == hash
}

// Vouch decides how many of a validated candidate's entries count, when it
// arrived from server from and the receiver already holds its first start
// entries (§6.5). A device-signed entry beyond them counts if its previous
// home is from, or the previous home confirms it. A recovery-signed entry
// counts from any server, and vouches for every entry before it.
func Vouch(cand *Chain, start int, from string, ask Asker) int {
	ask = memo(ask)
	accepted := start
	pending := false // an entry since accepted couldn't be confirmed
	for i := start; i < cand.Len(); i++ {
		info := cand.Info[i]
		switch {
		case info.Recovery:
			accepted, pending = i+1, false
		case pending:
			// Wait for a recovery-signed entry to vouch for the gap.
		case confirmed(info, from, ask):
			accepted = i + 1
		default:
			pending = true
		}
	}
	return accepted
}

func confirmed(info EntryInfo, from string, ask Asker) bool {
	if info.HomeBefore == from {
		return true
	}
	a := ask(info.HomeBefore)
	return !a.Gone && !a.Unreachable && contains(a.Chain, info.Seq, info.Hash)
}

// ErrUnconfirmed reports a first fetch whose device-signed move a previous
// home couldn't confirm.
var ErrUnconfirmed = errors.New("identity: a move in the chain isn't confirmed by its previous home")

// ErrRetryLater reports a first fetch that can't be decided yet: a
// previous home couldn't be reached. Ask again later.
var ErrRetryLater = errors.New("identity: a previous home can't be reached to confirm a move; try again later")

// FirstFetch checks a validated chain the receiver holds nothing of yet
// (§6.5): it has to come from its current home, and every device-signed
// set-home that no later recovery-signed entry vouches for is confirmed with
// the home before it. A home that answers 404 or 410 is skipped.
func FirstFetch(cand *Chain, from string, ask Asker) error {
	if cand.State.Home != from {
		return fmt.Errorf("%w: a first copy comes from the identity's home, %s, not %s", ErrInvalidChain, cand.State.Home, from)
	}
	ask = memo(ask)
	lastRec := int(cand.State.LastRecoverySeq)
	for i := lastRec + 1; i < cand.Len(); i++ {
		info := cand.Info[i]
		if i == 0 || info.Recovery || !movesHome(info) || info.HomeBefore == from {
			continue
		}
		a := ask(info.HomeBefore)
		switch {
		case a.Gone:
			continue
		case a.Unreachable:
			return ErrRetryLater
		case !contains(a.Chain, info.Seq, info.Hash):
			return ErrUnconfirmed
		}
	}
	return nil
}

func movesHome(info EntryInfo) bool {
	for _, op := range info.Body.Ops {
		if op.Op == OpSetHome {
			return true
		}
	}
	return false
}
