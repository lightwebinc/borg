package reader

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/borg/internal/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
)

// Spends is a source that indexes spends: a node's view of whether an
// output is spent, and by what. nodeapi.Asset is one. An answer of "" with
// no error is the positive answer "unspent"; an error is no answer at all.
type Spends interface {
	Spender(ctx context.Context, txid string, vout uint32) (string, error)
}

// GrantStatus is one grant as a reader judged it (spec sections 5.6 and
// 13).
type GrantStatus struct {
	Car   *Car
	Grant *borgrec.Grant
	// Values are the fields in the clear, from the reveal, when it carries
	// one.
	Values map[string]string
	// InRoll is set when the grant's commitment is in the roll of the
	// organisation's latest usable token.
	InRoll bool
	// Member is set when the scope grants member.
	Member bool
	// Expired is set when its expiry has passed at the reader's clock.
	Expired bool
	// Spent names the transaction that spends its revocation outpoint;
	// Unknown is set when no source could say whether it is spent.
	Spent   string
	Unknown bool
	// Current is the verdict; Why the first reason it is not.
	Current bool
	Why     string
}

// Judge applies the reader's checks to a verified grant carrier: in the
// roll, a scope that grants member, an expiry not passed (allowing the
// skew), a reveal unless the organisation is opaque, and a revocation
// outpoint positively unspent. spends may be nil: then "unspent" is never
// known, and the grant is never current for anything that releases a key.
func Judge(ctx context.Context, car *Car, ov *OrgView, spends Spends, now time.Time) GrantStatus {
	g := car.Carrier.Grant
	st := GrantStatus{Car: car, Grant: g, InRoll: ov.InRoll(car.Carrier.Commitment)}
	why := func(s string) {
		if st.Why == "" {
			st.Why = s
		}
	}
	if !st.InRoll {
		why("not in the roll of the organisation's latest token")
	}
	if g.Reveal != nil {
		vals, err := g.CheckReveal()
		if err != nil {
			why("its reveal does not open")
		} else {
			st.Values = vals
			st.Member = borgrec.IsMembership(vals["scope"])
			if !st.Member {
				why("its scope does not grant member")
			}
			if exp, err := borgrec.ParseExpiry(vals["expiry"]); err != nil || !now.Add(-limits.ExpirySkew).Before(exp) {
				st.Expired = true
				why("expired")
			}
		}
	} else if !ov.Token.Opaque {
		why("no reveal, and the organisation is not opaque: its scope cannot be checked")
	}
	if spends == nil {
		st.Unknown = true
		why("no spend source to say its revocation outpoint is unspent")
	} else {
		by, err := spends.Spender(ctx, car.Carrier.Funding.Txid.String(), car.Carrier.Funding.Index)
		switch {
		case err != nil:
			st.Unknown = true
			why("the spend source could not say whether its revocation outpoint is spent")
		case by != "":
			st.Spent = by
			why("revoked: its revocation outpoint is spent by " + by)
		}
	}
	if ov.Token.Opaque && g.Reveal == nil {
		// An opaque grant's fields are its members' to read; a reader of
		// the roll sees only that it exists.
		st.Member = true
	}
	st.Current = st.Why == ""
	return st
}

