package module

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/borg/borgrec"
)

// The module inside the engine's admission path over an in-memory store
// (engine.test.ts): what the engine records for a refusal, the
// suppression attacks spec section 11.1 guards against, a token refused
// while the host's headers lag and admitted once they catch up, every
// golden transaction, answers hydrated from storage, restore from the
// engine's own storage in any order, and a sibling host filled by replay
// from what the first one stores. overlay-engine's integration test runs
// the module in the real engine.

func TestSuppressionNaiveManagerLosesAGenuineObject(t *testing.T) {
	v := loadTxVectors(t)
	for _, pair := range [][2]string{
		{"carrier-beef-tree-unproven", "grant-carrier-beef-v1"},
		{"token-alone", "org-token-revoke-predecessor-not-held"},
		{"token-parent-txid-only", "org-token-revoke-predecessor-not-held"},
	} {
		worse, genuine := v.byName(t, pair[0]), v.byName(t, pair[1])
		if worse.Txid != genuine.Txid {
			t.Fatalf("%s: another txid", pair[0])
		}
		naive := setup(t, engineOpts{naive: true})
		outs(t, naive.submit(worse.beef(t)))
		if !naive.storage.isApplied(genuine.Txid, topic) {
			t.Fatalf("%s: the refusal is recorded as applied", pair[0])
		}
		before := naive.calls
		outs(t, naive.submit(genuine.beef(t)))
		if naive.calls != before {
			t.Fatalf("%s: the genuine object is a duplicate the manager never sees", pair[0])
		}

		real := setup(t, engineOpts{})
		outs(t, real.submit(worse.beef(t)))
		if real.storage.isApplied(genuine.Txid, topic) {
			t.Fatalf("%s: nothing recorded", pair[0])
		}
		if real.host.count("borg_refused_total", map[string]string{"reason": "beef"}) != 1 {
			t.Fatalf("%s: counted", pair[0])
		}
		outs(t, real.submit(genuine.beef(t)), genuine.Admits.OutputsToAdmit[0])
	}
}

func TestATokenRefusedWhileHeadersLagIsAdmittedOnceTheyCatchUp(t *testing.T) {
	chain := newTestChain(71000)
	org := newOrgSim(t, hall, "lagging org", chain, orgOpts{})
	g0 := org.orgToken(nil, nil, tokenOpts{})
	g1 := org.orgToken(grantsOf(org.grant(newParty("lag member"))), nil, tokenOpts{})
	// The host's headers: everything but the block of seq 0, the parent of
	// seq 1.
	height := g0.tx.MerklePath.BlockHeight
	root, _ := chain.root(height)
	s := setup(t, engineOpts{chain: chain})
	chain.dropRoot(height)
	outs(t, s.submit(g1.beef))
	if s.host.count("borg_refused_total", map[string]string{"reason": "unmined"}) != 1 || s.storage.isApplied(idOf(g1.tx), topic) {
		t.Fatal("refused unmined, without a trace")
	}
	chain.setRoot(height, root)
	outs(t, s.submit(g1.beef), 0)
	if tokenStatus(t, s.ls, idOf(g1.tx)) != borgrec.StatusPending {
		t.Fatal("its predecessor is not held")
	}
	outs(t, s.submit(g0.beef), 0)
	if tokenStatus(t, s.ls, idOf(g1.tx)) != borgrec.StatusConsistent {
		t.Fatal("consistent once its predecessor is")
	}
}

