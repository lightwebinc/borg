package module

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/borg/borgrec"
)

// ls_borg through the engine's admission path (ls_borg.test.ts): every
// lookup class answered, paged and refused as spec section 10.2 says; each
// token status of section 11.2 and the answers that follow from it
// (pending, a fork, a broken fold); the bounded table of spends no carrier
// used; the evidence kept per outpoint; and retention at its floors
// (section 11.3).

const day = 86400

func grantsOf(gs ...*grantMade) [][32]byte {
	out := make([][32]byte, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.c)
	}
	return out
}

func grantTxs(gs ...*grantMade) []*transaction.Transaction {
	out := make([]*transaction.Transaction, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.tx)
	}
	return out
}

func policyTxs(ps ...*policyMade) []*transaction.Transaction {
	out := make([]*transaction.Transaction, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.tx)
	}
	return out
}

// subjectOf is the subject transaction of an answered output's BEEF.
func subjectOf(t *testing.T, beef []byte) *transaction.Transaction {
	t.Helper()
	_, tx, _, err := guard.ParseBEEF(beef, guard.DefaultBound)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func hydrated(t *testing.T, ls *Service, q any) []bmodule.HydratedOutput {
	t.Helper()
	a, err := ls.Lookup(context.Background(), question(q))
	if err != nil {
		t.Fatal(err)
	}
	hs, _ := a.Result.([]bmodule.HydratedOutput)
	return hs
}

func TestEveryFreeClassAnswersAndPages(t *testing.T) {
	chain := newTestChain(80000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "lookup org", chain, orgOpts{trees: 80})
	var people []*party
	for i := 0; i < 69; i++ {
		people = append(people, newParty(fmt.Sprintf("lookup member %d", i)))
	}
	twice := people[0]
	var grants []*grantMade
	for _, p := range people {
		grants = append(grants, org.grant(p))
	}
	grants = append(grants, org.grantWith(twice, grantValues{expiry: "2099-12-31T23:59:59Z", role: "admin", scope: "member,admin"}, org.nextGrantVout(), lockTime0))
	for _, g := range grants {
		outs(t, s.submitTx(g.tx), 0)
	}
	o0 := org.orgToken(grantsOf(grants[:64]...), nil, tokenOpts{})
	o1 := org.orgToken(grantsOf(grants[64:]...), nil, tokenOpts{})
	for _, m := range []*minted{o0, o1} {
		outs(t, s.submit(m.beef), 0)
	}

	// org, org-seq.
	eq(t, s.answer(qm{"hall": hall, "org": org.hex()}), ids(o1.tx), "org")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "seq": "0"}), ids(o0.tx), "org-seq 0")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "seq": "7"}), nil, "org-seq 7")
	// roll, roll-after: 70 grants, 64 a page, by txid.
	all := byTxid(grantTxs(grants...)...)
	page1 := s.answer(qm{"hall": hall, "roll": org.hex()})
	eq(t, page1, all[:64], "roll")
	eq(t, s.answer(qm{"hall": hall, "roll": org.hex(), "after": page1[63]}), all[64:], "roll-after")
	eq(t, s.answer(qm{"hall": hall, "roll": org.hex(), "after": all[69]}), nil, "roll-after the last")
	// member: both of a subject's grants, each with an inclusion path to the roll.
	hs := hydrated(t, s.ls, qm{"hall": hall, "org": org.hex(), "member": twice.hex})
	if len(hs) != 2 {
		t.Fatalf("member: %d outputs", len(hs))
	}
	for _, h := range hs {
		ic, err := borgrec.DecodeContext(h.Context)
		if err != nil {
			t.Fatal(err)
		}
		if ic.Seq != 1 || ic.Size != 70 {
			t.Fatalf("context seq %d size %d", ic.Seq, ic.Size)
		}
		if !ic.VerifyGrant(commitment(subjectOf(t, h.Beef)), o1.org.Roll) {
			t.Fatal("the inclusion path does not verify to the roll")
		}
	}

	// Groups: three, one of them without people[1].
	g1 := org.group("first", []*party{people[1], people[2]}, false)
	g2 := org.group("second", []*party{people[2]}, false)
	g3 := org.group("third", []*party{people[1], people[3]}, false)
	g3.change(changeOpts{add: []*party{people[4]}})
	for _, g := range []*groupSim{g1, g2, g3} {
		for _, m := range g.history {
			outs(t, s.submit(m.beef), 0)
		}
	}
	gs := []*groupSim{g1, g2, g3}
	sortGroups(gs)
	tips := func(gs []*groupSim) []string {
		var out []string
		for _, g := range gs {
			out = append(out, idOf(g.tip.tx))
		}
		return out
	}
	eq(t, s.answer(qm{"hall": hall, "groupsOf": org.hex()}), tips(gs), "groups")
	eq(t, s.answer(qm{"hall": hall, "groupsOf": org.hex(), "after": gs[0].idHex()}), tips(gs[1:]), "groups-after")
	var with1 []*groupSim
	for _, g := range gs {
		if g != g2 {
			with1 = append(with1, g)
		}
	}
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": people[1].hex}), tips(with1), "member-of")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": people[1].hex, "after": with1[0].idHex()}), tips(with1[1:]), "member-of-after")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": people[50].hex}), nil, "member-of nobody")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "group": g3.idHex()}), ids(g3.tip.tx), "group")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "group": g3.idHex(), "seq": "0"}), ids(g3.history[0].tx), "group-seq")

	// wraps, wraps-after, wrap: 66 recipients one to a record, 64 a page.
	big := org.group("big", people[:60], false)
	big.change(changeOpts{add: people[60:66]})
	for _, m := range big.history {
		s.submit(m.beef)
	}
	wraps := big.wraps(nil, 1)
	for _, w := range wraps {
		outs(t, s.submitTx(w), 0)
	}
	wAll := byTxid(wraps...)
	wq := qm{"hall": hall, "org": org.hex(), "group": big.idHex(), "epoch": "0"}
	eq(t, s.answer(wq), wAll[:64], "wraps")
	eq(t, s.answer(with(wq, "after", wAll[63])), wAll[64:], "wraps-after")
	var to5 *transaction.Transaction
	for _, w := range wraps {
		if strings.Contains(hex.EncodeToString(*w.Outputs[0].LockingScript), people[5].hex) {
			to5 = w
		}
	}
	eq(t, s.answer(with(wq, "member", people[5].hex)), ids(to5), "wrap")
	eq(t, s.answer(with(wq, "epoch", "1")), nil, "wraps of an epoch never begun")

	// sweep: a mined sweep of two grant-tree outputs answers for each.
	sw := mintSweep(t, org.grantTree, []uint32{grants[5].vout, 79}, org.party, borgrec.GrantDerivation, chain)
	outs(t, s.submitTx(sw), 0)
	eq(t, s.answer(qm{"hall": hall, "spent": fmt.Sprintf("%s.%d", idOf(org.grantTree), grants[5].vout)}), ids(sw), "sweep")
	eq(t, s.answer(qm{"hall": hall, "spent": idOf(org.grantTree) + ".79"}), ids(sw), "sweep of an unused output")
	// The roll answers the swept grant still: it is in the roll whatever its status (spec 11.2).
	if got := status(t, s.ls, cHex(grants[5].c)); got != borgrec.StatusRetracted {
		t.Fatalf("swept grant %s", got)
	}
	rolled := append(s.answer(qm{"hall": hall, "roll": org.hex()}), s.answer(qm{"hall": hall, "roll": org.hex(), "after": page1[63]})...)
	if !contains(rolled, idOf(grants[5].tx)) {
		t.Fatal("the roll leaves out a swept grant it holds")
	}
}

