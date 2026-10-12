package reader

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/borg/borgrec"
)

// PolicyAnswer is the policy set as one host answered it.
type PolicyAnswer struct {
	Host     string
	Policies []*Car
	// Root is the root of the commitments answered; Matches is set when it
	// is the latest usable token's policy root. A root other than the
	// token's means the host withheld or added a record (spec section 13).
	Root      [32]byte
	Matches   bool
	Refused   []Refusal
	Err       error
	Truncated bool
}

// PolicySet asks every host the policy class (all pages), verifies every
// record and computes the root of what each host answered.
func (c *Client) PolicySet(ctx context.Context, ov *OrgView) []PolicyAnswer {
	q := Query{"hall": ov.Hall, "policy": hex.EncodeToString(ov.Org)}
	var out []PolicyAnswer
	vs := verdicts{}
	for _, h := range c.Hosts {
		ho := c.Pages(ctx, h, q, true, func(o lookup.Output) string { return SubjectID(o.Beef) })
		pa := PolicyAnswer{Host: h, Err: ho.Err, Truncated: ho.Truncated}
		var cs [][32]byte
		for _, o := range ho.Outputs {
			car, err := c.policyCar(ctx, vs, h, ov, o.Beef)
			if err != nil {
				pa.Refused = append(pa.Refused, Refusal{Host: h, Txid: SubjectID(o.Beef), Reason: borgrec.Reason(err), Err: err})
				continue
			}
			pa.Policies = append(pa.Policies, car)
			cs = append(cs, car.Carrier.Commitment)
		}
		pa.Root = borgrec.PolicyRoot(cs)
		pa.Matches = pa.Err == nil && pa.Root == ov.Token.Policy
		out = append(out, pa)
	}
	return out
}

// policyCar verifies one answered policy carrier and holds it to this
// organisation's.
func (c *Client) policyCar(ctx context.Context, vs verdicts, host string, ov *OrgView, beef []byte) (*Car, error) {
	car, _, _, err := vs.carrier(ctx, c, host, beef, ov.Hall, borgrec.TxPolicy)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(car.Carrier.Policy.Org, ov.Org) {
		return nil, errors.New("reader: another organisation's policy record")
	}
	return car, nil
}

// RoleOf asks every host for the records that name one role: its
// definition or definitions, and every binding that gives it.
func (c *Client) RoleOf(ctx context.Context, ov *OrgView, role string) ([]*Car, []Refusal, map[string]error) {
	return c.policyQuery(ctx, ov, Query{"hall": ov.Hall, "org": hex.EncodeToString(ov.Org), "role": role})
}

// BoundTo asks every host for the bindings whose subject is one group
// identifier or one identity key.
func (c *Client) BoundTo(ctx context.Context, ov *OrgView, to []byte) ([]*Car, []Refusal, map[string]error) {
	return c.policyQuery(ctx, ov, Query{"hall": ov.Hall, "org": hex.EncodeToString(ov.Org), "boundTo": hex.EncodeToString(to)})
}