func TestEveryTransactionVectorThroughTheEngine(t *testing.T) {
	v := loadTxVectors(t)
	s := setup(t, engineOpts{})
	admitted := map[string]bool{}
	for _, x := range v.Cases {
		if x.Verdict == "admit" {
			admitted[x.Txid] = true
		}
	}
	var engineRefused []string
	for i := range v.Cases {
		x := &v.Cases[i]
		got := s.submit(x.beef(t))
		if got == nil {
			// The engine's own SPV refuses before any manager runs: a proof
			// at a height its headers do not hold, a script its interpreter
			// refuses.
			if x.Verdict != "refuse" {
				t.Fatalf("%s: the engine's SPV refused an admitted vector", x.Name)
			}
			engineRefused = append(engineRefused, x.Name)
		}
		switch {
		case x.Verdict == "admit":
			if !s.storage.isApplied(x.Txid, topic) {
				t.Fatalf("%s: not applied", x.Name)
			}
		case !admitted[x.Txid]:
			if len(got) != 0 || s.storage.isApplied(x.Txid, topic) {
				t.Fatalf("%s: admitted %v", x.Name, got)
			}
		}
	}
	if len(v.Cases) != 100 || len(engineRefused) == 0 {
		t.Fatalf("%d cases, %d refused by the engine", len(v.Cases), len(engineRefused))
	}
	id := func(n string) string { return v.byName(t, n).Txid }
	c := func(n string) string { return v.byName(t, n).Admits.Commitment }
	// Statuses: the sweep spends bob's grant outpoint; a second carrier on
	// alice's is superseded.
	for _, w := range [][2]string{{"grant-carrier-bob", borgrec.StatusRetracted}, {"grant-carrier-alice", borgrec.StatusHeld},
		{"carrier-locktime-max", borgrec.StatusSuperseded}, {"wrap-carrier-epoch-1", borgrec.StatusHeld}} {
		if got := status(t, s.ls, c(w[0])); got != w[1] {
			t.Fatalf("%s: %s, want %s", w[0], got, w[1])
		}
	}
	for _, n := range []string{"org-token-genesis", "org-token-revoke", "group-token-create", "group-token-add", "group-token-remove-same-block"} {
		if got := tokenStatus(t, s.ls, id(n)); got != borgrec.StatusConsistent {
			t.Fatalf("%s: %s", n, got)
		}
	}
	// Answers hydrated from storage.
	orgRec, err := borgrec.DecodeOrg(unhex(t, v.byName(t, "org-token-genesis").Admits.Record))
	if err != nil {
		t.Fatal(err)
	}
	org := fmt.Sprintf("%x", orgRec.Org)
	group := "406504d3fbe1ae098f09a49dffa12222"
	alice := "029c1f585aa80762f9f4458f6806978b7befb46c7b68af82d399630793ff1a2b50"
	bob := "031428f3a3532ff4f1cac70f7292bfad06d1037f800ee8839b56ebba917a22e900"
	eq(t, s.answer(qm{"hall": hall, "org": org}), []string{id("org-token-revoke")}, "org")
	eq(t, s.answer(qm{"hall": hall, "org": org, "seq": "0"}), []string{id("org-token-genesis")}, "org-seq 0")
	eq(t, s.answer(qm{"hall": hall, "roll": org}), []string{id("grant-carrier-alice"), id("grant-carrier-carol")}, "roll")
	eq(t, s.answer(qm{"hall": hall, "org": org, "member": alice}), []string{id("grant-carrier-alice")}, "member alice")
	ctxs := contexts(t, s.ls, qm{"hall": hall, "org": org, "member": alice})
	ic, err := borgrec.DecodeContext(ctxs[0])
	if err != nil {
		t.Fatal(err)
	}
	revoke, err := borgrec.DecodeOrg(unhex(t, v.byName(t, "org-token-revoke").Admits.Record))
	if err != nil {
		t.Fatal(err)
	}
	var ca [32]byte
	copy(ca[:], unhex(t, c("grant-carrier-alice")))
	if ic.Seq != 1 || !ic.VerifyGrant(ca, revoke.Roll) {
		t.Fatal("the inclusion path verifies to the roll the answer used")
	}
	eq(t, s.answer(qm{"hall": hall, "org": org, "member": bob}), nil, "bob was revoked")
	eq(t, s.answer(qm{"hall": hall, "groupsOf": org}), []string{id("group-token-remove-same-block")}, "groups")
	eq(t, s.answer(qm{"hall": hall, "org": org, "group": group}), []string{id("group-token-remove-same-block")}, "group")
	eq(t, s.answer(qm{"hall": hall, "org": org, "memberOf": alice}), []string{id("group-token-remove-same-block")}, "member-of alice")
	eq(t, s.answer(qm{"hall": hall, "org": org, "memberOf": bob}), nil, "bob was removed")
	eq(t, s.answer(qm{"hall": hall, "org": org, "group": group, "epoch": "0"}), []string{id("wrap-carrier-epoch-0")}, "wraps 0")
	eq(t, s.answer(qm{"hall": hall, "org": org, "group": group, "epoch": "1", "member": alice}), []string{id("wrap-carrier-epoch-1")}, "wrap 1 alice")
	eq(t, s.answer(qm{"hall": hall, "org": org, "group": group, "epoch": "1", "member": bob}), nil, "no wrap of epoch 1 names bob")
	eq(t, s.answer(qm{"hall": hall, "spent": v.byName(t, "sweep").Admits.Spent[0]}), []string{id("sweep")}, "sweep")
	eq(t, s.answer(qm{"hall": hall, "history": org, "chain": group}), []string{id("group-token-create"), id("group-token-add"), id("group-token-remove-same-block")}, "history")
}