const lockTime0 = 4102444800

func with(q qm, k, v string) qm {
	out := qm{}
	for a, b := range q {
		out[a] = b
	}
	out[k] = v
	return out
}

func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

func sortGroups(gs []*groupSim) {
	for i := range gs {
		for j := i + 1; j < len(gs); j++ {
			if gs[j].idHex() < gs[i].idHex() {
				gs[i], gs[j] = gs[j], gs[i]
			}
		}
	}
}

func TestHistoryPagesBySeqAndIsPriced(t *testing.T) {
	chain := newTestChain(81000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "history org", chain, orgOpts{})
	g := org.group("long", []*party{newParty("h a")}, false)
	for i := 0; i < 65; i++ {
		g.change(changeOpts{rotate: true})
	}
	for _, m := range g.history {
		outs(t, s.submit(m.beef), 0)
	}
	var all []string
	for _, m := range g.history {
		all = append(all, idOf(m.tx))
	}
	if len(all) != 66 {
		t.Fatalf("%d tokens", len(all))
	}
	q := qm{"hall": hall, "history": org.hex(), "chain": g.idHex()}
	eq(t, s.answer(q), all[:64], "history")
	eq(t, s.answer(with(q, "after", "63")), all[64:], "history-after 63")
	eq(t, s.answer(with(q, "after", "65")), nil, "history-after 65")
	o := org.orgToken(nil, nil, tokenOpts{})
	s.submit(o.beef)
	eq(t, s.answer(qm{"hall": hall, "history": org.hex(), "chain": "org"}), ids(o.tx), "history of the org chain")
	// Priced: the engine's own route refuses it.
	c := baseConfig()
	c.Prices = map[string]uint64{"history": 5, "history-after": 5}
	m, err := Mount(newFakeHost(), c, []string{topic}, &MemoryJournal{}, chain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), nil, s.storage); err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Lookup(context.Background(), question(q)); err == nil || !strings.Contains(err.Error(), "priced") {
		t.Fatalf("a priced class through the engine: %v", err)
	}
}

