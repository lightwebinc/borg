package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/bcommon/unicast"
	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/reader"
)

const fillHelp = `usage: borg fill [group...]

Fills every host in hosts that lacks something another host answered
(spec sections 11.6 and 12, "Filling a host that missed something"): the
organization's chain, the grants of its roll, the records of its policy
set, and for each group (every
group any host answers, or those named) its chain and the wraps of every
epoch a host still answers. Each token is replayed in the BEEF a host admits, assembled
here from the token and its parent (the token at seq - 1, or at seq 0 the
chain tree: from this home when it is the organization's home that keeps
the tree, else from the chain view, config key chain (a node drops a fully
spent transaction); the tree is checked either way); each carrier is
submitted as another host answered it, once it verifies here. Everything is
checked against this home's headers before it is sent, and each fill is
confirmed by a lookup at the host it was sent to. Needs no coin and signs
nothing: anyone who reads the hall can fill a host.

Exit 0 when every host holds everything any host answered (or now does),
3 when a host could not be asked or a fill was not confirmed.`

// cmdFill replays to each host what another host answered and it lacks.
func cmdFill(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("fill", fillHelp)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	t, err := g.readTarget(ctx)
	if err != nil {
		return err
	}
	chain, err := g.chainView()
	if err != nil {
		return err
	}
	f := &filler{g: g, t: t, asset: chain}
	if w, err := g.openWallet(); err == nil {
		if st, err := state.Load(g.cfg.Home, w.Signer().IdentityHex()); err == nil && st.Org != nil && st.Identity == hex.EncodeToString(t.org) {
			f.home = st
		}
	}
	ov, err := t.rd.Org(ctx, t.hall, t.org)
	if err != nil {
		return g.chainProblem("the organization", err)
	}
	f.chain(ctx, "organization chain", ov.ChainView)
	f.carriers(ctx, "grant", borgrec.TxGrant, reader.Query{"hall": t.hall, "roll": hex.EncodeToString(t.org)},
		func(c *reader.Car) reader.Query {
			return reader.Query{"hall": t.hall, "org": hex.EncodeToString(t.org), "member": hex.EncodeToString(c.Carrier.Grant.Subject)}
		})
	f.carriers(ctx, "policy record", borgrec.TxPolicy, reader.Query{"hall": t.hall, "policy": hex.EncodeToString(t.org)},
		func(c *reader.Car) reader.Query {
			return reader.Query{"hall": t.hall, "org": hex.EncodeToString(t.org), "role": c.Carrier.Policy.Role}
		})
	groups := pos
	if len(groups) == 0 {
		ids, errs := t.rd.GroupsAll(ctx, t.hall, t.org)
		for h, e := range errs {
			f.problem = true
			g.say("host %s: NOT ASKED for the groups: %s", h, firstLine(e.Error()))
		}
		groups = ids
	}
	for _, name := range groups {
		gid, err := g.resolveGroup(ctx, t, name)
		if err != nil {
			return err
		}
		gv, err := t.rd.Group(ctx, t.hall, t.org, gid)
		if err != nil {
			f.problem = true
			g.say("group %s: %v", gid, err)
			continue
		}
		f.chain(ctx, "group "+gid, gv.ChainView)
		orgHex := hex.EncodeToString(t.org)
		for n := uint32(0); ; n++ {
			if gv.Begins[n] != nil {
				e := fmt.Sprint(n)
				f.carriers(ctx, "wrap", borgrec.TxWrap, reader.Query{"hall": t.hall, "org": orgHex, "group": gid, "epoch": e},
					func(c *reader.Car) reader.Query {
						return reader.Query{"hall": t.hall, "org": orgHex, "group": gid, "epoch": e,
							"member": hex.EncodeToString(c.Carrier.Wrap.Recipients()[0])}
					})
			}
			if n == gv.Epoch() {
				break
			}
		}
	}
	fmt.Fprintf(g.stdout, "filled  %d object(s) at %d host(s)\n", f.filled, len(f.hostsFilled))
	if f.problem {
		return incomplete("a host could not be asked, or a fill was not confirmed")
	}
	return nil
}

type filler struct {
	g     *global
	t     *target
	asset nodeapi.Chain
	// home is the organization's home state when fill runs in it: the
	// chain trees it keeps are taken before the node is asked.
	home        *state.State
	filled      int
	hostsFilled map[string]bool
	problem     bool
}

