package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/producer"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/reader"
)

// Catching up (spec section 7.4). A home that runs an organization is not
// the only place its state lives: every chain token, every grant of the
// roll and every wrap is at the hosts. Sync rebuilds the home's record of
// the organization from them, as any reader reads them: each token verified
// against this home's headers and folded from seq 0, each grant by the
// host's own admission rules and the roll's root, each epoch key opened
// from the wrap to the organization's signer key and checked against the
// epoch's commitment. It is what a second instance of the tooling runs
// after another instance changed something, and what a home on a new
// machine runs once.
//
// It reads everything before it changes anything: a roll no host answers
// whole, or a chain that does not fold, leaves the home as it was.

// SyncOptions are Sync's choices.
type SyncOptions struct {
	// RetireTrees stops this home spending its current grant and record
	// trees whatever the hosts show: for a home that is a copy of another,
	// which holds the same trees.
	RetireTrees bool
}

// MissingKey is an epoch whose key Sync could not open.
type MissingKey struct {
	Group string
	Epoch uint32
	// Current is set when it is the group's epoch in force.
	Current bool
	Why     string
}

// SyncReport is what Sync did.
type SyncReport struct {
	// OrgTokens and GroupTokens are the tokens adopted, Displaced those of
	// this home's that are no longer in the chain (a reorganization).
	OrgTokens, GroupTokens, Displaced int
	// Groups are the groups this home did not hold; Grants the grants;
	// Policies the policy records.
	Groups, Grants, Policies int
	// Keys are the epoch keys opened from wraps to the signer key.
	Keys    int
	Missing []MissingKey
	// Coins are the outputs added to the pool; Spent those removed because
	// another instance spent them.
	Coins, Spent int
	// Retired are the funding trees this home stopped spending.
	Retired []string
	// Unasked are the hosts that could not be asked.
	Unasked []string
	// Refused are the answers a host gave that did not verify: tokens,
	// grants and wraps, each naming its host. None is used.
	Refused []reader.Refusal
}

// Changed reports whether Sync changed the home.
func (r *SyncReport) Changed() bool {
	return r.OrgTokens+r.GroupTokens+r.Displaced+r.Groups+r.Grants+r.Keys+r.Coins+r.Spent+len(r.Retired) > 0
}

// SyncError is a catch-up that could not be made from what the hosts
// answered. Incomplete is set when asking again, or other hosts, may help;
// otherwise the evidence contradicts the home.
type SyncError struct {
	Incomplete bool
	Msg        string
}

func (e *SyncError) Error() string { return e.Msg }

func syncIncomplete(format string, args ...any) error {
	return &SyncError{Incomplete: true, Msg: fmt.Sprintf(format, args...)}
}

func syncRefused(format string, args ...any) error {
	return &SyncError{Msg: fmt.Sprintf(format, args...)}
}

// groupPlan is one group as the hosts answered it.
type groupPlan struct {
	id     string
	view   *reader.GroupView
	opened map[uint32]reader.Opened
	// wrapped are the recipients of the current epoch's wraps.
	wrapped map[string]bool
}

// Stale reports whether another instance changed the organization since
// this home last wrote: the node shows the latest token of one of its
// chains spent, or a host answers a group this home does not hold. It asks
// the node once per chain and each host once.
func (e *Engine) Stale(ctx context.Context) bool {
	o := e.St.Org
	if o == nil || len(o.Tokens) == 0 {
		return false
	}
	spent := func(t *state.Token) bool {
		if t == nil {
			return false
		}
		by, err := e.Legs.Chain.Spender(ctx, t.Txid, t.Vout)
		return err == nil && by != ""
	}
	if spent(state.Latest(o.Tokens)) {
		return true
	}
	for i := range o.Groups {
		if spent(state.Latest(o.Groups[i].Tokens)) {
			return true
		}
	}
	if rd := e.Legs.Reader; rd != nil {
		ids, _ := rd.GroupsAll(ctx, o.Hall, e.OrgKey())
		for _, id := range ids {
			if o.Group(id) == nil {
				return true
			}
		}
	}
	return false
}