func TestAQuestionIsRefusedUnlessExactlyOneClass(t *testing.T) {
	s := setup(t, engineOpts{})
	bad := "02" + strings.Repeat("1", 64)
	key := "029c1f585aa80762f9f4458f6806978b7befb46c7b68af82d399630793ff1a2b50"
	for _, c := range []struct {
		q   any
		why string
	}{
		{qm{"hall": "other_hall_abcdefghij", "org": key}, "does not carry"},
		{qm{"hall": hall, "org": key, "extra": "x"}, "no class"},
		{qm{"hall": hall, "org": key, "history": key, "chain": "org"}, "no class"},
		{qm{"hall": hall, "org": key, "seq": "01"}, "seq"},
		{map[string]any{"hall": hall, "org": key, "seq": 7}, "not a string"},
		{qm{"hall": hall, "org": bad}, "org"},
		{qm{"hall": hall, "spent": "nope"}, "spent"},
		{qm{"hall": hall, "roll": key, "after": strings.Repeat("g", 64)}, "after"},
		{"not an object", "JSON object"},
	} {
		if _, err := s.ls.Lookup(context.Background(), question(c.q)); err == nil || !strings.Contains(err.Error(), c.why) {
			t.Fatalf("%v: %v, want %q", c.q, err, c.why)
		}
	}
	q := question(qm{"hall": hall, "org": key})
	q.Service = "ls_other"
	if _, err := s.ls.Lookup(context.Background(), q); err == nil || !strings.Contains(err.Error(), "ls_borg") {
		t.Fatalf("another service: %v", err)
	}
}

func TestStatusesPendingForkAndBrokenFold(t *testing.T) {
	chain := newTestChain(82000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "status org", chain, orgOpts{})
	a, b, c := newParty("st a"), newParty("st b"), newParty("st c")
	ga, gb, gc := org.grant(a), org.grant(b), org.grant(c)
	for _, g := range []*grantMade{ga, gb, gc} {
		s.submitTx(g.tx)
	}
	o0 := org.orgToken(grantsOf(ga), nil, tokenOpts{})
	o1 := org.orgToken(grantsOf(gb), nil, tokenOpts{})
	o2 := org.orgToken(grantsOf(gc), nil, tokenOpts{})
	o3 := org.orgToken(nil, grantsOf(ga), tokenOpts{})
	for _, m := range []*minted{o0, o1, o3} {
		outs(t, s.submit(m.beef), 0)
	}
	// seq 3 arrived before seq 2: pending; the latest answer is seq 3, the roll is seq 1's.
	if got := tokenStatus(t, s.ls, idOf(o3.tx)); got != borgrec.StatusPending {
		t.Fatalf("seq 3 %s", got)
	}
	if got := s.host.gauge("borg_tokens_pending"); got != 1 {
		t.Fatalf("borg_tokens_pending %v", got)
	}
	eq(t, s.answer(qm{"hall": hall, "org": org.hex()}), ids(o3.tx), "org")
	eq(t, s.answer(qm{"hall": hall, "roll": org.hex()}), byTxid(ga.tx, gb.tx), "roll of seq 1")
	outs(t, s.submit(o2.beef), 0)
	if got := tokenStatus(t, s.ls, idOf(o3.tx)); got != borgrec.StatusConsistent {
		t.Fatalf("seq 3 %s", got)
	}
	eq(t, s.answer(qm{"hall": hall, "roll": org.hex()}), byTxid(gb.tx, gc.tx), "roll of seq 3")

	// A fork at seq 1 of a group (two tokens on one predecessor, as a
	// reorganization leaves): both answered as latest, the roll of the first
	// mined used.
	g := org.group("fork", []*party{a}, false)
	g0 := g.tip
	left := g.change(changeOpts{add: []*party{b}})
	// Build the other branch on the same predecessor.
	g.tip = g0
	g.members = [][]byte{a.id}
	right := g.change(changeOpts{add: []*party{c}})
	for _, m := range []*minted{g0, right, left} {
		outs(t, s.submit(m.beef), 0)
	}
	got := s.answer(qm{"hall": hall, "org": org.hex(), "group": g.idHex()})
	eq(t, sortedCopy(got), byTxid(left.tx, right.tx), "both answered as latest")
	first, other, firstTx := b, c, left.tx
	if right.tx.MerklePath.BlockHeight < left.tx.MerklePath.BlockHeight {
		first, other, firstTx = c, b, right.tx
	}
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": first.hex}), ids(firstTx), "member-of the first mined")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "memberOf": other.hex}), nil, "member-of the other branch")

	// roll-mismatch: a token whose roll root is wrong is admitted (the host
	// reads only its predecessor) and flagged; so is its successor.
	bad := org.orgToken(nil, nil, tokenOpts{mutate: func(r *borgrec.Org) { r.Roll = borgrec.RollRoot(nil) }})
	after := org.orgToken(nil, nil, tokenOpts{})
	for _, m := range []*minted{bad, after} {
		outs(t, s.submit(m.beef), 0)
	}
	for _, m := range []*minted{bad, after} {
		if got := tokenStatus(t, s.ls, idOf(m.tx)); got != borgrec.StatusRollMismatch {
			t.Fatalf("%s", got)
		}
	}
	if s.host.count("borg_token_status_total", map[string]string{"status": "roll-mismatch"}) < 2 {
		t.Fatal("roll-mismatch is counted")
	}
}