// send submits beef to one host and confirms it by q answering txid.
func (f *filler) send(ctx context.Context, host, what, txid string, beef []byte, q reader.Query) bool {
	set := &unicast.Set{Hosts: []string{host}, Need: 1, Retries: engine.Retries, HTTP: httpClient, Note: f.g.say, Verbose: f.g.verbose}
	confirm := func(ctx context.Context, h string) (bool, error) { return f.t.rd.Held(ctx, h, q, txid) }
	if err := set.Send(ctx, what, borgrec.Topic(f.t.hall), beef, 1, confirm).Err(); err != nil {
		f.problem = true
		f.g.say("host %s: %s NOT FILLED: %v", host, what, err)
		return false
	}
	if ok, err := f.t.rd.Held(ctx, host, q, txid); err != nil || !ok {
		f.problem = true
		f.g.say("host %s: %s sent, and the host does not answer it", host, what)
		return false
	}
	if f.hostsFilled == nil {
		f.hostsFilled = map[string]bool{}
	}
	f.hostsFilled[host] = true
	f.filled++
	fmt.Fprintf(f.g.stdout, "filled  %s at %s\n", what, host)
	return true
}

// chain replays every canonical token a host lacks, in seq order, each in
// the BEEF a host admits.
func (f *filler) chain(ctx context.Context, what string, v *reader.ChainView) {
	for h, e := range v.HostErr {
		f.problem = true
		f.g.say("host %s: NOT ASKED about the %s: %s", h, what, firstLine(e.Error()))
	}
	hosts := make([]string, 0, len(v.Lacks))
	for h := range v.Lacks {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		for _, s := range v.Lacks[h] {
			tok := v.Tokens[s]
			beef, err := f.tokenBEEF(ctx, v, s)
			if err != nil {
				f.problem = true
				f.g.say("%s seq %d %s: cannot assemble its BEEF: %v", what, s, tok.Txid, err)
				continue
			}
			q := reader.Query{"hall": v.Hall, "org": hex.EncodeToString(v.Org), "seq": reader.Seq(s)}
			if v.Chain != reader.ChainOrg {
				q["group"] = v.Chain
			}
			f.send(ctx, h, fmt.Sprintf("%s seq %d %s", what, s, engine.Short(tok.Txid)), tok.Txid, beef, q)
		}
	}
}

// tokenBEEF assembles the BEEF of the canonical token at seq s: the token
// and its parent, each with its minimal proof, held to the host's rules.
func (f *filler) tokenBEEF(ctx context.Context, v *reader.ChainView, s uint64) ([]byte, error) {
	tok := v.Tokens[s]
	if s > 0 {
		return f.assemble(ctx, v.Hall, tok.Tx, v.Tokens[s-1].Tx)
	}
	// The chain tree: the transaction the token's chain input spends,
	// which no host keeps. The organization's home that published the
	// chain keeps it with its proof, and is asked first; the node is
	// asked after, since a node drops a fully spent transaction.
	var refused []string
	if f.home != nil {
		for _, in := range tok.Tx.Inputs {
			id := in.SourceTXID.String()
			rec, _, ok := f.home.TreeOf(id)
			if !ok {
				continue
			}
			tx, err := f.homeTree(ctx, id, rec)
			if err == nil {
				var cand []byte
				if cand, err = f.assemble(ctx, v.Hall, tok.Tx, tx); err == nil {
					return cand, nil
				}
			}
			refused = append(refused, fmt.Sprintf("this home's record of the chain tree %s is REFUSED: %v", id, err))
		}
	}
	for _, in := range tok.Tx.Inputs {
		id := in.SourceTXID.String()
		raw, err := f.asset.TxRaw(ctx, id)
		if err != nil {
			continue
		}
		tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
		if err != nil || tx.TxID().String() != id {
			continue
		}
		mp, _, err := f.asset.Proof(ctx, id)
		if err != nil {
			continue
		}
		tx.MerklePath = mp
		cand, err := f.assemble(ctx, v.Hall, tok.Tx, tx)
		if err == nil {
			return cand, nil
		}
	}
	tree := "its chain tree"
	if len(tok.Tx.Inputs) > 0 {
		tree = "its chain tree " + tok.Tx.Inputs[0].SourceTXID.String()
	}
	const node = "and the node does not answer it with a proof that makes the token's BEEF (a node drops a fully spent transaction, and no host keeps a chain tree)"
	switch {
	case len(refused) > 0:
		return nil, fmt.Errorf("%s; %s", strings.Join(refused, "; "), node)
	case f.home == nil:
		return nil, fmt.Errorf("%s: this home is not the organization's, %s; run borg fill from the organization's home that published the chain", tree, node)
	default:
		return nil, fmt.Errorf("%s: this home keeps no record of it, %s; run borg fill from the organization's home that published the chain", tree, node)
	}
}