// Sync catches the home up with the organization as the hosts in hall
// answer it.
func (e *Engine) Sync(ctx context.Context, hall string, opt SyncOptions) (*SyncReport, error) {
	rd := e.Legs.Reader
	if rd == nil {
		return nil, errors.New("catching up reads the hosts: none is configured (config key hosts)")
	}
	if o := e.St.Org; o != nil && o.Hall != hall {
		return nil, fmt.Errorf("the organization lives in hall %s, not %s", o.Hall, hall)
	}
	rep := &SyncReport{}
	if h, err := e.Legs.Headers.CurrentHeight(ctx); err == nil {
		e.payer.Tip = h
	}
	// A transition of this home's own still in flight is settled first: it
	// is mined, or it lost.
	var lost *LostError
	if err := e.FinishTransition(ctx); errors.As(err, &lost) {
		e.note("%v", err)
	} else if err != nil {
		return nil, err
	}
	org := e.OrgKey()

	// Read.
	ov, err := rd.Org(ctx, hall, org)
	if errors.Is(err, reader.ErrNoChain) {
		if e.St.Org != nil && len(e.St.Org.Tokens) > 0 {
			return nil, syncIncomplete("no host answers the organization's chain, and this home holds %d token(s) of it: nothing to catch up from (%v)", len(e.St.Org.Tokens), err)
		}
		return nil, syncIncomplete("no host answers an organization of this home's key in hall %s: %v", hall, err)
	}
	if err != nil {
		return nil, syncRefused("the organization's chain: %v", err)
	}
	for h := range ov.HostErr {
		rep.Unasked = append(rep.Unasked, h)
	}
	sort.Strings(rep.Unasked)
	rep.Refused = append(rep.Refused, ov.Refused...)
	if u := ov.Usable(); u < len(ov.Tokens) {
		return nil, syncRefused("the organization's chain folds to %s at seq %d; nothing is taken from it", ov.Statuses[u], u)
	}
	grants := map[[32]byte]*reader.Car{}
	for _, ra := range rd.Roll(ctx, ov) {
		rep.Refused = append(rep.Refused, ra.Refused...)
		for _, car := range ra.Grants {
			if ov.InRoll(car.Carrier.Commitment) && grants[car.Carrier.Commitment] == nil {
				grants[car.Carrier.Commitment] = car
			}
		}
	}
	if len(grants) != len(ov.Roll) {
		// A grant missing here would read as a member with no current
		// grant, and the next transition would remove that member.
		return nil, syncIncomplete("the hosts answer %d of the %d grant(s) in the roll: the roll is not taken in part (borg fill replays what a host lacks)", len(grants), len(ov.Roll))
	}
	ids, _ := rd.GroupsAll(ctx, hall, org)
	if o := e.St.Org; o != nil {
		for i := range o.Groups {
			if !slices.Contains(ids, o.Groups[i].ID) {
				ids = append(ids, o.Groups[i].ID)
			}
		}
	}
	sort.Strings(ids)
	sk, err := e.SignerKey()
	if err != nil {
		return nil, err
	}
	var sw wallet.Interface
	var plans []*groupPlan
	var wraps []*reader.Car
	for _, id := range ids {
		gv, err := rd.Group(ctx, hall, org, id)
		if errors.Is(err, reader.ErrNoChain) {
			if e.St.Org != nil && e.St.Org.Group(id) != nil {
				e.note("group %s: no host answers its chain; this home's record of it is kept", id)
				continue
			}
			return nil, syncIncomplete("group %s: %v", id, err)
		}
		if err != nil {
			return nil, syncRefused("group %s: %v", id, err)
		}
		if u := gv.Usable(); u < len(gv.Tokens) {
			return nil, syncRefused("group %s folds to %s at seq %d; nothing is taken from it", id, gv.Statuses[u], u)
		}
		rep.Refused = append(rep.Refused, gv.Refused...)
		p := &groupPlan{id: id, view: gv, opened: map[uint32]reader.Opened{}, wrapped: map[string]bool{}}
		mine := map[uint32]bool{}
		for _, ep := range gv.EpochsOf(sk) {
			mine[ep] = true
		}
		var held *state.Group
		if e.St.Org != nil {
			held = e.St.Org.Group(id)
		}
		for ep, begin := range gv.Begins {
			cars, refused := rd.Wraps(ctx, gv, ep)
			rep.Refused = append(rep.Refused, refused...)
			wraps = append(wraps, cars...)
			if ep == gv.Epoch() {
				for _, car := range cars {
					for _, r := range car.Carrier.Wrap.Recipients() {
						p.wrapped[hex.EncodeToString(r)] = true
					}
				}
			}
			if held != nil {
				if he := held.EpochOf(ep); he != nil && he.Commitment == hex.EncodeToString(begin.Token.Group.Commitment[:]) {
					continue
				}
			}
			if !mine[ep] {
				p.opened[ep] = reader.Opened{Epoch: ep, Label: "entry", Err: errors.New("the signer key was not a member of this epoch")}
				continue
			}
			if sw == nil {
				if sw, err = e.signerWallet(); err != nil {
					return nil, err
				}
			}
			p.opened[ep] = rd.Open(ctx, sw, sk, gv, ep)
			rep.Refused = append(rep.Refused, p.opened[ep].Refused...)
		}
		plans = append(plans, p)
	}

	// Apply.
	o := e.St.Org
	if o == nil {
		o = &state.Org{Hall: hall}
		e.St.Org = o
	}
	o.Name, o.Opaque = ov.Token.Name, ov.Token.Opaque
	var adopted []*reader.Tok
	var n, d int
	if o.Tokens, n, d, err = e.merge(ctx, "the organization's chain", o.Tokens, ov.Tokens, &adopted); err != nil {
		return nil, err
	}
	rep.OrgTokens, rep.Displaced = n, d
	e.keepChainTree(ctx, o.Tokens)
	for _, p := range plans {
		g := o.Group(p.id)
		if g == nil {
			o.Groups = append(o.Groups, state.Group{ID: p.id})
			g = &o.Groups[len(o.Groups)-1]
			rep.Groups++
		}
		tok := p.view.Token
		g.Name, g.RotateOnJoin, g.RotateEvery = tok.Name, tok.RotateOnJoin, tok.RotateEvery
		if g.Tokens, n, d, err = e.merge(ctx, "group "+p.id, g.Tokens, p.view.Tokens, &adopted); err != nil {
			return nil, err
		}
		rep.GroupTokens += n
		rep.Displaced += d
		e.keepChainTree(ctx, g.Tokens)
		// An epoch this home held whose token is no longer in the chain
		// goes with its token.
		g.Epochs = slices.DeleteFunc(g.Epochs, func(ep state.Epoch) bool {
			return int(ep.Seq) >= len(g.Tokens) || !beginsWith(g.Tokens[ep.Seq], ep.Commitment) //nolint:gosec // a seq this home holds
		})
		for ep, op := range p.opened {
			if op.Label != "" {
				rep.Missing = append(rep.Missing, MissingKey{Group: p.id, Epoch: ep, Current: ep == p.view.Epoch(), Why: firstLine(op.Err)})
				continue
			}
			g.Epochs = append(g.Epochs, state.Epoch{Epoch: ep, Seq: p.view.Begins[ep].Seq(), Key: hex.EncodeToString(op.Key[:]),
				Commitment: hex.EncodeToString(op.Commitment[:]), EpochID: hex.EncodeToString(op.EpochID[:])})
			rep.Keys++
		}
		sort.Slice(g.Epochs, func(i, j int) bool { return g.Epochs[i].Epoch < g.Epochs[j].Epoch })
		if cur := g.EpochOf(p.view.Epoch()); cur != nil && len(g.Tokens) == len(p.view.Tokens) {
			for r := range p.wrapped {
				if !slices.Contains(cur.Wrapped, r) {
					cur.Wrapped = append(cur.Wrapped, r)
				}
			}
			sort.Strings(cur.Wrapped)
		}
	}
	sort.Slice(rep.Missing, func(i, j int) bool {
		a, b := rep.Missing[i], rep.Missing[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Epoch < b.Epoch
	})
	foreign, err := e.syncGrants(ctx, o, grants, rep)
	if err != nil {
		return nil, err
	}
	if err := e.syncPolicies(ctx, o, ov, rd, rep); err != nil {
		return nil, err
	}
	e.syncTrees(ctx, grants, foreign, wraps, opt, rep)
	e.syncPool(ctx, adopted, rep)
	if err := e.St.Save(); err != nil {
		return nil, err
	}
	return rep, nil
}

func firstLine(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// beginsWith reports whether a token's record commits to commitment (hex).
func beginsWith(t state.Token, commitment string) bool {
	b, err := hex.DecodeString(t.Record)
	if err != nil {
		return false
	}
	g, err := borgrec.DecodeGroup(b)
	return err == nil && hex.EncodeToString(g.Commitment[:]) == commitment
}

// merge extends the home's record of one chain with the canonical chain
// the hosts answered. The two must agree where both hold a token. A token
// of the home's that differs is kept only while its proof still holds
// against the headers, and then nothing is merged: two mined tokens at one
// seq is a fork the home does not resolve. One whose proof no longer holds
// was displaced by a reorganization and is dropped with everything after
// it.
func (e *Engine) merge(ctx context.Context, what string, local []state.Token, canon []*reader.Tok, adopted *[]*reader.Tok) ([]state.Token, int, int, error) {
	added, dropped := 0, 0
	for i, t := range canon {
		if i < len(local) {
			if local[i].Txid == t.Txid {
				continue
			}
			// Mined under the proof the home kept, or under the node's
			// current one when a reorganization left the kept one stale.
			if tx, err := rebuild(&local[i]); err == nil && e.minedNow(ctx, tx) {
				return nil, 0, 0, syncRefused("%s: this home holds %s at seq %d, mined, and the hosts answer %s: a fork; nothing is merged", what, Short(local[i].Txid), i, Short(t.Txid))
			}
			e.note("%s: this home's seq %d %s is no longer in the chain (a reorganization); it and what followed are dropped", what, i, Short(local[i].Txid))
			dropped += len(local) - i
			local = local[:i]
		}
		parent := ""
		switch {
		case i > 0:
			parent = canon[i-1].Txid
		case len(t.Tx.Inputs) > 0 && t.Tx.Inputs[0].SourceTXID != nil:
			parent = t.Tx.Inputs[0].SourceTXID.String()
		}
		local = append(local, state.Token{Seq: t.Seq(), Txid: t.Txid, RawHex: t.Tx.Hex(), BumpHex: t.Tx.MerklePath.Hex(), Height: t.Height,
			Record: hex.EncodeToString(t.Token.Record), Vout: t.Token.Vout, Parent: parent, Published: true})
		*adopted = append(*adopted, t)
		added++
	}
	return local, added, dropped, nil
}

// minedNow reports whether tx is mined in the publisher's headers, under
// the proof it carries or the node's current one.
func (e *Engine) minedNow(ctx context.Context, tx *transaction.Transaction) bool {
	_, err := e.current(ctx, tx)
	return err == nil
}

// syncGrants takes every grant of the roll the home does not hold, and
// marks each grant by the chain: admitted, revoked and at which seq, and
// swept when the node shows its revocation outpoint spent. It returns the
// txids of the grants taken: those another instance issued.
func (e *Engine) syncGrants(ctx context.Context, o *state.Org, grants map[[32]byte]*reader.Car, rep *SyncReport) (map[string]bool, error) {
	foreign := map[string]bool{}
	var ow wallet.Interface
	cars := make([]*reader.Car, 0, len(grants))
	for _, car := range grants {
		cars = append(cars, car)
	}
	sort.Slice(cars, func(i, j int) bool { return cars[i].Txid < cars[j].Txid })
	for _, car := range cars {
		ch := hex.EncodeToString(car.Carrier.Commitment[:])
		if o.GrantByCommitment(ch) != nil {
			continue
		}
		g := car.Carrier.Grant
		var vals map[string]string
		var err error
		if g.Reveal != nil {
			vals, err = g.CheckReveal()
		} else {
			if ow == nil {
				if e.Key == nil {
					return nil, errors.New("the organization's identity key is not open: a grant without a reveal cannot be read back")
				}
				if ow, err = wallet.NewCompletedProtoWallet(e.Key); err != nil {
					return nil, err
				}
			}
			vals, err = g.OpenFieldsAsOrg(ctx, ow)
		}
		if err != nil {
			return nil, syncRefused("grant %s: its fields do not open: %v", Short(car.Txid), err)
		}
		o.Grants = append(o.Grants, state.Grant{Subject: hex.EncodeToString(g.Subject), Txid: car.Txid, Commitment: ch,
			Tree: car.Carrier.Funding.Txid.String(), Vout: car.Carrier.Funding.Index, Role: vals["role"], Scope: vals["scope"],
			Expiry: vals["expiry"], Record: hex.EncodeToString(car.Carrier.Record), Beef: hex.EncodeToString(car.Beef)})
		foreign[car.Txid] = true
		rep.Grants++
	}
	recs, err := orgRecords(o)
	if err != nil {
		return nil, err
	}
	for i := range o.Grants {
		o.Grants[i].Admitted, o.Grants[i].Revoked, o.Grants[i].RevSeq = false, false, 0
	}
	for _, r := range recs {
		for _, c := range r.Admit {
			if g := o.GrantByCommitment(hex.EncodeToString(c[:])); g != nil {
				g.Admitted = true
			}
		}
		for _, c := range r.Revoke {
			if g := o.GrantByCommitment(hex.EncodeToString(c[:])); g != nil {
				g.Revoked, g.RevSeq = true, r.Seq
			}
		}
	}
	for i := range o.Grants {
		g := &o.Grants[i]
		if !g.Admitted || g.Revoked || g.Swept != "" {
			continue
		}
		if by, err := e.Legs.Chain.Spender(ctx, g.Tree, g.Vout); err == nil && by != "" {
			g.Swept = by
		}
	}
	return foreign, nil
}

// syncTrees stops the home spending a grant or record tree another
// instance also spends: one whose output a grant this home did not issue
// spends, or a carrier spends at or past the home's next output. Two
// carriers on one funding output share one retraction, and two grants one
// revocation. The chain tree is advanced past every output the node shows
// spent.
func (e *Engine) syncTrees(ctx context.Context, grants map[[32]byte]*reader.Car, foreign map[string]bool, wraps []*reader.Car, opt SyncOptions, rep *SyncReport) {
	retire := map[string]bool{}
	see := func(car *reader.Car, foreign bool) {
		t, kind, ok := e.St.TreeOf(car.Carrier.Funding.Txid.String())
		if !ok {
			e.keepForeignTree(car)
			return
		}
		if kind == state.TreeChain || t.Remaining() == 0 {
			return
		}
		if foreign || car.Carrier.Funding.Index >= t.Next {
			retire[t.Txid] = true
		}
	}
	for _, car := range wraps {
		see(car, false)
	}
	for _, car := range grants {
		see(car, foreign[car.Txid])
	}
	for _, kind := range []string{state.TreeGrant, state.TreePolicy, state.TreeRecord} {
		ts := e.St.Trees[kind]
		if ts == nil {
			continue
		}
		done := func(t *funding.Tree) {
			if t.Remaining() == 0 || !(opt.RetireTrees || retire[t.Txid]) {
				return
			}
			t.Next = t.Count
			if !slices.Contains(rep.Retired, t.Txid) {
				rep.Retired = append(rep.Retired, t.Txid)
			}
		}
		if ts.Tree != nil {
			done(ts.Tree)
		}
		for i := range ts.Trees {
			done(&ts.Trees[i])
		}
		for i := range ts.Ahead {
			done(&ts.Ahead[i])
		}
	}
	if ts := e.St.Trees[state.TreeChain]; ts != nil && ts.Tree != nil {
		t := ts.Tree
		for t.Next < t.Count {
			by, err := e.Legs.Chain.Spender(ctx, t.Txid, t.Next)
			if err != nil || by == "" {
				break
			}
			t.Next++
		}
		ts.Trees = producer.Index(ts.Trees, t)
	}
}

// keepChainTree records the chain tree a chain's seq 0 token spends, read
// from the node with its proof, when this home keeps no record of it: the
// organization's other home minted it. No host keeps a chain tree and a
// node drops a fully spent transaction, so fill replays the first token
// from this record (spec section 11.6). Nothing is spent from it. A node
// that does not answer leaves the home as it was.
func (e *Engine) keepChainTree(ctx context.Context, tokens []state.Token) {
	if len(tokens) == 0 || tokens[0].Parent == "" {
		return
	}
	id := tokens[0].Parent
	if _, _, ok := e.St.TreeOf(id); ok {
		return
	}
	tok, err := rebuild(&tokens[0])
	if err != nil || len(tok.Inputs) == 0 || tok.Inputs[0].SourceTXID == nil || tok.Inputs[0].SourceTXID.String() != id {
		return
	}
	raw, err := e.Legs.Chain.TxRaw(ctx, id)
	if err != nil {
		return
	}
	tree, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil || tree.TxID().String() != id {
		return
	}
	mp, _, err := e.Legs.Chain.Proof(ctx, id)
	if err != nil {
		return
	}
	if ok, err := mp.Verify(ctx, tree.TxID(), e.Legs.Headers); err != nil || !ok {
		return
	}
	idx := int(tok.Inputs[0].SourceTxOutIndex)
	if idx >= len(tree.Outputs) || tree.Outputs[idx].LockingScript == nil {
		return
	}
	lock := tree.Outputs[idx].LockingScript
	count := uint32(0)
	for _, o := range tree.Outputs {
		if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lock) {
			count++
		}
	}
	ts := e.St.Set(state.TreeChain)
	ts.Trees = append(ts.Trees, funding.Tree{IdentityKeyHex: e.Signer.IdentityHex(), Txid: id, RawHex: tree.Hex(),
		BumpHex: mp.Hex(), Height: mp.BlockHeight, Sats: tree.Outputs[idx].Satoshis, Count: count, Next: count,
		Funder: "another instance"})
}

// keepForeignTree records the funding tree of a carrier another instance
// funded, as its BEEF carries it, with no output left to spend from here:
// the tree's outputs are locked to keys this identity derives, so this home
// can sweep it (a revocation, the kill switch) although it never minted it.
func (e *Engine) keepForeignTree(car *reader.Car) {
	kind := state.TreeRecord
	if car.Kind == borgrec.TxGrant {
		kind = state.TreeGrant
	}
	var tree *transaction.Transaction
	for _, in := range car.Tx.Inputs {
		if in.SourceTransaction != nil && in.SourceTXID != nil && *in.SourceTXID == car.Carrier.Funding.Txid {
			tree = in.SourceTransaction
		}
	}
	idx := int(car.Carrier.Funding.Index)
	if tree == nil || tree.MerklePath == nil || idx >= len(tree.Outputs) {
		return
	}
	lock := tree.Outputs[idx].LockingScript
	count := uint32(0)
	for _, o := range tree.Outputs {
		if o.LockingScript != nil && bytes.Equal(*o.LockingScript, *lock) {
			count++
		}
	}
	ts := e.St.Set(kind)
	ts.Trees = append(ts.Trees, funding.Tree{IdentityKeyHex: e.Signer.IdentityHex(), Txid: tree.TxID().String(), RawHex: tree.Hex(),
		BumpHex: tree.MerklePath.Hex(), Height: tree.MerklePath.BlockHeight, Sats: tree.Outputs[idx].Satoshis, Count: count, Next: count,
		Funder: "another instance"})
}

// syncPool makes the pool the home's unspent coins again: a coin the node
// shows spent (another instance held it too, and spent it) is removed, and
// the change of each adopted token that pays this home's fund key, unspent,
// is added with its proof.
func (e *Engine) syncPool(ctx context.Context, adopted []*reader.Tok, rep *SyncReport) {
	for _, o := range e.Pool.Outputs() {
		if o.Unproven {
			continue
		}
		if by, err := e.Legs.Chain.Spender(ctx, o.TxID, o.Vout); err != nil || by == "" {
			continue
		}
		if removed, err := e.Pool.Remove(o); err == nil && removed {
			rep.Spent++
		}
	}
	fund, err := e.Signer.FundScript()
	if err != nil {
		return
	}
	for _, t := range adopted {
		for i, out := range t.Tx.Outputs {
			if out.LockingScript == nil || !bytes.Equal(*out.LockingScript, *fund) {
				continue
			}
			by, err := e.Legs.Chain.Spender(ctx, t.Txid, uint32(i)) //nolint:gosec // an output index
			if err != nil || by != "" {
				continue
			}
			n, err := e.Pool.Add(bwallet.Output{TxID: t.Txid, Vout: uint32(i), Satoshis: out.Satoshis, //nolint:gosec // an output index
				LockingScript: out.LockingScript.String(), Height: t.Height, Raw: t.Tx.Hex(), Bump: t.Tx.MerklePath.Hex()})
			if err == nil {
				rep.Coins += n
			}
		}
	}
}

// syncPolicies takes every policy record the set holds that this home does
// not. The set is not taken in part: a record missing here would read as a
// role nobody holds, and the home's next transition would be built over a
// set that is not the chain's.
func (e *Engine) syncPolicies(ctx context.Context, o *state.Org, ov *reader.OrgView, rd *reader.Client, rep *SyncReport) error {
	set := map[[32]byte]*reader.Car{}
	for _, pa := range rd.PolicySet(ctx, ov) {
		rep.Refused = append(rep.Refused, pa.Refused...)
		for _, car := range pa.Policies {
			if ov.InPolicySet(car.Carrier.Commitment) && set[car.Carrier.Commitment] == nil {
				set[car.Carrier.Commitment] = car
			}
		}
	}
	if len(set) != len(ov.PolicySet) {
		return syncIncomplete("the hosts answer %d of the %d policy record(s) in the set: the set is not taken in part (borg fill replays what a host lacks)", len(set), len(ov.PolicySet))
	}
	cars := make([]*reader.Car, 0, len(set))
	for _, car := range set {
		cars = append(cars, car)
	}
	sort.Slice(cars, func(i, j int) bool { return cars[i].Txid < cars[j].Txid })
	for _, car := range cars {
		ch := hex.EncodeToString(car.Carrier.Commitment[:])
		if o.PolicyByCommitment(ch) != nil {
			continue
		}
		p := car.Carrier.Policy
		o.Policies = append(o.Policies, state.Policy{Txid: car.Txid, Commitment: ch,
			Tree: car.Carrier.Funding.Txid.String(), Vout: car.Carrier.Funding.Index,
			Kind: p.Kind, Role: p.Role, Permissions: p.Permissions, To: hex.EncodeToString(p.To),
			Expiry: p.Expiry, Record: hex.EncodeToString(car.Carrier.Record),
			Beef: hex.EncodeToString(car.Beef), Defined: true})
		rep.Policies++
	}
	// A record this home holds that the set no longer does was retired by a
	// token another instance built.
	for i := range o.Policies {
		p := &o.Policies[i]
		if p.Defined && !p.Retired && set[hashOfHex(p.Commitment)] == nil {
			p.Retired = true
		}
	}
	return nil
}