func sortedCopy(a []string) []string {
	out := append([]string(nil), a...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestGroupStatusesAreReportedNeverRefused(t *testing.T) {
	chain := newTestChain(83000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "group status org", chain, orgOpts{})
	a, b, c := newParty("gs a"), newParty("gs b"), newParty("gs c")
	g := org.group("statuses", []*party{a, b}, false)
	g.change(changeOpts{remove: []*party{b}})
	rejoin := g.change(changeOpts{add: []*party{b}, keepEpoch: true})
	reuse := g.change(changeOpts{reuse: u64(0)})
	broken := g.change(changeOpts{add: []*party{c}, mutate: func(r *borgrec.Group) { r.Members = borgrec.RollRoot(nil) }})
	later := g.change(changeOpts{rotate: true})
	for _, m := range g.history {
		outs(t, s.submit(m.beef), 0)
	}
	for _, w := range []struct {
		m    *minted
		want string
	}{{rejoin, borgrec.StatusRejoinUnrotated}, {reuse, borgrec.StatusCommitmentReuse}, {broken, borgrec.StatusMembersMismatch},
		{later, borgrec.StatusMembersMismatch}, {g.history[1], borgrec.StatusConsistent}} {
		if got := tokenStatus(t, s.ls, idOf(w.m.tx)); got != w.want {
			t.Fatalf("seq %d: %s, want %s", w.m.grp.Seq, got, w.want)
		}
	}
	for _, st := range []string{"rejoin-unrotated", "commitment-reuse", "members-mismatch"} {
		if s.host.count("borg_token_status_total", map[string]string{"status": st}) < 1 {
			t.Fatalf("%s not counted", st)
		}
	}
	if !s.host.loggedWith("ls_borg token status", "status", "rejoin-unrotated") {
		t.Fatal("a status other than consistent is logged")
	}
}

func TestBoundedSpendTable(t *testing.T) {
	chain := newTestChain(84000)
	s := setup(t, engineOpts{chain: chain, spendTable: 3})
	org := newOrgSim(t, hall, "table org", chain, orgOpts{})
	// The sweep arrives first: its spend waits in the table, then joins the
	// carrier's row for life.
	ga := org.grant(newParty("table a"))
	early := mintSweep(t, org.grantTree, []uint32{ga.vout}, org.party, borgrec.GrantDerivation, chain)
	outs(t, s.submitTx(early), 0)
	if s.ls.Counts().Table != 1 {
		t.Fatal("the early spend waits in the table")
	}
	outs(t, s.submitTx(ga.tx), 0)
	if got := status(t, s.ls, cHex(ga.c)); got != borgrec.StatusRetracted || s.ls.Counts().Table != 0 {
		t.Fatalf("%s, table %d", got, s.ls.Counts().Table)
	}
	found := false
	for _, l := range s.journal.Lines {
		found = found || (l.K == "s" && l.S == idOf(early))
	}
	if !found {
		t.Fatal("the spend is journaled with the row")
	}
	// A stranger mines five sweeps of its own outputs: the table keeps the
	// newest three.
	stranger := newParty("stranger")
	tree := fundingTree(t, stranger, borgrec.RecordDerivation, 5, chain)
	var sweeps []*transaction.Transaction
	for i := uint32(0); i < 5; i++ {
		sweeps = append(sweeps, mintSweep(t, tree, []uint32{i}, stranger, borgrec.RecordDerivation, chain))
	}
	for _, x := range sweeps {
		outs(t, s.submitTx(x), 0)
	}
	if s.ls.Counts().Table != 3 || s.host.count("borg_dropped_total", map[string]string{"why": "table"}) != 2 {
		t.Fatalf("table %d, dropped %v", s.ls.Counts().Table, s.host.count("borg_dropped_total", map[string]string{"why": "table"}))
	}
	for i, x := range sweeps {
		kept := i >= 2
		if (s.storage.find(idOf(x), 0, topic) != nil) != kept {
			t.Fatalf("sweep %d stored", i)
		}
		want := []string{}
		if kept {
			want = ids(x)
		}
		eq(t, s.answer(qm{"hall": hall, "spent": fmt.Sprintf("%s.%d", idOf(tree), i)}), want, fmt.Sprintf("sweep %d", i))
	}
	for _, l := range s.journal.Lines {
		for _, x := range sweeps {
			if l.K == "s" && l.S == idOf(x) {
				t.Fatal("a stranger's spend is journaled")
			}
		}
	}
	// The retraction outlives a restart; the table is rebuilt from the kept
	// tombstones.
	again := s.restored(nil)
	if got := status(t, again, cHex(ga.c)); got != borgrec.StatusRetracted || again.Counts().Table != 3 {
		t.Fatalf("after a restart: %s, table %d", got, again.Counts().Table)
	}
}

func TestEvidenceSixtyFourSupersededGrants(t *testing.T) {
	chain := newTestChain(85000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "evidence org", chain, orgOpts{trees: 2})
	g := issueGrant(t, hall, org.party, newParty("evidence subject"), org.grantTree, 0, defaultValues, true)
	rec := encode(t, g)
	var many []*transaction.Transaction
	for i := 0; i < 66; i++ {
		many = append(many, mintCarrier(t, org.grantTree, 0, org.party, borgrec.GrantDerivation, rec, lockTime0+uint32(i))) //nolint:gosec // test
	}
	for _, x := range many {
		outs(t, s.submitTx(x), 0)
	}
	sorted := byC(many)
	if got := status(t, s.ls, cOf(sorted[0])); got != borgrec.StatusHeld {
		t.Fatalf("winner %s", got)
	}
	if s.ls.Counts().Grants != 65 || s.host.count("borg_dropped_total", map[string]string{"why": "evidence"}) != 1 {
		t.Fatalf("grants %d", s.ls.Counts().Grants)
	}
	// The row keeps a line for what it answers and nothing for the rest, so
	// neither memory nor journal grows with what is offered on an outpoint.
	if s.ls.Counts().Lines != 65 {
		t.Fatalf("lines %d", s.ls.Counts().Lines)
	}
	if _, ok := s.ls.Status(cOf(sorted[65])); ok {
		t.Fatal("the 66th has no record")
	}
	// Offered again it meets the same row and is refused again, which is
	// why forgetting it is safe and why the refusal costs no journal line.
	lines := s.ls.Counts().Lines
	outs(t, s.submitTx(sorted[65]))
	if s.ls.Counts().Lines != lines || s.ls.Counts().Grants != 65 {
		t.Fatal("a refused carrier adds no line and is not answered")
	}
	// The engine turns the re-offer away as a duplicate before the module
	// is asked, so the module's own refusal is not reached a second time.
	if s.host.count("borg_dropped_total", map[string]string{"why": "evidence"}) != 1 {
		t.Fatal("dropped once")
	}
}

func TestRetentionOfGrantsAndWraps(t *testing.T) {
	chain := newTestChain(86000)
	s := setup(t, engineOpts{chain: chain, now: 1_900_000_000})
	org := newOrgSim(t, hall, "retention org", chain, orgOpts{})
	a, b, c, d := newParty("ret a"), newParty("ret b"), newParty("ret c"), newParty("ret d")
	ga, gb, gc, gd := org.grant(a), org.grant(b), org.grant(c), org.grant(d)
	for _, x := range []*grantMade{ga, gb, gc, gd} {
		s.submitTx(x.tx)
	}
	o0 := org.orgToken(grantsOf(ga, gb, gc), nil, tokenOpts{})
	s.submit(o0.beef)
	grp := org.group("kept", []*party{a, b}, false)
	s.submit(grp.tip.beef)
	w0 := grp.wraps(nil, 0)
	for _, w := range w0 {
		s.submitTx(w)
	}
	s.clock.add(10 * day)
	o1 := org.orgToken(nil, grantsOf(gb), tokenOpts{})
	sw := mintSweep(t, org.grantTree, []uint32{gc.vout}, org.party, borgrec.GrantDerivation, chain)
	g1 := grp.change(changeOpts{remove: []*party{b}})
	w1 := grp.wraps(nil, 0)
	for _, beef := range [][]byte{o1.beef, atomicOf(t, sw), g1.beef} {
		outs(t, s.submit(beef), 0)
	}
	for _, w := range w1 {
		outs(t, s.submitTx(w), 0)
	}
	ctx := context.Background()
	s.clock.add(30 * day)
	if n := s.ls.Prune(ctx); n != 0 {
		t.Fatalf("under the floor %d went", n)
	}
	s.clock.add(1 * day)
	if n := s.ls.Prune(ctx); n != 1+len(w0) {
		t.Fatalf("%d went, want b's grant and epoch 0's %d wraps", n, len(w0))
	}
	if !s.ls.Dropped(cHex(gb.c)) {
		t.Fatal("revoked 31 days ago")
	}
	if s.ls.Dropped(cHex(gc.c)) || s.ls.Dropped(cHex(ga.c)) || s.ls.Dropped(cHex(gd.c)) {
		t.Fatal("swept but in the roll, in the roll, or never admitted and never swept: kept")
	}
	for _, w := range w0 {
		if !s.ls.Dropped(cOf(w)) {
			t.Fatal("epoch 0 ended 31 days ago")
		}
	}
	for _, w := range w1 {
		if s.ls.Dropped(cOf(w)) {
			t.Fatal("the current epoch's wraps are kept")
		}
	}
	if s.storage.find(idOf(gb.tx), 0, topic) != nil {
		t.Fatal("deleted from storage")
	}
	// Tokens are kept for the host's life.
	if s.ls.Counts().Tokens != 4 {
		t.Fatalf("tokens %d", s.ls.Counts().Tokens)
	}
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "seq": "0"}), ids(o0.tx), "org-seq 0")
}

