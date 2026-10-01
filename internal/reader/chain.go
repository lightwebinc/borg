package reader

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/lightwebinc/borg/internal/borgrec"
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
	// Forks are tokens that verified at a seq of the canonical chain but do
	// not continue it.
	Forks []*Tok
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
// id. Every host is asked the latest class; then every seq from 0 to the
// highest any host answered is asked of every host, so the reader folds the
// chain from seq 0 itself.
func (c *Client) Chain(ctx context.Context, hall string, org []byte, chain string) (*ChainView, error) {
	if c.Headers == nil {
		return nil, ErrNoHeaders
	}
	if len(c.Hosts) == 0 {
		return nil, errors.New("no overlay host configured (config key hosts, or -hosts)")
	}
	orgHex := hex.EncodeToString(org)
	v := &ChainView{Hall: hall, Org: org, Chain: chain, HostLatest: map[string]uint64{}, HostErr: map[string]error{}, Lacks: map[string][]uint64{}}
	var top uint64
	found := false
	for _, h := range c.Hosts {
		outs, err := c.Ask(ctx, h, c.latestQuery(hall, orgHex, chain))
		if err != nil {
			v.HostErr[h] = err
			continue
		}
		for _, o := range outs {
			t, err := VerifyToken(ctx, o.Beef, hall, org, c.Headers)
			if err == nil && !matches(t, chain) {
				err = errors.New("reader: the token is of another chain")
			}
			if err != nil {
				v.Refused = append(v.Refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
				continue
			}
			found = true
			if t.Seq() >= v.HostLatest[h] {
				v.HostLatest[h] = t.Seq()
			}
			if t.Seq() > top {
				top = t.Seq()
			}
		}
		if _, ok := v.HostLatest[h]; !ok && v.HostErr[h] == nil {
			v.HostLatest[h] = 0
		}
	}
	if !found {
		if len(v.HostErr) == len(c.Hosts) {
			return v, fmt.Errorf("%w: every host failed (%v)", ErrNoChain, firstErr(v.HostErr))
		}
		return v, ErrNoChain
	}
	// Every seq of every host: candidates per seq, and which hosts hold
	// which txid.
	cands := make([][]*Tok, top+1)
	holds := map[string]map[string]bool{}
	for _, h := range c.Hosts {
		if v.HostErr[h] != nil {
			continue
		}
		holds[h] = map[string]bool{}
		for s := uint64(0); s <= top; s++ {
			outs, err := c.Ask(ctx, h, c.seqQuery(hall, orgHex, chain, s))
			if err != nil {
				v.HostErr[h] = err
				break
			}
			for _, o := range outs {
				t, err := VerifyToken(ctx, o.Beef, hall, org, c.Headers)
				if err == nil && (!matches(t, chain) || t.Seq() != s) {
					err = errors.New("reader: the token is not of the chain and seq asked")
				}
				if err != nil {
					v.Refused = append(v.Refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
					continue
				}
				holds[h][t.Txid] = true
				dup := false
				for _, x := range cands[s] {
					if x.Txid == t.Txid {
						dup = true
					}
				}
				if !dup {
					cands[s] = append(cands[s], t)
				}
			}
		}
	}
	// The canonical chain: the first-mined seq 0, then at each seq the
	// first-mined token whose prev is the chain's last.
	for s := uint64(0); s <= top; s++ {
		var best *Tok
		for _, t := range cands[s] {
			if s > 0 && t.Prev() != v.Tokens[s-1].Txid {
				continue
			}
			if s == 0 && t.Token.Seq() != 0 {
				continue
			}
			if best == nil || t.Before(best) {
				best = t
			}
		}
		if best == nil {
			break
		}
		for _, t := range cands[s] {
			if t != best {
				v.Forks = append(v.Forks, t)
			}
		}
		v.Tokens = append(v.Tokens, best)
	}
	if len(v.Tokens) == 0 {
		return v, fmt.Errorf("%w: no seq 0 token verifies", ErrNoChain)
	}
	for h, set := range holds {
		if v.HostErr[h] != nil {
			continue
		}
		for _, t := range v.Tokens {
			if !set[t.Txid] {
				v.Lacks[h] = append(v.Lacks[h], t.Seq())
			}
		}
	}
	v.fold()
	return v, nil
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
