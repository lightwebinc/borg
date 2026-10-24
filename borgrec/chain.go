package borgrec

import (
	"bytes"
	"fmt"
	"sort"
)

// Token statuses the index reports (spec section 11.2). They are never
// admission verdicts: each needs the chain, which a topic manager does not
// read while it admits.
const (
	StatusConsistent      = "consistent"
	StatusPending         = "pending"
	StatusRollMismatch    = "roll-mismatch"
	StatusPolicyMismatch  = "policy-mismatch"
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

// OrgFold folds an organization's chain from seq 0: the roll after each
// token. Once a token breaks the fold the chain is unusable from it on, and
// every later token reports the same status.
type OrgFold struct {
	roll         map[[32]byte]bool
	policy       map[[32]byte]bool
	broken       bool
	policyBroken bool
}

// NewOrgFold starts a fold before seq 0.
func NewOrgFold() *OrgFold {
	return &OrgFold{roll: map[[32]byte]bool{}, policy: map[[32]byte]bool{}}
}

// Apply folds the next token (the caller supplies them in seq order along
// spends) and returns its status: roll-mismatch when it revokes a commitment
// the roll does not hold, admits one it already holds, or does not reproduce
// count and roll; policy-mismatch when the same happens to the policy set;
// consistent otherwise. Both are sticky, and the roll is reported first, so
// a chain that breaks once reports the same status from there on.
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
	if f.policyBroken {
		return StatusPolicyMismatch
	}
	for _, c := range o.Retire {
		if !f.policy[c] {
			f.policyBroken = true
			return StatusPolicyMismatch
		}
		delete(f.policy, c)
	}
	for _, c := range o.Define {
		if f.policy[c] {
			f.policyBroken = true
			return StatusPolicyMismatch
		}
		f.policy[c] = true
	}
	if uint64(len(f.policy)) != o.PolicyCount || PolicyRoot(f.Policy()) != o.Policy {
		f.policyBroken = true
		return StatusPolicyMismatch
	}
	return StatusConsistent
}

// Policy is the policy set after the last token folded, ascending.
func (f *OrgFold) Policy() [][32]byte {
	out := make([][32]byte, 0, len(f.policy))
	for c := range f.policy {
		out = append(out, c)
	}
	sortCommitments(out)
	return out
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

// Holds reports whether the roll after the last token folded holds the
// commitment c.
func (f *OrgFold) Holds(c [32]byte) bool { return f.roll[c] }

// Clone is a copy of the fold, to fold a second successor on: a host's
// index keeps the fold after a token while a successor of it may still
// arrive.
func (f *OrgFold) Clone() *OrgFold {
	out := &OrgFold{roll: make(map[[32]byte]bool, len(f.roll)), policy: make(map[[32]byte]bool, len(f.policy)), broken: f.broken, policyBroken: f.policyBroken}
	for c := range f.roll {
		out.roll[c] = true
	}
	for c := range f.policy {
		out.policy[c] = true
	}
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

// Holds reports whether key is a member after the last token folded.
func (f *GroupFold) Holds(key []byte) bool { return f.members[string(key)] }

// Clone is a copy of the fold, to fold a second successor on.
func (f *GroupFold) Clone() *GroupFold {
	out := &GroupFold{members: make(map[string]bool, len(f.members)), removed: make(map[string]bool, len(f.removed)),
		commitments: make(map[[32]byte]bool, len(f.commitments)), broken: f.broken}
	for k := range f.members {
		out.members[k] = true
	}
	for k := range f.removed {
		out.removed[k] = true
	}
	for c := range f.commitments {
		out.commitments[c] = true
	}
	return out
}

// Link is one token of a group's chain as a reader holds it: its record,
// the txid of its transaction (hash byte order) and the height of its block.
type Link struct {
	Record []byte
	Txid   [32]byte
	Height uint32
}

// Epoch is one epoch a group's chain began.
type Epoch struct {
	Epoch uint64
	// ID is EpochID of the record of the token that began the epoch.
	ID [32]byte
	// Commitment is the BRC-369 digest of the epoch's key.
	Commitment [32]byte
	// Seq, Txid and Height are that token's.
	Seq    uint64
	Txid   [32]byte
	Height uint32
	// Removed reports that the token removed a member.
	Removed bool
}

// GroupChain is a group's chain folded from seq 0 (FoldGroup).
type GroupChain struct {
	// Tokens are the records in chain order, and Statuses the fold's status
	// of each (GroupFold.Apply).
	Tokens   []*Group
	Statuses []string
	// Epochs are the epochs the usable chain began, ascending.
	Epochs  []Epoch
	members [][]byte
}

// FoldGroup folds a group's chain from seq 0 with nothing but the records:
// links are the tokens in chain order, each the successor by spend of the
// one before. It asks no host and verifies no proof; the caller has checked
// each token against its own headers. It refuses a chain no host would have
// admitted (a record that does not decode, a first token that is not seq 0,
// a transition that breaks CheckGroupTransition, a token mined below its
// predecessor), and reports per token the status GroupFold gives. Epochs
// come only from the usable part.
func FoldGroup(links []Link) (*GroupChain, error) {
	c := &GroupChain{members: [][]byte{}}
	f := NewGroupFold()
	for i, l := range links {
		g, err := DecodeGroup(l.Record)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if g.Seq != 0 {
				return nil, fmt.Errorf("%w: the chain does not start at seq 0", ErrChain)
			}
		} else {
			if err := CheckGroupTransition(c.Tokens[i-1], links[i-1].Txid, g); err != nil {
				return nil, err
			}
			if l.Height < links[i-1].Height {
				return nil, fmt.Errorf("%w: mined below its predecessor", ErrChain)
			}
		}
		c.Tokens = append(c.Tokens, g)
		status := f.Apply(g)
		c.Statuses = append(c.Statuses, status)
		if status == StatusMembersMismatch {
			// Unusable from here on; the members stay those after the last
			// usable token.
			continue
		}
		c.members = f.Members()
		if g.BeginsEpoch() {
			c.Epochs = append(c.Epochs, Epoch{Epoch: g.Epoch, ID: EpochID(l.Record), Commitment: g.Commitment,
				Seq: g.Seq, Txid: l.Txid, Height: l.Height, Removed: len(g.Remove) > 0})
		}
	}
	return c, nil
}

// Usable is how many tokens from seq 0 fold without a mismatch: a reader
// fails closed from the first mismatched token on.
func (c *GroupChain) Usable() int {
	for i, s := range c.Statuses {
		if s == StatusMembersMismatch {
			return i
		}
	}
	return len(c.Tokens)
}

// Latest is the last usable token, or nil.
func (c *GroupChain) Latest() *Group {
	if n := c.Usable(); n > 0 {
		return c.Tokens[n-1]
	}
	return nil
}

// Members is the member set after the last usable token, ascending.
func (c *GroupChain) Members() [][]byte { return c.members }

// EpochByNumber is epoch e of the usable chain, if the chain began it.
func (c *GroupChain) EpochByNumber(e uint64) (Epoch, bool) {
	for _, x := range c.Epochs {
		if x.Epoch == e {
			return x, true
		}
	}
	return Epoch{}, false
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