func TestPolicyRoleAndBoundAnswerTheSetWithPaths(t *testing.T) {
	chain := newTestChain(80000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "policy org", chain, orgOpts{})
	alice := newParty("policy alice")
	gid := make([]byte, 16)
	for i := range gid {
		gid[i] = 7
	}
	def := org.policyRecord("releaser", policyOpts{permissions: []string{"bsecret/release/payroll", "bstore/get/*"}})
	other := org.policyRecord("moderator", policyOpts{permissions: []string{"bchat/moderate/general"}})
	toGroup := org.policyRecord("releaser", policyOpts{to: gid})
	toKey := org.policyRecord("releaser", policyOpts{to: alice.id, permissions: []string{"bstore/get/*"}})
	retired := org.policyRecord("moderator", policyOpts{to: alice.id})
	for _, p := range []*policyMade{def, other, toGroup, toKey, retired} {
		outs(t, s.submitTx(p.tx), 0)
	}
	// seq 0 defines all five, seq 1 retires one: the answers follow the set
	// the latest token commits to, never what the host happens to hold.
	o0 := org.orgToken(nil, nil, tokenOpts{define: [][32]byte{def.c, other.c, toGroup.c, toKey.c, retired.c}})
	outs(t, s.submit(o0.beef), 0)
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(policyTxs(def, other, toGroup, toKey, retired)...), "policy at seq 0")
	o1 := org.orgToken(nil, nil, tokenOpts{retire: [][32]byte{retired.c}})
	outs(t, s.submit(o1.beef), 0)
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(policyTxs(def, other, toGroup, toKey)...), "policy at seq 1")

	// role: the definition and every binding that names it.
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "role": "releaser"}), byTxid(policyTxs(def, toGroup, toKey)...), "role releaser")
	// A retired record leaves every answer, whatever still names it.
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "role": "moderator"}), byTxid(other.tx), "role moderator")
	// bound: by group identifier and by key, told apart by length.
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "boundTo": hex.EncodeToString(gid)}), byTxid(toGroup.tx), "bound to the group")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "boundTo": alice.hex}), byTxid(toKey.tx), "bound to the key")
	// A cursor moves past what it names.
	page := s.answer(qm{"hall": hall, "org": org.hex(), "role": "releaser"})
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "role": "releaser", "after": page[0]}), page[1:], "role-after")

	// Every role and bound answer carries a path from the record's
	// commitment to the policy root of the token its seq names, and to no
	// other root.
	hs := hydrated(t, s.ls, qm{"hall": hall, "org": org.hex(), "role": "releaser"})
	if len(hs) != 3 {
		t.Fatalf("%d outputs", len(hs))
	}
	for _, h := range hs {
		ic, err := borgrec.DecodeContext(h.Context)
		if err != nil {
			t.Fatal(err)
		}
		if ic.Seq != 1 || ic.Size != 4 {
			t.Fatalf("context seq %d size %d", ic.Seq, ic.Size)
		}
		c := commitment(subjectOf(t, h.Beef))
		if !ic.VerifyPolicy(c, o1.org.Policy) || ic.VerifyPolicy(c, o0.org.Policy) || ic.VerifyGrant(c, o1.org.Roll) {
			t.Fatal("the path verifies against the policy root of its seq, and no other root")
		}
	}
}

