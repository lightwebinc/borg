package reader

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
)

// ChainOrg is the chain member of a question about an organisation's own
// chain.
const ChainOrg = "org"

// ChainView is one chain as a reader established it from every host it
// asked (spec section 13): the canonical chain, which runs by spends from
// the first-mined seq 0 token, each token verified against the reader's
// headers, and how each host's answers compare with it.
type ChainView struct {
	Hall  string
	Org   []byte
	Chain string // ChainOrg or a group id
	// Tokens are the canonical chain, seq 0 to the latest.
	Tokens []*Tok
	// Statuses are the fold's status of each token (spec section 11.2).
	Statuses []string
	// HostLatest is the highest seq each host answered to the latest
	// class; HostErr the hosts that could not be asked.
	HostLatest map[string]uint64
	HostErr    map[string]error
	// Lacks are, per host that answered, the canonical seqs it did not
	// answer: a host behind, or withholding.
	Lacks map[string][]uint64
	// Refused are answered tokens that did not verify.
	Refused []Refusal
	// Forks are tokens that verified and do not continue the canonical
	// chain: at a seq of it, or at the seq the walk stopped at.
	Forks []*Tok
	// Claimed are the verified tokens the hosts answered as the chain's
	// latest.
	Claimed []*Tok
	// Replaced is set when a Floor remembered another token at a seq of
	// the canonical chain than the one read now: its txid. A reorganisation
	// displaced the remembered one, or the chain forked there.
	Replaced string
}

// Beyond are the verified tokens of the chain at a seq above the chain
// read, each once: a host answered them (as the latest, or at the seq the
// walk could not link), and no chain of predecessors reaches them from the
// first token. A token between was withheld, or the organisation signed a
// stray one; either way what was read may be an older state than the
// chain's latest, and a reader with no memory of its own has nothing else
// to tell it so.
func (v *ChainView) Beyond() []*Tok {
	var out []*Tok
	seen := map[string]bool{}
	for _, t := range append(append([]*Tok(nil), v.Claimed...), v.Forks...) {
		if t.Seq() >= uint64(len(v.Tokens)) && !seen[t.Txid] {
			seen[t.Txid] = true
			out = append(out, t)
		}
	}
	return out
}

// Latest is the canonical chain's last token, or nil.
func (v *ChainView) Latest() *Tok {
	if len(v.Tokens) == 0 {
		return nil
	}
	return v.Tokens[len(v.Tokens)-1]
}

// Usable is how many tokens from seq 0 fold without a mismatch: a reader
// fails closed from the first mismatched token on.
func (v *ChainView) Usable() int {
	for i, s := range v.Statuses {
		if s == borgrec.StatusRollMismatch || s == borgrec.StatusMembersMismatch {
			return i
		}
	}
	return len(v.Tokens)
}

// Agree reports whether every host that answered holds the whole canonical
// chain.
func (v *ChainView) Agree() bool {
	for _, l := range v.Lacks {
		if len(l) > 0 {
			return false
		}
	}
	return true
}

// ErrNoChain is a chain no host answered.
var ErrNoChain = errors.New("reader: no host answers this chain")

func (c *Client) latestQuery(hall, org, chain string) Query {
	if chain == ChainOrg {
		return Query{"hall": hall, "org": org}
	}
	return Query{"hall": hall, "org": org, "group": chain}
}

func (c *Client) seqQuery(hall, org, chain string, seq uint64) Query {
	q := c.latestQuery(hall, org, chain)
	q["seq"] = Seq(seq)
	return q
}

// matches reports whether a verified token belongs to the chain asked.
func matches(t *Tok, chain string) bool {
	if chain == ChainOrg {
		return t.Token.Org != nil
	}
	return t.Token.Group != nil && GroupHex(t.Token.Group.Group) == chain
}