func (c *Client) policyQuery(ctx context.Context, ov *OrgView, q Query) ([]*Car, []Refusal, map[string]error) {
	var out []*Car
	var refused []Refusal
	errs := map[string]error{}
	seen := map[string]bool{}
	vs := verdicts{}
	for _, h := range c.Hosts {
		ho := c.Pages(ctx, h, q, true, func(o lookup.Output) string { return SubjectID(o.Beef) })
		if ho.Err != nil {
			errs[h] = ho.Err
			continue
		}
		if ho.Truncated {
			refused = append(refused, Refusal{Host: h, Reason: "truncated", Err: errors.New("reader: the host's answer was cut short")})
		}
		for _, o := range ho.Outputs {
			car, err := c.policyCar(ctx, vs, h, ov, o.Beef)
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

// PolicyPathHolds reports whether a policy record's inclusion context
// verifies against the policy root of the canonical token at the seq it
// names. A context that does not verify is a hint and nothing more: the
// caller falls back to the set (spec section 10.2).
func (o *OrgView) PolicyPathHolds(car *Car) bool {
	if len(car.Context) == 0 {
		return false
	}
	ic, err := borgrec.DecodeContext(car.Context)
	if err != nil || ic.Seq >= uint64(len(o.Tokens)) {
		return false
	}
	return ic.VerifyPolicy(car.Carrier.Commitment, o.Tokens[ic.Seq].Token.Org.Policy)
}

// Decision is what a service concluded about one subject, with everything
// it rested on, so that it can say why and write a receipt.
type Decision struct {
	// Allow is the answer. It is false whenever the read was incomplete.
	Allow bool
	// Reason is a short label when Allow is false: not-member, no-match,
	// incomplete, or stale.
	Reason string
	// Effective is the permission set the subject holds.
	Effective []string
	// Seq and Token name the organisation token the decision rested on.
	Seq   uint64
	Token string
	// Grants and Policies are the commitments the decision used, ascending.
	Grants   [][32]byte
	Policies [][32]byte
	// Groups are the groups of the organisation that hold the subject.
	Groups []string
	// Refused are answers that did not verify, HostErr the hosts that could
	// not be asked. Either leaves the read incomplete, and a service that
	// cares about completeness denies on them.
	Refused []Refusal
	HostErr map[string]error
}

// Effective is the procedure of spec section 13 for one subject: its
// current grants, the groups that hold it, and the policy records that
// reach it, each verified against the caller's own headers, folded into the
// permissions it holds.
//
// It fails closed. A subject with no current grant holds nothing; a read
// that could not be completed holds nothing and says so in Reason. Freshness
// is the caller's: spends decides whether a grant's revocation outpoint is
// unspent, and nothing here treats an absence as unspent.
func (c *Client) Effective(ctx context.Context, ov *OrgView, subject []byte, spends Spends, now time.Time) Decision {
	d := Decision{Reason: "not-member", HostErr: map[string]error{}}
	if ov.Token != nil {
		d.Seq = ov.Token.Seq
	}
	if t := ov.Latest(); t != nil {
		d.Token = t.Txid
	}
	grants, refused, errs := c.GrantsOf(ctx, ov, subject)
	d.Refused = append(d.Refused, refused...)
	for h, err := range errs {
		d.HostErr[h] = err
	}
	sub := borgrec.Subject{Key: subject}
	for _, g := range grants {
		st := Judge(ctx, g, ov, spends, now)
		if !st.Current {
			continue
		}
		sub.Member = true
		d.Grants = append(d.Grants, g.Carrier.Commitment)
		// The role a grant names is readable only where the grant carries a
		// reveal. In an opaque organisation the subject discloses it, which
		// is not something a reader asks a host for, so a grant without one
		// names no role here and only bindings reach the subject.
		if role, ok := st.Values["role"]; ok && borgrec.CheckRoleName(role) == nil {
			sub.Roles = append(sub.Roles, role)
		}
	}
	if !sub.Member {
		return d
	}
	// The hosts' member-of answer names candidate groups; it is not
	// evidence. Each one's chain is established and folded here, and the
	// subject counts as a member only where this reader's own fold of
	// verified tokens says so. A group that cannot be established, or whose
	// chain breaks its fold, contributes nothing: leaving a group out can
	// only take permissions away, so this fails closed.
	candidates, gerrs := c.GroupsOf(ctx, ov.Hall, ov.Org, subject)
	for h, err := range gerrs {
		d.HostErr[h] = err
	}
	for _, g := range candidates {
		b, err := hex.DecodeString(g)
		if err != nil || len(b) != borgrec.GroupIDLen {
			continue
		}
		gv, err := c.Group(ctx, ov.Hall, ov.Org, g)
		if err != nil {
			d.Refused = append(d.Refused, Refusal{Txid: g, Reason: "group", Err: err})
			continue
		}
		if u := gv.Usable(); u < len(gv.Tokens) {
			d.Refused = append(d.Refused, Refusal{Txid: g, Reason: "group",
				Err: fmt.Errorf("the group's chain folds to %s at seq %d, so its membership is not taken", gv.Statuses[u], u)})
			continue
		}
		holds := false
		for _, m := range gv.Members {
			if bytes.Equal(m, subject) {
				holds = true
				break
			}
		}
		if !holds {
			d.Refused = append(d.Refused, Refusal{Txid: g, Reason: "group",
				Err: errors.New("a host named this group, and its own chain does not hold the subject")})
			continue
		}
		d.Groups = append(d.Groups, g)
		sub.Groups = append(sub.Groups, [borgrec.GroupIDLen]byte(b))
	}
	// The set the latest token commits to, taken from the hosts and held to
	// its root: a host that withheld or added a record is not used.
	var set []*borgrec.Policy
	matched := false
	for _, pa := range c.PolicySet(ctx, ov) {
		d.Refused = append(d.Refused, pa.Refused...)
		if pa.Err != nil {
			d.HostErr[pa.Host] = pa.Err
			continue
		}
		if !pa.Matches || matched {
			continue
		}
		matched = true
		for _, car := range pa.Policies {
			set = append(set, car.Carrier.Policy)
			d.Policies = append(d.Policies, car.Carrier.Commitment)
		}
	}
	if !matched {
		d.Reason = "incomplete"
		return d
	}
	sortCommitments(d.Grants)
	sortCommitments(d.Policies)
	d.Effective = borgrec.Effective(sub, set, now)
	d.Allow, d.Reason = false, "no-match"
	return d
}

// Allows is Effective's verdict for one asked permission. It denies
// whatever is wrong: a subject that is not a current member, a read that
// could not be completed, or a permission no record grants.
func (c *Client) Allows(ctx context.Context, ov *OrgView, subject []byte, permission string, spends Spends, now time.Time) (Decision, error) {
	d := c.Effective(ctx, ov, subject, spends, now)
	if d.Reason == "incomplete" || d.Reason == "not-member" {
		return d, nil
	}
	ok, err := borgrec.Allows(d.Effective, permission)
	if err != nil {
		return d, err
	}
	if ok {
		d.Allow, d.Reason = true, ""
	}
	return d, nil
}

func sortCommitments(a [][32]byte) {
	sort.Slice(a, func(i, j int) bool { return bytes.Compare(a[i][:], a[j][:]) < 0 })
}