func TestSweepRetractsAPolicyCarrierAndTheNextTokenDropsIt(t *testing.T) {
	chain := newTestChain(80000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "policy sweep org", chain, orgOpts{})
	def := org.policyRecord("releaser", policyOpts{permissions: []string{"s/a/b"}})
	keep := org.policyRecord("auditor", policyOpts{permissions: []string{"s/read/b"}})
	for _, p := range []*policyMade{def, keep} {
		outs(t, s.submitTx(p.tx), 0)
	}
	o0 := org.orgToken(nil, nil, tokenOpts{define: [][32]byte{def.c, keep.c}})
	outs(t, s.submit(o0.beef), 0)
	if c := s.ls.Counts(); c.Policies != 2 || c.Wraps != 0 {
		t.Fatalf("%+v", c)
	}
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(def.tx, keep.tx), "policy")

	// The sweep is the fast half: the carrier is retracted at once, and a
	// reader learns it from the sweep class, not from the answer going
	// quiet. The policy classes answer every record the root holds,
	// whatever its status, so that a reader computing the root sees each
	// and checks it.
	sw := mintSweep(t, org.policyTree, []uint32{0}, org.party, borgrec.PolicyDerivation, chain)
	outs(t, s.submitTx(sw), 0)
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(def.tx, keep.tx), "policy after the sweep")
	eq(t, s.answer(qm{"hall": hall, "spent": idOf(org.policyTree) + ".0"}), ids(sw), "sweep")

	// The committed half: the organization's next token drops the
	// commitment, and only then does the record leave every answer.
	o1 := org.orgToken(nil, nil, tokenOpts{retire: [][32]byte{def.c}})
	outs(t, s.submit(o1.beef), 0)
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(keep.tx), "policy after the retirement")
	eq(t, s.answer(qm{"hall": hall, "org": org.hex(), "role": "releaser"}), nil, "role releaser")
}

