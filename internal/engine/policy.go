package engine

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/state"
)

// PolicyReq is one policy record to publish: a role definition when To is
// empty, a binding otherwise.
type PolicyReq struct {
	Role        string
	Permissions []string
	// To is a binding's subject: a group identifier (16 bytes) or an
	// identity key (33). Empty makes the record a role definition.
	To []byte
	// Expiry is a binding's expiry in the one spelling, or empty.
	Expiry string
}

// PublishPolicy writes one policy record on a policy-tree output and
// queues its carrier. The organization's next token defines it (spec
// section 3.3): until then the record is published and in no set, which is
// what a host answers nothing for.
func (e *Engine) PublishPolicy(ctx context.Context, req PolicyReq) (*state.Policy, error) {
	o, err := e.org()
	if err != nil {
		return nil, err
	}
	kind := borgrec.PolicyRole
	if len(req.To) > 0 {
		kind = borgrec.PolicyBinding
	}
	perms := append([]string(nil), req.Permissions...)
	sort.Strings(perms)
	for i := 1; i < len(perms); i++ {
		if perms[i-1] == perms[i] {
			return nil, fmt.Errorf("permission %q given twice", perms[i])
		}
	}
	rec := &borgrec.Policy{Hall: o.Hall, Org: e.OrgKey(), Kind: kind, Role: req.Role,
		Permissions: perms, To: req.To, Expiry: req.Expiry, Created: e.created()}
	if err := rec.Validate(); err != nil {
		return nil, fmt.Errorf("the policy record breaks the host's rules (%s); it is not built: %w", borgrec.Reason(err), err)
	}
	if kind == borgrec.PolicyBinding && len(req.To) == 33 {
		if e.currentGrant(hex.EncodeToString(req.To)) == nil {
			e.note("warning: %s holds no current grant of this organization, so the binding authorises nothing until it does", hex.EncodeToString(req.To))
		}
	}
	b, err := rec.Encode()
	if err != nil {
		return nil, err
	}
	tree, vout, err := e.spend(ctx, state.TreePolicy)
	if err != nil {
		return nil, err
	}
	a, beef, err := e.carrierFor(ctx, o.Hall, state.TreePolicy, b, tree, vout)
	if err != nil {
		return nil, err
	}
	txid := Txid(a.Txid)
	sp := state.Policy{Txid: txid, Commitment: hex.EncodeToString(a.Carrier.Commitment[:]),
		Tree: tree.TxID().String(), Vout: vout, Kind: kind, Role: req.Role, Permissions: perms,
		To: hex.EncodeToString(req.To), Expiry: req.Expiry,
		Record: hex.EncodeToString(b), Beef: hex.EncodeToString(beef)}
	e.markSpent(state.TreePolicy, tree, vout)
	o.Policies = append(o.Policies, sp)
	p := state.Pending{Kind: state.KindPolicy, Hall: o.Hall, Txid: txid, Beef: hex.EncodeToString(beef), Org: e.orgHex()}
	if err := e.queue(ctx, p); err != nil {
		return o.PolicyByTxid(txid), err
	}
	return o.PolicyByTxid(txid), nil
}

// Records decodes this home's policy set, for the effective-permission fold.
func Records(set []*state.Policy) ([]*borgrec.Policy, error) {
	out := make([]*borgrec.Policy, 0, len(set))
	for _, p := range set {
		b, err := hex.DecodeString(p.Record)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Txid, err)
		}
		r, err := borgrec.DecodePolicy(b)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Txid, err)
		}
		out = append(out, r)
	}
	return out, nil
}