// snapshot is what a scenario's questions answer, with the statuses it
// holds.
type snap struct {
	answers  [][]string
	carriers []string
	tokens   []string
}

func snapshotOf(t *testing.T, ls *Service, questions []qm, carriers, tokens []string) snap {
	t.Helper()
	var s snap
	for _, q := range questions {
		s.answers = append(s.answers, answerOf(t, ls, q))
	}
	for _, c := range carriers {
		s.carriers = append(s.carriers, status(t, ls, c))
	}
	for _, x := range tokens {
		s.tokens = append(s.tokens, tokenStatus(t, ls, x))
	}
	return s
}

func TestRestoreReachesTheSameAnswersInAnyOrder(t *testing.T) {
	v := loadTxVectors(t)
	chain := newTestChain(72000).withVectors(t)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "restore org", chain, orgOpts{})
	a, b, c := newParty("restore a"), newParty("restore b"), newParty("restore c")
	ga, gb, gc := org.grant(a), org.grant(b), org.grant(c)
	o0 := org.orgToken(grantsOf(ga, gb, gc), nil, tokenOpts{})
	o1 := org.orgToken(nil, grantsOf(gb), tokenOpts{})
	grp := org.group("restore group", []*party{a, b}, false)
	w0 := grp.wraps(nil, 0)
	grp.change(changeOpts{add: []*party{c}})
	w0c := grp.wraps([][]byte{c.id}, 0)
	g2 := grp.change(changeOpts{remove: []*party{b}})
	w1 := grp.wraps(nil, 0)
	sw := mintSweep(t, org.grantTree, []uint32{gb.vout}, org.party, borgrec.GrantDerivation, chain)
	// In order, so seq 1 spends a held seq 0 and the engine keeps seq 0 as
	// a spent output.
	for _, beef := range [][]byte{o0.beef, o1.beef} {
		outs(t, s.submit(beef), 0)
	}
	if tokenStatus(t, s.ls, idOf(o1.tx)) != borgrec.StatusConsistent {
		t.Fatal("seq 1")
	}
	carriersTx := append(append(append([]*transaction.Transaction{ga.tx, gb.tx, gc.tx}, w0...), w0c...), w1...)
	for _, x := range carriersTx {
		outs(t, s.submitTx(x), 0)
	}
	// The group's tokens, the latest first: each pending until its parent
	// arrives.
	hist := grp.history
	outs(t, s.submit(hist[len(hist)-1].beef), 0)
	if tokenStatus(t, s.ls, idOf(g2.tx)) != borgrec.StatusPending {
		t.Fatal("pending")
	}
	for i := len(hist) - 2; i >= 0; i-- {
		outs(t, s.submit(hist[i].beef), 0)
	}
	if tokenStatus(t, s.ls, idOf(g2.tx)) != borgrec.StatusConsistent {
		t.Fatal("consistent")
	}
	outs(t, s.submitTx(sw), 0)
	alone := v.byName(t, "funding-tree-alone")
	outs(t, s.submit(alone.beef(t)), 0)

	questions := []qm{
		{"hall": hall, "org": org.hex()},
		{"hall": hall, "roll": org.hex()},
		{"hall": hall, "org": org.hex(), "member": a.hex},
		{"hall": hall, "groupsOf": org.hex()},
		{"hall": hall, "org": org.hex(), "memberOf": c.hex},
		{"hall": hall, "org": org.hex(), "memberOf": b.hex},
		{"hall": hall, "org": org.hex(), "group": grp.idHex(), "epoch": "0"},
		{"hall": hall, "org": org.hex(), "group": grp.idHex(), "epoch": "1"},
		{"hall": hall, "spent": fmt.Sprintf("%s.%d", idOf(org.grantTree), gb.vout)},
		{"hall": hall, "history": org.hex(), "chain": grp.idHex()},
		{"hall": hall, "spent": alone.Admits.Spent[0]},
	}
	var carriers []string
	for _, g := range []*grantMade{ga, gb, gc} {
		carriers = append(carriers, cHex(g.c))
	}
	for _, w := range append(append(w0, w0c...), w1...) {
		carriers = append(carriers, cOf(w))
	}
	tokens := ids(o0.tx, o1.tx, g2.tx)
	want := snapshotOf(t, s.ls, questions, carriers, tokens)
	if fmt.Sprint(want.tokens) != "[consistent consistent consistent]" || len(want.answers[5]) != 0 || len(want.answers[1]) != 2 {
		t.Fatalf("%+v", want)
	}
	rows := s.storage.utxos(topic, true)
	for _, o := range rows {
		if o.Outpoint.Txid.String() == idOf(o0.tx) {
			t.Fatal("a token its successor spent is not among the unspent rows")
		}
	}
	bare := s.storage.utxos(topic, false)
	reversed := make([]bmodule.Output, len(rows))
	for i := range rows {
		reversed[i] = rows[len(rows)-1-i]
	}
	noTokens := &MemoryJournal{}
	for _, l := range s.journal.Lines {
		if l.K != "t" {
			noTokens.Lines = append(noTokens.Lines, l)
		}
	}
	for _, x := range []struct {
		label   string
		order   []bmodule.Output
		journal *MemoryJournal
	}{
		{"as stored", rows, s.journal},
		{"reversed", reversed, s.journal},
		{"without BEEF", bare, s.journal},
		{"nothing handed over", nil, s.journal},
		{"without the token rows: walked back through the stored tokens", rows, noTokens},
	} {
		m, err := Mount(newFakeHost(), baseConfig(), []string{topic}, x.journal, chain, nil)
		if err != nil {
			t.Fatal(err)
		}
		n, err := m.LS.Restore(context.Background(), x.order, s.storage)
		if err != nil || n == 0 {
			t.Fatalf("%s: %d %v", x.label, n, err)
		}
		got := snapshotOf(t, m.LS, questions, carriers, tokens)
		if len(x.order) == 0 {
			// The bounded table is rebuilt from the tombstones the host
			// hands over and is never journaled, so a restore handed
			// nothing has no unmatched spends; everything the journal names
			// is read back.
			if len(got.answers[10]) != 0 {
				t.Fatalf("%s: an unmatched spend", x.label)
			}
			got.answers = got.answers[:10]
			want2 := want
			want2.answers = want.answers[:10]
			if !reflect.DeepEqual(got, want2) {
				t.Fatalf("%s:\n got  %+v\n want %+v", x.label, got, want2)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s:\n got  %+v\n want %+v", x.label, got, want)
		}
	}
}