// GrantsOf asks every host the member class for k and returns the grants
// that verified, each with whether its inclusion path verifies against the
// roll of the token it names.
func (c *Client) GrantsOf(ctx context.Context, ov *OrgView, k []byte) ([]*Car, []Refusal, map[string]error) {
	q := Query{"hall": ov.Hall, "org": hex.EncodeToString(ov.Org), "member": hex.EncodeToString(k)}
	var out []*Car
	var refused []Refusal
	errs := map[string]error{}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		outs, err := c.Ask(ctx, h, q)
		if err != nil {
			errs[h] = err
			continue
		}
		for _, o := range outs {
			car, err := VerifyCarrier(ctx, o.Beef, ov.Hall, borgrec.TxGrant, c.Headers)
			if err == nil && (!bytes.Equal(car.Carrier.Grant.Org, ov.Org) || !bytes.Equal(car.Carrier.Grant.Subject, k)) {
				err = errors.New("reader: the grant is not the organisation's grant to this key")
			}
			if err != nil {
				refused = append(refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
				continue
			}
			car.Context = o.Context
			if !seen[car.Txid] {
				seen[car.Txid] = true
				out = append(out, car)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Txid < out[j].Txid })
	return out, refused, errs
}

// PathHolds reports whether a grant's inclusion context verifies against
// the roll of the canonical token at the seq it names.
func (o *OrgView) PathHolds(car *Car) bool {
	if len(car.Context) == 0 {
		return false
	}
	ic, err := borgrec.DecodeContext(car.Context)
	if err != nil || ic.Seq >= uint64(len(o.Tokens)) {
		return false
	}
	return ic.VerifyGrant(car.Carrier.Commitment, o.Tokens[ic.Seq].Token.Org.Roll)
}

// RollAnswer is the roll as one host answered it.
type RollAnswer struct {
	Host   string
	Grants []*Car
	// Root is the root of the commitments answered; Matches is set when it
	// is the latest usable token's roll.
	Root      [32]byte
	Matches   bool
	Refused   []Refusal
	Err       error
	Truncated bool
}

// Roll asks every host the roll class (all pages), verifies every grant and
// computes the root of what each host answered: a root other than the
// token's means the host withheld or added a grant (spec section 13).
func (c *Client) Roll(ctx context.Context, ov *OrgView) []RollAnswer {
	q := Query{"hall": ov.Hall, "roll": hex.EncodeToString(ov.Org)}
	var out []RollAnswer
	for _, h := range c.Hosts {
		ho := c.Pages(ctx, h, q, true, func(o lookup.Output) string { return SubjectID(o.Beef) })
		ra := RollAnswer{Host: h, Err: ho.Err, Truncated: ho.Truncated}
		var cs [][32]byte
		for _, o := range ho.Outputs {
			car, err := VerifyCarrier(ctx, o.Beef, ov.Hall, borgrec.TxGrant, c.Headers)
			if err == nil && !bytes.Equal(car.Carrier.Grant.Org, ov.Org) {
				err = errors.New("reader: another organisation's grant")
			}
			if err != nil {
				ra.Refused = append(ra.Refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
				continue
			}
			ra.Grants = append(ra.Grants, car)
			cs = append(cs, car.Carrier.Commitment)
		}
		ra.Root = borgrec.RollRoot(cs)
		ra.Matches = ra.Err == nil && ra.Root == ov.Token.Roll
		out = append(out, ra)
	}
	return out
}

// Opened is what a member opened, or could not, for one epoch.
type Opened struct {
	Epoch      uint32
	Key        [32]byte
	EpochID    [32]byte
	Commitment [32]byte
	// Label is "" when the key opened, else the check that failed (spec
	// section 6.4): token, entry, undecryptable or commitment.
	Label string
	Err   error
	// Host is the host whose wrap opened it.
	Host string
}

// Open is the member's checks of spec section 6.4 for one epoch of a
// group, in order: the token that began the epoch, from the reader's own
// canonical chain; a wrap record that verifies and holds an entry for me;
// the wallet's decryption of it; the commitment. Every host is asked until
// one answers a wrap that opens.
func (c *Client) Open(ctx context.Context, w wallet.Interface, me []byte, gv *GroupView, epoch uint32) Opened {
	op := Opened{Epoch: epoch}
	begin := gv.Begins[epoch]
	if begin == nil {
		op.Label, op.Err = "token", fmt.Errorf("no token of the canonical chain begins epoch %d", epoch)
		return op
	}
	op.EpochID = borgrec.EpochID(begin.Token.Record)
	op.Commitment = begin.Token.Group.Commitment
	org, err := ec.PublicKeyFromBytes(gv.Org)
	if err != nil {
		op.Label, op.Err = "token", err
		return op
	}
	q := Query{"hall": gv.Hall, "org": hex.EncodeToString(gv.Org), "group": gv.Chain, "epoch": fmt.Sprint(epoch), "member": hex.EncodeToString(me)}
	op.Label, op.Err = "entry", fmt.Errorf("no host answers a wrap of epoch %d to this key", epoch)
	for _, h := range c.Hosts {
		outs, err := c.Ask(ctx, h, q)
		if err != nil {
			continue
		}
		for _, o := range outs {
			car, err := VerifyCarrier(ctx, o.Beef, gv.Hall, borgrec.TxWrap, c.Headers)
			if err != nil {
				continue
			}
			wr := car.Carrier.Wrap
			if _, err := borgrec.CheckEpochToken(begin.Token.Record, wr); err != nil {
				op.Label, op.Err = "token", err
				continue
			}
			var entry []byte
			for i, r := range wr.Recipients() {
				if bytes.Equal(r, me) {
					entry = wr.Entries[i]
				}
			}
			if entry == nil {
				continue
			}
			k, err := borgrec.Unwrap(ctx, w, entry, org, op.EpochID, epoch, op.Commitment)
			if err != nil {
				op.Label, op.Err = borgrec.Reason(err), err
				continue
			}
			op.Key, op.Label, op.Err, op.Host = k, "", nil, h
			return op
		}
	}
	return op
}

// Wraps asks every host the wraps class for an epoch (all pages) and
// returns every wrap carrier that verified against the epoch's token, once
// each.
func (c *Client) Wraps(ctx context.Context, gv *GroupView, epoch uint32) ([]*Car, []Refusal) {
	begin := gv.Begins[epoch]
	var out []*Car
	var refused []Refusal
	if begin == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	q := Query{"hall": gv.Hall, "org": hex.EncodeToString(gv.Org), "group": gv.Chain, "epoch": fmt.Sprint(epoch)}
	for _, h := range c.Hosts {
		ho := c.Pages(ctx, h, q, true, func(o lookup.Output) string { return SubjectID(o.Beef) })
		for _, o := range ho.Outputs {
			car, err := VerifyCarrier(ctx, o.Beef, gv.Hall, borgrec.TxWrap, c.Headers)
			if err == nil {
				_, err = borgrec.CheckEpochToken(begin.Token.Record, car.Carrier.Wrap)
			}
			if err != nil {
				refused = append(refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
				continue
			}
			if !seen[car.Txid] {
				seen[car.Txid] = true
				out = append(out, car)
			}
		}
	}
	return out, refused
}

// Recipients asks every host the wraps class for an epoch (all pages) and
// returns every recipient of every wrap that verified against the epoch's
// token, for the member enumeration of spec section 13.
func (c *Client) Recipients(ctx context.Context, gv *GroupView, epoch uint32) (map[string]bool, []Refusal) {
	out := map[string]bool{}
	cars, refused := c.Wraps(ctx, gv, epoch)
	for _, car := range cars {
		for _, r := range car.Carrier.Wrap.Recipients() {
			out[string(r)] = true
		}
	}
	return out, refused
}

// GroupsOf asks every host the member-of class for k (all pages) and
// returns the group ids any host answered.
func (c *Client) GroupsOf(ctx context.Context, hall string, org, k []byte) ([]string, map[string]error) {
	q := Query{"hall": hall, "org": hex.EncodeToString(org), "memberOf": hex.EncodeToString(k)}
	return c.groupIDs(ctx, hall, org, q)
}

// GroupsAll asks every host the groups class for org (all pages).
func (c *Client) GroupsAll(ctx context.Context, hall string, org []byte) ([]string, map[string]error) {
	q := Query{"hall": hall, "groupsOf": hex.EncodeToString(org)}
	return c.groupIDs(ctx, hall, org, q)
}

func (c *Client) groupIDs(ctx context.Context, hall string, org []byte, q Query) ([]string, map[string]error) {
	set := map[string]bool{}
	errs := map[string]error{}
	cursor := func(o lookup.Output) string {
		_, t, err := borgrec.StoredToken(o.Beef)
		if err != nil || t.Group == nil {
			return ""
		}
		return GroupHex(t.Group.Group)
	}
	for _, h := range c.Hosts {
		ho := c.Pages(ctx, h, q, true, cursor)
		if ho.Err != nil {
			errs[h] = ho.Err
			continue
		}
		for _, o := range ho.Outputs {
			t, err := VerifyToken(ctx, o.Beef, hall, org, c.Headers)
			if err != nil || t.Token.Group == nil {
				continue
			}
			set[GroupHex(t.Token.Group.Group)] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, errs
}

// Fresh is a writer's evidence about a group's latest token (spec section
// 6.5): its proof verified against the writer's headers, and what the spend
// source said about its output.
type Fresh struct {
	Tok *Tok
	// Unspent is the positive answer; By the spender when it is spent.
	Unspent bool
	By      string
	// Next is set when the spender is the group's next token and keeps the
	// epoch: the writer goes on under the same epoch.
	Next *borgrec.Group
	At   time.Time
}

// OK reports whether a writer may key content under the token's epoch.
func (f Fresh) OK() bool { return f.Unspent || f.Next != nil }

// Latest asks spends whether the latest token's output is unspent. A
// spender is fetched with raw: when it is the group's next token and keeps
// the epoch, the writer goes on; any other spender (a new epoch, or a
// transaction that is not a token, which terminates the chain) is no
// evidence. An error from the source is no evidence: fail closed.
func Latest(ctx context.Context, spends Spends, raw func(ctx context.Context, txid string) ([]byte, error), gv *GroupView, now time.Time) (Fresh, error) {
	t := gv.Latest()
	f := Fresh{Tok: t, At: now}
	if spends == nil {
		return f, errors.New("no spend source (config key asset): a writer needs a positive answer that the latest token is unspent")
	}
	by, err := spends.Spender(ctx, t.Txid, t.Token.Vout)
	if err != nil {
		return f, fmt.Errorf("the spend source could not say whether %s.%d is spent: %w", t.Txid, t.Token.Vout, err)
	}
	if by == "" {
		f.Unspent = true
		return f, nil
	}
	f.By = by
	if raw == nil {
		return f, fmt.Errorf("the latest token %s is spent by %s", t.Txid, by)
	}
	b, err := raw(ctx, by)
	if err != nil {
		return f, fmt.Errorf("the latest token is spent by %s, which could not be read: %w", by, err)
	}
	tx, err := transaction.NewTransactionFromBytes(b)
	if err != nil || tx.TxID().String() != by {
		return f, fmt.Errorf("the latest token is spent by %s, and the source answered other bytes", by)
	}
	next, err := borgrec.ReadToken(tx)
	if err != nil || next.Group == nil {
		return f, fmt.Errorf("the latest token is spent by %s, which is not a token: the chain is terminated", by)
	}
	g := next.Group
	if !bytes.Equal(g.Org, gv.Org) || GroupHex(g.Group) != gv.Chain || g.Seq != t.Seq()+1 || chainhash.Hash(g.Prev).String() != t.Txid {
		return f, fmt.Errorf("the latest token is spent by %s, which is not this group's next token: the chain is terminated", by)
	}
	if g.Epoch != gv.Token.Epoch {
		return f, fmt.Errorf("the group's next token %s, not yet mined, begins epoch %d: wait until it mines and its key is wrapped", by, g.Epoch)
	}
	f.Next = g
	return f, nil
}

// Short is a txid's first 12 digits, for notes.
func Short(txid string) string {
	if len(txid) > 12 {
		return txid[:12]
	}
	return txid
}

// JoinSeqs words a list of seqs.
func JoinSeqs(s []uint64) string {
	parts := make([]string, len(s))
	for i, n := range s {
		parts[i] = Seq(n)
	}
	return strings.Join(parts, ", ")
}