// Chain establishes a chain of org's in hall: chain is ChainOrg or a group
// id. Every host is asked the latest class; then the chain is walked seq by
// seq from 0, every host asked each seq, so the reader folds the chain from
// seq 0 itself. A seq is asked for only once the one before it is on the
// chain, and the walk ends at the highest seq any host claimed as the
// latest or at the first seq nothing continues: the work is bounded by the
// chain itself, never by a seq a host, or a stray token of the
// organisation's, names. Verified tokens above where the walk ended are
// reported (Beyond).
//
// With a Floor, hosts that answer the chain only below the seq the caller
// verified before are refused with a *RollbackError, and what is verified
// now is recorded.
func (c *Client) Chain(ctx context.Context, hall string, org []byte, chain string) (*ChainView, error) {
	if c.Headers == nil {
		return nil, ErrNoHeaders
	}
	if len(c.Hosts) == 0 {
		return nil, errors.New("no overlay host configured (config key hosts, or -hosts)")
	}
	orgHex := hex.EncodeToString(org)
	v := &ChainView{Hall: hall, Org: org, Chain: chain, HostLatest: map[string]uint64{}, HostErr: map[string]error{}, Lacks: map[string][]uint64{}}
	verify := func(h string, o lookup.Output, seq *uint64) *Tok {
		t, err := c.Token(ctx, o.Beef, hall, org)
		if err == nil && !matches(t, chain) {
			err = errors.New("reader: the token is of another chain")
		}
		if err == nil && seq != nil && t.Seq() != *seq {
			err = errors.New("reader: the token is not of the seq asked")
		}
		if err != nil {
			v.Refused = append(v.Refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
			return nil
		}
		return t
	}
	// top is the highest seq any host claimed as the chain's latest. It
	// ends the walk; it never sizes anything.
	var top uint64
	found := false
	for _, h := range c.Hosts {
		outs, err := c.Ask(ctx, h, c.latestQuery(hall, orgHex, chain))
		if err != nil {
			v.HostErr[h] = err
			continue
		}
		v.HostLatest[h] = 0
		for _, o := range capOutputs(outs, limits.SeqAnswers) {
			t := verify(h, o, nil)
			if t == nil {
				continue
			}
			found = true
			v.Claimed = append(v.Claimed, t)
			if t.Seq() >= v.HostLatest[h] {
				v.HostLatest[h] = t.Seq()
			}
			if t.Seq() > top {
				top = t.Seq()
			}
		}
	}
	if !found {
		if len(v.HostErr) == len(c.Hosts) {
			return v, fmt.Errorf("%w: every host failed (%v)", ErrNoChain, firstErr(v.HostErr))
		}
		return v, ErrNoChain
	}
	live := map[string]bool{}
	for _, h := range c.Hosts {
		live[h] = v.HostErr[h] == nil
	}
	for s := uint64(0); s <= top; s++ {
		var cands []*Tok
		holds := map[string]map[string]bool{}
		for _, h := range c.Hosts {
			if !live[h] {
				continue
			}
			outs, err := c.Ask(ctx, h, c.seqQuery(hall, orgHex, chain, s))
			if err != nil {
				v.HostErr[h], live[h] = err, false
				continue
			}
			holds[h] = map[string]bool{}
			for _, o := range capOutputs(outs, limits.SeqAnswers) {
				t := verify(h, o, &s)
				if t == nil {
					continue
				}
				holds[h][t.Txid] = true
				dup := false
				for _, x := range cands {
					dup = dup || x.Txid == t.Txid
				}
				if !dup {
					cands = append(cands, t)
				}
			}
		}
		// The canonical chain: the first-mined seq 0, then at each seq the
		// first-mined token whose prev is the chain's last.
		var best *Tok
		for _, t := range cands {
			if s > 0 && t.Prev() != v.Tokens[s-1].Txid {
				continue
			}
			if best == nil || t.Before(best) {
				best = t
			}
		}
		for _, t := range cands {
			if t != best {
				v.Forks = append(v.Forks, t)
			}
		}
		if best == nil {
			break
		}
		v.Tokens = append(v.Tokens, best)
		for h, set := range holds {
			if !set[best.Txid] {
				v.Lacks[h] = append(v.Lacks[h], s)
			}
		}
	}
	if len(v.Tokens) == 0 {
		return v, fmt.Errorf("%w: no seq 0 token verifies", ErrNoChain)
	}
	for h := range v.HostErr {
		delete(v.Lacks, h)
	}
	v.fold()
	if c.Floor != nil {
		if err := v.hold(c.Floor); err != nil {
			return v, err
		}
	}
	return v, nil
}

// capOutputs is at most n of a host's answered outputs.
func capOutputs(outs []lookup.Output, n int) []lookup.Output {
	if len(outs) > n {
		return outs[:n]
	}
	return outs
}

func firstErr(m map[string]error) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", keys[0], m[keys[0]])
}