func TestRetentionOfPolicyRecords(t *testing.T) {
	chain := newTestChain(80000)
	s := setup(t, engineOpts{chain: chain, now: 1_700_000_000})
	org := newOrgSim(t, hall, "policy retention org", chain, orgOpts{})
	kept := org.policyRecord("auditor", policyOpts{permissions: []string{"s/read/a"}})
	retired := org.policyRecord("releaser", policyOpts{permissions: []string{"s/a/b"}})
	swept := org.policyRecord("moderator", policyOpts{permissions: []string{"s/mod/a"}, vout: u32(5)})
	for _, p := range []*policyMade{kept, retired, swept} {
		outs(t, s.submitTx(p.tx), 0)
	}
	outs(t, s.submit(org.orgToken(nil, nil, tokenOpts{define: [][32]byte{kept.c, retired.c, swept.c}}).beef), 0)
	ctx := context.Background()
	if s.ls.Counts().Policies != 3 {
		t.Fatal("three policy records")
	}

	// Nothing goes while the set holds all three, however long the host
	// waits.
	s.clock.add(400 * day)
	if s.ls.Prune(ctx) != 0 || s.ls.Counts().Policies != 3 {
		t.Fatal("the set holds all three")
	}

	// One is retired by a token, one is retracted by a sweep while the set
	// still names it. The retired one goes once the floor has passed; the
	// retracted one is kept, because a reader computing the policy root
	// still needs every record the root holds, as a swept grant in the roll
	// is kept.
	outs(t, s.submit(org.orgToken(nil, nil, tokenOpts{retire: [][32]byte{retired.c}}).beef), 0)
	sw := mintSweep(t, org.policyTree, []uint32{5}, org.party, borgrec.PolicyDerivation, chain)
	outs(t, s.submitTx(sw), 0)
	if s.ls.Prune(ctx) != 0 || s.ls.Counts().Policies != 3 {
		t.Fatal("under the floor nothing goes")
	}
	s.clock.add(32 * day)
	if n := s.ls.Prune(ctx); n != 1 {
		t.Fatalf("%d went, want the retired one", n)
	}
	if s.ls.Counts().Policies != 2 || len(s.answer(qm{"hall": hall, "policy": org.hex()})) != 2 {
		t.Fatal("two left")
	}

	// Once the root drops the retracted one too, the floor runs again and
	// it goes; what the set still holds never does.
	outs(t, s.submit(org.orgToken(nil, nil, tokenOpts{retire: [][32]byte{swept.c}}).beef), 0)
	s.clock.add(32 * day)
	if n := s.ls.Prune(ctx); n != 1 {
		t.Fatalf("%d went, want the retracted one", n)
	}
	if s.ls.Counts().Policies != 1 {
		t.Fatal("one left")
	}
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(kept.tx), "policy")
}

func TestATokenIsCountedOnce(t *testing.T) {
	chain := newTestChain(80000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "counting org", chain, orgOpts{trees: 40})
	var grants []*grantMade
	for i := 0; i < 3; i++ {
		grants = append(grants, org.grant(newParty(fmt.Sprintf("counting member %d", i))))
	}
	for _, g := range grants {
		outs(t, s.submitTx(g.tx), 0)
	}
	outs(t, s.submit(org.orgToken(grantsOf(grants...), nil, tokenOpts{}).beef), 0)
	o1 := org.orgToken(nil, grantsOf(grants[0]), tokenOpts{})
	outs(t, s.submit(o1.beef), 0)
	counted := func() float64 {
		return s.host.count("borg_token_status_total", map[string]string{"status": "consistent"})
	}
	if counted() != 2 {
		t.Fatalf("counted %v", counted())
	}

	// Nine tokens at one seq: the bound keeps eight and forgets one, which
	// clears every status in the chain and decides them all again. A token
	// already counted is not counted a second time for the same status, so
	// the total is the tokens the host holds and no more.
	for i := 0; i < 9; i++ {
		n := uint64(i + 1)
		fork := org.orgToken(nil, nil, tokenOpts{mutate: func(r *borgrec.Org) { r.Created += n }})
		org.tip = o1
		s.submit(fork.beef)
	}
	// Eleven: the two, and each of the nine forks counted when it was
	// decided, the one the bound later turned away included.
	if counted() != 11 {
		t.Fatalf("counted %v consistent tokens", counted())
	}
}

