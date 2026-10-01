package borgrec

import (
	"bytes"
	"sort"
)

// Token statuses the index reports (spec section 11.2). They are never
// admission verdicts: each needs the chain, which a topic manager does not
// read while it admits.
const (
	StatusConsistent      = "consistent"
	StatusPending         = "pending"
	StatusRollMismatch    = "roll-mismatch"
	StatusMembersMismatch = "members-mismatch"
	StatusCommitmentReuse = "commitment-reuse"
	StatusRejoinUnrotated = "rejoin-unrotated"
)

// Carrier statuses (spec section 11.2), the first that applies.
const (
	StatusRetracted  = "retracted"
	StatusSuperseded = "superseded"
	StatusHeld       = "held"
)

// OrgFold folds an organisation's chain from seq 0: the roll after each
// token. Once a token breaks the fold the chain is unusable from it on, and
// every later token reports the same status.
type OrgFold struct {
	roll   map[[32]byte]bool
	broken bool
}

// NewOrgFold starts a fold before seq 0.
func NewOrgFold() *OrgFold { return &OrgFold{roll: map[[32]byte]bool{}} }

// Apply folds the next token (the caller supplies them in seq order along
// spends) and returns its status: roll-mismatch when it revokes a commitment
// the roll does not hold, admits one it already holds, or does not reproduce
// count and roll; consistent otherwise.
func (f *OrgFold) Apply(o *Org) string {
	if f.broken {
		return StatusRollMismatch
	}
	for _, c := range o.Revoke {
		if !f.roll[c] {
			f.broken = true
			return StatusRollMismatch
		}
		delete(f.roll, c)
	}
	for _, c := range o.Admit {
		if f.roll[c] {
			f.broken = true
			return StatusRollMismatch
		}
		f.roll[c] = true
	}
	if uint64(len(f.roll)) != o.Count || RollRoot(f.Roll()) != o.Roll {
		f.broken = true
		return StatusRollMismatch
	}
	return StatusConsistent
}

// Roll is the roll after the last token folded, ascending.
func (f *OrgFold) Roll() [][32]byte {
	out := make([][32]byte, 0, len(f.roll))
	for c := range f.roll {
		out = append(out, c)
	}
	sortCommitments(out)
	return out
}

// GroupFold folds a group's chain from seq 0: the members after each token,
// every key any token removed, and every epoch's commitment.
type GroupFold struct {
	members     map[string]bool
	removed     map[string]bool
	commitments map[[32]byte]bool
	broken      bool
}

// NewGroupFold starts a fold before seq 0.
func NewGroupFold() *GroupFold {
	return &GroupFold{members: map[string]bool{}, removed: map[string]bool{}, commitments: map[[32]byte]bool{}}
}

// Apply folds the next token and returns its status, the first that
// applies: members-mismatch (it removes a non-member, adds a member, or does
// not reproduce count and members; sticky), commitment-reuse (it begins an
// epoch whose commitment an earlier epoch had), rejoin-unrotated (it adds a
// key an earlier token removed without beginning an epoch), consistent.
func (f *GroupFold) Apply(g *Group) string {
	if f.broken {
		return StatusMembersMismatch
	}
	rejoin := RejoinUnrotated(f.removed, g)
	if Fold(f.members, g) != nil {
		f.broken = true
		return StatusMembersMismatch
	}
	for _, k := range g.Remove {
		f.removed[string(k)] = true
	}
	reuse := g.BeginsEpoch() && f.commitments[g.Commitment]
	if g.BeginsEpoch() {
		f.commitments[g.Commitment] = true
	}
	switch {
	case reuse:
		return StatusCommitmentReuse
	case rejoin:
		return StatusRejoinUnrotated
	}
	return StatusConsistent
}

// Members is the member set after the last token folded, ascending.
func (f *GroupFold) Members() [][]byte {
	out := make([][]byte, 0, len(f.members))
	for k := range f.members {
		out = append(out, []byte(k))
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// OutpointRow is the index's row for one funding outpoint (spec section
// 11.2): the commitments of the carriers admitted on it, whether its winner
// was dropped, and whether an admitted sweep in the host's best chain spends
// it.
type OutpointRow struct {
	Commitments   [][32]byte
	WinnerDropped bool
	Swept         bool
}

// Winner is the outpoint's answered carrier: the lowest commitment, compared
// as 32 bytes in hash byte order. ok is false for an empty row.
func (r *OutpointRow) Winner() (c [32]byte, ok bool) {
	for i, x := range r.Commitments {
		if i == 0 || bytes.Compare(x[:], c[:]) < 0 {
			c = x
		}
	}
	return c, len(r.Commitments) > 0
}

// Status is a carrier's status on its row: retracted, then superseded, then
// held. It depends on the admitted set and the row, never on arrival order.
func (r *OutpointRow) Status(c [32]byte) string {
	if r.Swept {
		return StatusRetracted
	}
	if w, ok := r.Winner(); !ok || w != c || r.WinnerDropped {
		return StatusSuperseded
	}
	return StatusHeld
}
