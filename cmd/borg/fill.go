package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/termsafe"

	"github.com/lightwebinc/borg/internal/borgrec"
	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/reader"
	"github.com/lightwebinc/borg/internal/unicast"
)

const fillHelp = `usage: borg fill [group...]

Fills every host in hosts that lacks something another host answered
(spec sections 11.6 and 12, "Filling a host that missed something"): the
organisation's chain, the grants of its roll, and for each group (every
group any host answers, or those named) its chain and the wraps of every
epoch a host still answers. Each token is replayed in the BEEF a host admits, assembled
here from the token and its parent (the token at seq - 1, or at seq 0 the
chain tree, read from the node: config key asset); each carrier is
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
	_, asset, err := g.node(false)
	if err != nil {
		return err
	}
	f := &filler{g: g, t: t, asset: asset}
	ov, err := t.rd.Org(ctx, t.hall, t.org)
	if err != nil {
		if errors.Is(err, reader.ErrNoChain) {
			return incomplete("the organisation: %v", err)
		}
		return refused("the organisation: %v", err)
	}
	f.chain(ctx, "organisation chain", ov.ChainView)
	f.carriers(ctx, "grant", borgrec.TxGrant, reader.Query{"hall": t.hall, "roll": hex.EncodeToString(t.org)},
		func(c *reader.Car) reader.Query {
			return reader.Query{"hall": t.hall, "org": hex.EncodeToString(t.org), "member": hex.EncodeToString(c.Carrier.Grant.Subject)}
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
	asset interface {
		TxRaw(ctx context.Context, txid string) ([]byte, error)
		MerkleProof(ctx context.Context, txid string) (*transaction.MerklePath, error)
	}
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
	var parent *transaction.Transaction
	if s > 0 {
		parent = v.Tokens[s-1].Tx
	} else {
		// The chain tree: the transaction the token's chain input spends,
		// which no host keeps; read from the node with its proof.
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
			mp, err := f.asset.MerkleProof(ctx, id)
			if err != nil {
				continue
			}
			tx.MerklePath = mp
			cand, err := f.assemble(ctx, v.Hall, tok.Tx, tx)
			if err == nil {
				return cand, nil
			}
		}
		return nil, errors.New("no input's transaction, read from the node with its proof, is its chain tree")
	}
	return f.assemble(ctx, v.Hall, tok.Tx, parent)
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
