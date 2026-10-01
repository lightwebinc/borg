package main

import (
	"context"
	"fmt"

	"github.com/lightwebinc/bcommon/funding"

	"github.com/lightwebinc/borg/internal/state"
)

const killHelp = `usage: borg kill -confirm KILL [-yes]

The kill switch (spec sections 9.4 and 9.5). It sweeps every grant tree and
every record tree this home holds: one mined transaction per tree, spending
every funding output it has, used or not. Once a sweep mines, every carrier
those outputs funded is a double spend: every grant on the tree is revoked
for every BRC-52 verifier, and every honest host retracts the grants and
the wraps. It is IRREVERSIBLE, and it is the revocation of the whole roll.

Without -yes it prints what it would sweep and sends nothing. With -yes
each sweep is persisted, settled, waited for until it mines (one block a
tree) and published to the plane and to every host named. A run that stops
part way is finished by running kill again: a tree already swept is passed
over.

What it does not do: the organisation's and the groups' chain tokens are
mined and stay, and readers go on reading them (each member then holds no
current grant); a copy of a wrap or a grant anyone kept stays a copy; and
an epoch key a member opened stays opened. A tree another home of this
organisation minted is swept only once this home knows it (borg org sync
records the tree of every grant and wrap the hosts answer).`

// killTrees are the grant and record trees a home holds that still have an
// output no sweep of this home's spends, each once.
func killTrees(st *state.State, swept func(txid string, vout uint32) bool) []killTree {
	var out []killTree
	seen := map[string]bool{}
	for _, kind := range []string{state.TreeGrant, state.TreeRecord} {
		ts := st.Trees[kind]
		if ts == nil {
			continue
		}
		add := func(t *funding.Tree) {
			if t == nil || seen[t.Txid] {
				return
			}
			seen[t.Txid] = true
			kt := killTree{kind: kind, txid: t.Txid, count: t.Count}
			for v := uint32(0); v < t.Count; v++ {
				if !swept(t.Txid, v) {
					kt.vouts = append(kt.vouts, v)
				}
			}
			if st.Org != nil {
				for i := range st.Org.Grants {
					if gr := &st.Org.Grants[i]; gr.Tree == t.Txid && gr.Swept == "" {
						kt.grants++
					}
				}
			}
			if len(kt.vouts) > 0 {
				out = append(out, kt)
			}
		}
		add(ts.Tree)
		for i := range ts.Trees {
			add(&ts.Trees[i])
		}
		for i := range ts.Ahead {
			add(&ts.Ahead[i])
		}
	}
	return out
}

type killTree struct {
	kind   string
	txid   string
	count  uint32
	vouts  []uint32
	grants int
}

func cmdKill(ctx context.Context, g *global, args []string) error {
	fs := g.flagSet("kill", killHelp)
	confirm := fs.String("confirm", "", "must be the literal KILL")
	yes := fs.Bool("yes", false, "actually sweep: mine and publish")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usage("kill -confirm KILL [-yes]")
	}
	if *confirm != "KILL" {
		return usage("kill sweeps every grant and record tree of this home, revokes the whole roll and retracts every grant and wrap; pass -confirm KILL")
	}
	if !*yes {
		e, err := g.openWallet()
		if err != nil {
			return err
		}
		st, err := state.Load(g.cfg.Home, e.Signer().IdentityHex())
		if err != nil {
			return err
		}
		trees := killTrees(st, func(txid string, vout uint32) bool { return sweptIn(st, txid, vout) })
		for _, t := range trees {
			fmt.Fprintf(g.stdout, "would sweep %s tree %s: %d of %d output(s), %d grant(s) of this home's record on it\n", t.kind, t.txid, len(t.vouts), t.count, t.grants)
		}
		fmt.Fprintf(g.stdout, "%d tree(s) to sweep; nothing was sent (pass -yes)\n", len(trees))
		return nil
	}
	r, err := g.openOrg(ctx, false)
	if err != nil {
		return err
	}
	defer r.close(ctx, g)
	o := r.h.st.Org
	if o == nil {
		return usage("this home runs no organisation: it published no grant and no wrap, so there is nothing to sweep")
	}
	trees := killTrees(r.h.st, func(txid string, vout uint32) bool { return r.eng.SweptBy(txid, vout) != nil })
	failed := 0
	for _, t := range trees {
		sw, err := r.eng.Retract(ctx, o.Hall, t.txid, t.vouts)
		if err != nil {
			failed++
			g.say("%s tree %s: NOT SWEPT: %v", t.kind, t.txid, err)
			continue
		}
		fmt.Fprintf(g.stdout, "swept   %s tree %s with %s: %d output(s), mined at height %d\n", t.kind, t.txid, sw.Txid, len(sw.Vouts), sw.Height)
	}
	if err := r.eng.Retire(); err != nil {
		return err
	}
	if failed > 0 {
		return refused("%d of %d tree(s) not swept; run borg kill -confirm KILL -yes again", failed, len(trees))
	}
	fmt.Fprintf(g.stdout, "killed  %d tree(s) swept: every grant and wrap they funded is retracted at every honest host, and every grant revoked\n", len(trees))
	g.say("a sweep is not erasure: the chains stay, a copy anyone kept stays a copy, and an epoch key a member opened stays opened")
	return nil
}

// sweptIn reports whether a sweep recorded in st, not failed, spends an
// output.
func sweptIn(st *state.State, txid string, vout uint32) bool {
	for i := range st.Sweeps {
		sw := &st.Sweeps[i]
		if sw.Failed != "" || sw.Tree != txid {
			continue
		}
		for _, v := range sw.Vouts {
			if v == vout {
				return true
			}
		}
	}
	return false
}
