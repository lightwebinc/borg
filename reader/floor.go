package reader

import (
	"encoding/hex"
	"fmt"
)

// A reader with no memory believes whatever the hosts it asks agree on: a
// host set that withholds the latest tokens of a chain answers an older
// state, every token of which verifies. Only the reader's own record of
// what it verified before shows that (spec section 13). The package keeps
// no such record itself: it reads no file and holds no state between
// calls. A caller that has somewhere to keep one gives the client a Floor.

// Floor is a caller's memory of the chains it verified: for each chain, the
// seq and txid of the latest token a read of it reached. Chain reads it
// before it returns and raises it after. An implementation persists what
// See is given before the caller acts on the chain read, and never lowers
// a seq on its own: forgetting is the caller's decision.
type Floor interface {
	// Seen is the latest token of the chain this caller verified before:
	// ok is false when it remembers none. org is the organization's key in
	// hex, chain ChainOrg or a group id.
	Seen(hall, org, chain string) (seq uint64, txid string, ok bool)
	// See records the latest token of the chain verified now.
	See(hall, org, chain string, seq uint64, txid string)
}

// RollbackError is a chain the hosts answer only below the seq the
// caller's Floor remembers: an older state than one it verified.
type RollbackError struct {
	// Answered is the seq of the latest token read now; Seq and Txid the
	// token the floor remembers.
	Answered uint64
	Seq      uint64
	Txid     string
}

func (e *RollbackError) Error() string {
	return fmt.Sprintf("the hosts answer the chain to seq %d, and this reader verified seq %d (%s): an older state; ask other hosts", e.Answered, e.Seq, e.Txid)
}

// hold holds the chain read to the floor, and raises the floor to it.
func (v *ChainView) hold(f Floor) error {
	org := hex.EncodeToString(v.Org)
	if seq, txid, ok := f.Seen(v.Hall, org, v.Chain); ok {
		if uint64(len(v.Tokens)) <= seq {
			return &RollbackError{Answered: v.Latest().Seq(), Seq: seq, Txid: txid}
		}
		if v.Tokens[seq].Txid != txid {
			v.Replaced = txid
		}
	}
	t := v.Latest()
	f.See(v.Hall, org, v.Chain, t.Seq(), t.Txid)
	return nil
}