func TestASiblingHostIsFilledByReplayOfStoredTokens(t *testing.T) {
	chain := newTestChain(73000).withVectors(t)
	first := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "replay org", chain, orgOpts{})
	grp := org.group("replay group", []*party{newParty("replay a"), newParty("replay b")}, false)
	grp.change(changeOpts{add: []*party{newParty("replay c")}})
	grp.change(changeOpts{remove: []*party{newParty("replay b")}})
	for _, m := range grp.history {
		outs(t, first.submit(m.beef), 0)
	}
	// What the first host answers is the token alone with its proof.
	var stored []*transaction.Transaction
	for seq := 0; seq < 3; seq++ {
		hs := hydrated(t, first.ls, qm{"hall": hall, "org": org.hex(), "group": grp.idHex(), "seq": fmt.Sprint(seq)})
		tx, tok, err := borgrec.StoredToken(hs[0].Beef)
		if err != nil || tok.Seq() != uint64(seq) { //nolint:gosec // test
			t.Fatalf("seq %d: %v", seq, err)
		}
		stored = append(stored, tx)
	}
	second := setup(t, engineOpts{chain: chain})
	// Any order: the latest first, each pending until its parent arrives.
	outs(t, second.submit(tokenBEEF(t, stored[2], stored[1])), 0)
	outs(t, second.submit(tokenBEEF(t, stored[1], stored[0])), 0)
	if tokenStatus(t, second.ls, idOf(stored[2])) != borgrec.StatusPending {
		t.Fatal("pending")
	}
	// At seq 0 the parent is the chain tree, which no host keeps: from the
	// publisher.
	outs(t, second.submit(tokenBEEF(t, stored[0], org.chainT)), 0)
	if tokenStatus(t, second.ls, idOf(stored[2])) != borgrec.StatusConsistent {
		t.Fatal("consistent")
	}
	for _, q := range []qm{{"hall": hall, "org": org.hex(), "group": grp.idHex()}, {"hall": hall, "history": org.hex(), "chain": grp.idHex()}} {
		eq(t, second.answer(q), first.answer(q), fmt.Sprint(q))
	}
}

