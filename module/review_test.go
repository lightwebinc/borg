package module

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/borg/borgrec"
)

// The contract review's attacks, replayed against the module on the
// engine's admission path (review.test.ts). Spec section 11.1 (the parent
// in the token's BEEF, merged paths, what is never admitted) and 11.2
// (pending, the bounded spend table, one answered carrier per outpoint)
// are attacked here; the rest are where their own subject is tested.

func TestSuppressionEveryWayTheVectorsMakeIt(t *testing.T) {
	v := loadTxVectors(t)
	admitted := map[string]*txVector{}
	for i := range v.Cases {
		if v.Cases[i].Verdict == "admit" {
			admitted[v.Cases[i].Txid] = &v.Cases[i]
		}
	}
	pairs := 0
	// A case that names its own sweep bound is refused only under that
	// bound.
	for i := range v.Cases {
		worse := &v.Cases[i]
		genuine := admitted[worse.Txid]
		if worse.Verdict != "refuse" || genuine == nil || worse.MaxSweepInputs != 0 {
			continue
		}
		s := setup(t, engineOpts{})
		if got := s.submit(worse.beef(t)); len(got) != 0 {
			t.Fatalf("%s: admitted %v", worse.Name, got)
		}
		if s.storage.isApplied(worse.Txid, topic) {
			t.Fatalf("%s: recorded", worse.Name)
		}
		if got := s.submit(genuine.beef(t)); fmt.Sprint(got) != fmt.Sprint(genuine.Admits.OutputsToAdmit) {
			t.Fatalf("%s then %s: %v", worse.Name, genuine.Name, got)
		}
		pairs++
	}
	if pairs != 18 {
		t.Fatalf("%d pairs", pairs)
	}
}

func TestATokenMinedInItsParentsBlock(t *testing.T) {
	chain := newTestChain(91000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "same block org", chain, orgOpts{})
	o0 := org.orgToken(nil, nil, tokenOpts{unmined: true})
	o1 := org.orgToken(nil, nil, tokenOpts{unmined: true})
	chain.mineTogether(t, o0.tx, o1.tx)
	// The chain tree was mined in an earlier block; seq 0 and seq 1 share
	// one.
	outs(t, s.submit(tokenBEEF(t, o0.tx, org.chainT)), 0)
	outs(t, s.submit(tokenBEEF(t, o1.tx, o0.tx)), 0)
	if tokenStatus(t, s.ls, idOf(o1.tx)) != borgrec.StatusConsistent {
		t.Fatal("consistent")
	}
}

func TestSpendingOnlyHeldOutputsIsRefused(t *testing.T) {
	v := loadTxVectors(t)
	s := setup(t, engineOpts{})
	sweep := v.byName(t, "sweep")
	outs(t, s.submit(sweep.beef(t)), 0)
	spend := v.byName(t, "spend-held-tombstone")
	outs(t, s.submit(spend.beef(t)))
	if s.host.count("borg_refused_total", map[string]string{"reason": "not-borg"}) != 1 || s.storage.isApplied(spend.Txid, topic) {
		t.Fatal("refused not-borg, nothing recorded")
	}
	if o := s.storage.find(sweep.Txid, 0, topic); o == nil || o.Spent {
		t.Fatal("the held output is not marked spent")
	}
}

func TestAMinedFundingTreeSubmittedAlone(t *testing.T) {
	v := loadTxVectors(t)
	s := setup(t, engineOpts{})
	tree := v.byName(t, "funding-tree-alone")
	outs(t, s.submit(tree.beef(t)), 0)
	held := 0
	for _, o := range s.storage.utxos(topic, false) {
		if o.Outpoint.Txid.String() == tree.Txid {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("%d outputs held", held)
	}
	if c := s.ls.Counts(); c.Table != 1 || c.Rows != 0 {
		t.Fatalf("%+v", c)
	}
	for _, l := range s.journal.Lines {
		if l.K == "s" {
			t.Fatal("nothing journaled for life")
		}
	}
}

func TestAFloodOfTokensAtOneSeqKeepsTheLowestEight(t *testing.T) {
	chain := newTestChain(92000)
	org := newOrgSim(t, hall, "fork flood org", chain, orgOpts{})
	g0 := org.orgToken(nil, nil, tokenOpts{})
	var forks []*transaction.Transaction
	var beefs [][]byte
	for i := 0; i < 10; i++ {
		org.tip = g0
		org.roll = nil
		n := fmt.Sprintf("fork %d", i)
		m := org.orgToken(nil, nil, tokenOpts{mutate: func(r *borgrec.Org) { r.Name = n }})
		forks = append(forks, m.tx)
		beefs = append(beefs, m.beef)
	}
	want := byTxid(forks...)[:8]
	latest := func(order []int) []string {
		s := setup(t, engineOpts{chain: chain})
		s.submit(g0.beef)
		for _, i := range order {
			outs(t, s.submit(beefs[i]), 0)
		}
		got := formulaTxids(t, s.ls, qm{"hall": hall, "org": org.hex()})
		sort.Strings(got)
		return got
	}
	eq(t, latest([]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}), want, "in order")
	eq(t, latest([]int{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}), want, "reversed")
	// A restored index keeps the same eight from storage, which holds all
	// ten.
	s := setup(t, engineOpts{chain: chain})
	s.submit(g0.beef)
	for _, b := range beefs {
		s.submit(b)
	}
	ls, err := NewService(Options{Halls: []string{hall}, Host: newFakeHost(), Journal: s.journal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Restore(context.Background(), s.storage.utxos(topic, true), s.storage); err != nil {
		t.Fatal(err)
	}
	got := formulaTxids(t, ls, qm{"hall": hall, "org": org.hex()})
	sort.Strings(got)
	eq(t, got, want, "restored")
}