func (v *ChainView) fold() {
	v.Statuses = nil
	if v.Chain == ChainOrg {
		f := borgrec.NewOrgFold()
		for _, t := range v.Tokens {
			v.Statuses = append(v.Statuses, f.Apply(t.Token.Org))
		}
		return
	}
	f := borgrec.NewGroupFold()
	for _, t := range v.Tokens {
		v.Statuses = append(v.Statuses, f.Apply(t.Token.Group))
	}
}

// OrgView is an organisation's state from its canonical chain.
type OrgView struct {
	*ChainView
	// Roll is the roll after the last usable token, ascending.
	Roll [][32]byte
	// Token is the last usable token's record.
	Token *borgrec.Org
}

// Org establishes an organisation's chain and folds its roll.
func (c *Client) Org(ctx context.Context, hall string, org []byte) (*OrgView, error) {
	v, err := c.Chain(ctx, hall, org, ChainOrg)
	if err != nil {
		return nil, err
	}
	ov := &OrgView{ChainView: v}
	f := borgrec.NewOrgFold()
	for _, t := range v.Tokens[:v.Usable()] {
		f.Apply(t.Token.Org)
		ov.Token = t.Token.Org
	}
	ov.Roll = f.Roll()
	if ov.Token == nil {
		return ov, fmt.Errorf("reader: the organisation's chain breaks its fold at seq 0 (%s)", v.Statuses[0])
	}
	return ov, nil
}

// InRoll reports whether commitment c (hash byte order) is in the roll.
func (o *OrgView) InRoll(c [32]byte) bool {
	i := sort.Search(len(o.Roll), func(i int) bool { return bytes.Compare(o.Roll[i][:], c[:]) >= 0 })
	return i < len(o.Roll) && o.Roll[i] == c
}

// GroupView is a group's state from its canonical chain.
type GroupView struct {
	*ChainView
	// Members are the members after the last usable token, ascending.
	Members [][]byte
	// Removed are every key a usable token removed.
	Removed map[string]bool
	// Begins maps an epoch to the token that began it.
	Begins map[uint32]*Tok
	// Token is the last usable token's record.
	Token *borgrec.Group
	// Rejoined is set when a usable token re-added a removed key without a
	// new epoch: the group is inconsistent (spec section 4.3).
	Rejoined bool
}

// Group establishes a group's chain and folds its members.
func (c *Client) Group(ctx context.Context, hall string, org []byte, group string) (*GroupView, error) {
	v, err := c.Chain(ctx, hall, org, group)
	if err != nil {
		return nil, err
	}
	return NewGroupView(v)
}

// NewGroupView folds a group's chain view.
func NewGroupView(v *ChainView) (*GroupView, error) {
	gv := &GroupView{ChainView: v, Removed: map[string]bool{}, Begins: map[uint32]*Tok{}}
	f := borgrec.NewGroupFold()
	for i, t := range v.Tokens[:v.Usable()] {
		g := t.Token.Group
		f.Apply(g)
		for _, k := range g.Remove {
			gv.Removed[string(k)] = true
		}
		if g.BeginsEpoch() {
			gv.Begins[uint32(g.Epoch)] = t //nolint:gosec // an epoch is at most 2^32 - 1
		}
		if v.Statuses[i] == borgrec.StatusRejoinUnrotated {
			gv.Rejoined = true
		}
		gv.Token = g
	}
	gv.Members = f.Members()
	if gv.Token == nil {
		return gv, fmt.Errorf("reader: the group's chain breaks its fold at seq 0 (%s)", v.Statuses[0])
	}
	return gv, nil
}

// Epoch is the epoch in force at the last usable token.
func (g *GroupView) Epoch() uint32 { return uint32(g.Token.Epoch) } //nolint:gosec // at most 2^32 - 1

// IsMember reports whether k is a member at the last usable token.
func (g *GroupView) IsMember(k []byte) bool {
	for _, m := range g.Members {
		if bytes.Equal(m, k) {
			return true
		}
	}
	return false
}

// EpochsOf are the epochs k was a member of, ascending: an epoch counts
// when k was a member at any token of it.
func (g *GroupView) EpochsOf(k []byte) []uint32 {
	set := map[string]bool{}
	var out []uint32
	seen := map[uint32]bool{}
	for _, t := range g.Tokens[:g.Usable()] {
		gr := t.Token.Group
		for _, r := range gr.Remove {
			delete(set, string(r))
		}
		for _, a := range gr.Add {
			set[string(a)] = true
		}
		e := uint32(gr.Epoch) //nolint:gosec // at most 2^32 - 1
		if set[string(k)] && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}