// homeTree is the chain tree id as the home's record keeps it, held to
// what the node's answer would be held to: its bytes are the transaction
// the token spends, and its proof verifies against this home's headers. A
// kept proof that no longer verifies (a reorganization) is replaced by the
// node's current one when the node answers; the home is not written.
func (f *filler) homeTree(ctx context.Context, id string, rec *funding.Tree) (*transaction.Transaction, error) {
	tx, err := funding.Rebuild(rec.RawHex, rec.BumpHex, rec.BeefHex)
	if err != nil {
		return nil, fmt.Errorf("it does not parse: %w", err)
	}
	if got := tx.TxID().String(); got != id {
		return nil, fmt.Errorf("its bytes are transaction %s, not the token's parent", got)
	}
	if tx.MerklePath != nil {
		ok, err := tx.MerklePath.Verify(ctx, tx.TxID(), f.t.rd.Headers)
		if err != nil {
			return nil, fmt.Errorf("checking its proof against the header source: %w", err)
		}
		if ok {
			return tx, nil
		}
	}
	mp, _, err := f.asset.Proof(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("its kept proof does not verify against the header source (a reorganization, or no proof kept), and the node gives no current one: %w", err)
	}
	if ok, err := mp.Verify(ctx, tx.TxID(), f.t.rd.Headers); err != nil || !ok {
		return nil, errors.New("neither its kept proof nor the node's current one verifies against the header source")
	}
	f.g.say("chain tree %s: the proof this home kept does not verify against the header source; the node's current proof, at height %d, is used", engine.Short(id), mp.BlockHeight)
	tx.MerklePath = mp
	return tx, nil
}

func (f *filler) assemble(ctx context.Context, hall string, token, parent *transaction.Transaction) ([]byte, error) {
	tc, pc := token.ShallowClone(), parent.ShallowClone()
	var err error
	if tc.MerklePath, err = engine.Minimal(token.MerklePath, token.TxID()); err != nil {
		return nil, err
	}
	if pc.MerklePath, err = engine.Minimal(parent.MerklePath, parent.TxID()); err != nil {
		return nil, err
	}
	beef, err := borgrec.TokenBEEF(tc, pc)
	if err != nil {
		return nil, err
	}
	if _, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: hall, Headers: f.t.rd.Headers}); err != nil {
		return nil, fmt.Errorf("the assembled BEEF breaks the host's rules (%s): %w", borgrec.Reason(err), err)
	}
	return beef, nil
}

// carriers asks every host a paged carrier class, verifies each answer,
// and submits each carrier to every host that did not answer it, as the
// host that did answered it. confirm names the free question that answers
// one carrier at a host.
func (f *filler) carriers(ctx context.Context, what string, kind borgrec.TxKind, q reader.Query, confirm func(*reader.Car) reader.Query) {
	rd := f.t.rd
	answered := map[string]map[string]bool{}
	cars := map[string]*reader.Car{}
	for _, h := range rd.Hosts {
		ho := rd.Pages(ctx, h, q, true, func(o lookup.Output) string { return reader.SubjectID(o.Beef) })
		if ho.Err != nil || ho.Truncated {
			f.problem = true
			f.g.say("host %s: NOT ASKED for every %s: %v", h, what, ho.Err)
			continue
		}
		answered[h] = map[string]bool{}
		for _, o := range ho.Outputs {
			car, err := reader.VerifyCarrier(ctx, o.Beef, f.t.hall, kind, rd.Headers)
			if err != nil {
				f.g.say("host %s: REFUSED %s %s (%s): %v", h, what, reader.SubjectID(o.Beef), borgrec.Reason(err), err)
				continue
			}
			answered[h][car.Txid] = true
			if cars[car.Txid] == nil {
				cars[car.Txid] = car
			}
		}
	}
	ids := make([]string, 0, len(cars))
	for id := range cars {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, h := range rd.Hosts {
		set, ok := answered[h]
		if !ok {
			continue
		}
		for _, id := range ids {
			if set[id] {
				continue
			}
			c := cars[id]
			f.send(ctx, h, fmt.Sprintf("%s %s", what, termsafe.Abbrev(engine.Short(id))), id, c.Beef, confirm(c))
		}
	}
}