func TestAnswersAreHydratedAndADroppedCarrierIsNeverRestored(t *testing.T) {
	chain := newTestChain(74000).withVectors(t)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "evidence org", chain, orgOpts{})
	g, many := equivocationsOf(t, org, "evidence group", 10)
	// Highest commitment first, so the highest is kept when it arrives and
	// pushed out by a lower one later.
	for i := len(many) - 1; i >= 0; i-- {
		outs(t, s.submitTx(many[i]), 0)
	}
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "group": g.idHex(), "epoch": "0"}), ids(many[0]), "one answered carrier per funding output")
	// The winner and eight superseded are kept; the tenth was dropped and
	// deleted.
	if s.storage.find(idOf(many[9]), 0, topic) != nil {
		t.Fatal("the tenth is deleted")
	}
	for _, x := range many[:9] {
		if s.storage.find(idOf(x), 0, topic) == nil {
			t.Fatal("the nine are stored")
		}
	}
	if s.host.count("borg_dropped_total", map[string]string{"why": "evidence"}) != 1 {
		t.Fatal("dropped once")
	}
	outs(t, s.submitTx(many[9]))
	again := s.restored(nil)
	if again.Counts().Wraps != 9 {
		t.Fatal("nine wraps")
	}
	// A carrier past the evidence bound leaves nothing behind: its output
	// was deleted, so a restore does not bring it back. This carrier was
	// kept when it arrived and pushed out later by a lower one, so its c
	// line is in the journal: a restart replays the line, rebuilds the row
	// and applies the bound again.
	if _, ok := again.Status(cOf(many[9])); ok {
		t.Fatal("not kept, and so not answered")
	}
	if c := again.Counts(); c.Lines != 9 || c.Forgotten != 1 {
		t.Fatalf("%+v", c)
	}
}