func TestPolicyMismatchIsReportedAndSticky(t *testing.T) {
	chain := newTestChain(83000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "policy status org", chain, orgOpts{})
	def := org.policyRecord("reader", policyOpts{permissions: []string{"s/get/a"}})
	other := org.policyRecord("writer", policyOpts{permissions: []string{"s/put/a"}})
	for _, p := range []*policyMade{def, other} {
		outs(t, s.submitTx(p.tx), 0)
	}
	// seq 0 defines one record and says so; seq 1 defines the second but
	// commits to a root that is not the set's.
	o0 := org.orgToken(nil, nil, tokenOpts{define: [][32]byte{def.c}})
	outs(t, s.submit(o0.beef), 0)
	bad := org.orgToken(nil, nil, tokenOpts{define: [][32]byte{other.c}, mutate: func(r *borgrec.Org) { r.Policy = borgrec.PolicyRoot(nil) }})
	outs(t, s.submit(bad.beef), 0)
	after := org.orgToken(nil, nil, tokenOpts{})
	outs(t, s.submit(after.beef), 0)
	for _, w := range []struct {
		m    *minted
		want string
	}{{o0, borgrec.StatusConsistent}, {bad, borgrec.StatusPolicyMismatch}, {after, borgrec.StatusPolicyMismatch}} {
		if got := tokenStatus(t, s.ls, idOf(w.m.tx)); got != w.want {
			t.Fatalf("seq %d: %s, want %s", w.m.org.Seq, got, w.want)
		}
	}
	if s.host.count("borg_token_status_total", map[string]string{"status": "policy-mismatch"}) < 1 || !s.host.loggedWith("ls_borg token status", "status", "policy-mismatch") {
		t.Fatal("policy-mismatch is counted and logged")
	}
	// The host answers from the fold and reports the status; it does not
	// decide. A fold that broke on the root still holds what it applied
	// before the check, as a broken roll does, so both records are answered
	// and a reader that reads the status refuses the chain from there on.
	eq(t, s.answer(qm{"hall": hall, "policy": org.hex()}), byTxid(def.tx, other.tx), "policy")
}

func TestTheRollIsDecidedBeforeThePolicySet(t *testing.T) {
	chain := newTestChain(83000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "both broken org", chain, orgOpts{})
	def := org.policyRecord("reader", policyOpts{permissions: []string{"s/get/a"}})
	outs(t, s.submitTx(def.tx), 0)
	var nine [32]byte
	for i := range nine {
		nine[i] = 9
	}
	both := org.orgToken(nil, nil, tokenOpts{define: [][32]byte{def.c}, mutate: func(r *borgrec.Org) {
		r.Roll = borgrec.PolicyRoot([][32]byte{nine})
		r.Policy = borgrec.PolicyRoot(nil)
	}})
	outs(t, s.submit(both.beef), 0)
	if got := tokenStatus(t, s.ls, idOf(both.tx)); got != borgrec.StatusRollMismatch {
		t.Fatalf("%s", got)
	}
}

func TestEvidenceRefusedOnArrivalCostsNoJournalLine(t *testing.T) {
	chain := newTestChain(85500)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "journal bound org", chain, orgOpts{trees: 2})
	g := issueGrant(t, hall, org.party, newParty("journal bound subject"), org.grantTree, 0, defaultValues, true)
	rec := encode(t, g)
	var many []*transaction.Transaction
	for i := 0; i < 70; i++ {
		many = append(many, mintCarrier(t, org.grantTree, 0, org.party, borgrec.GrantDerivation, rec, lockTime0+uint32(i))) //nolint:gosec // test
	}
	// Offered lowest commitment first, every carrier past the bound is
	// beyond it the moment it arrives, so the row refuses it before
	// anything is written: no line in the journal, nothing to forget later.
	sorted := byC(many)
	for _, x := range sorted {
		s.submitTx(x)
	}
	c := s.ls.Counts()
	if got := status(t, s.ls, cOf(sorted[0])); got != borgrec.StatusHeld || c.Grants != 65 || c.Lines != 65 || c.Forgotten != 5 {
		t.Fatalf("%s %+v", got, c)
	}
	cl, dl := 0, 0
	for _, l := range s.journal.Lines {
		switch l.K {
		case "c":
			cl++
		case "d":
			dl++
		}
	}
	if cl != 65 || dl != 0 {
		t.Fatalf("%d carrier lines and %d drop lines", cl, dl)
	}
	// A fresh module over the same journal and storage reaches the same
	// row.
	again := s.restored(nil)
	if c := again.Counts(); c.Grants != 65 || c.Lines != 65 || c.Forgotten != 0 {
		t.Fatalf("after a restart %+v", c)
	}
}
