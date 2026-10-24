package module

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"

	bmodule "github.com/lightwebinc/bcommon/module"

	"github.com/lightwebinc/borg/borgrec"
)

// Defects a review of a sibling host's index proved against it, each
// pinned here with the sequence that shows it in this one
// (hardening.test.ts): what a restart must not lose, what arrival order
// must not decide, what one topic must not take from another, and what
// must not wait in a queue.

// formulaTxids answers a question from the index alone, as the TypeScript
// service's lookup does, without hydrating it from storage.
func formulaTxids(t *testing.T, ls *Service, q qm) []string {
	t.Helper()
	c, err := ls.Classify("ls_borg", question(q).Query)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := ls.Answer(c, true)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Txid)
	}
	return out
}

// admitDirect tells ls of an admission as the engine does, with no store
// behind it.
func admitDirect(t *testing.T, ls *Service, topic string, tx *transaction.Transaction) {
	t.Helper()
	if err := ls.OutputAdmittedByTopic(context.Background(), &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx,
		Outpoint: transaction.Outpoint{Txid: *tx.TxID(), Index: 0}, Topic: topic, AtomicBEEF: atomicOf(t, tx)}); err != nil {
		t.Fatal(err)
	}
}

// equivocations are n wrap carriers on one funding output of the
// organization's record tree, lowest commitment first.
func equivocations(t *testing.T, org *orgSim, name string, n int) []*transaction.Transaction {
	t.Helper()
	_, many := equivocationsOf(t, org, name, n)
	return many
}

// equivocationsOf is equivocations with the group the wraps are of.
func equivocationsOf(t *testing.T, org *orgSim, name string, n int) (*groupSim, []*transaction.Transaction) {
	t.Helper()
	grp := org.group(name, []*party{newParty(name + " member")}, false)
	vout := org.nextRecordVout()
	ep := grp.epochs[0]
	var many []*transaction.Transaction
	for i := 0; i < n; i++ {
		rec := &borgrec.WrapRecord{Hall: org.hall, Org: org.party.id, Group: grp.id, Epoch: 0, EpochID: ep.epochID,
			Entries: [][]byte{wrapEntry(t, org.party, grp.members[0], ep.epochID, 0, ep.key)}}
		many = append(many, mintCarrier(t, org.recordTree, vout, org.party, borgrec.RecordDerivation, encode(t, rec), lockTime0))
	}
	return grp, byC(many)
}

func TestTableSweepRetractsAfterARestartThoughItsTombstoneWasSpent(t *testing.T) {
	chain := newTestChain(93000)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "sweep org", chain, orgOpts{})
	ga := org.grant(newParty("swept before it is published here"))
	op := fmt.Sprintf("%s.%d", idOf(org.grantTree), ga.vout)
	// The sweep arrives before the carrier it retracts, and waits in the
	// table.
	s1 := mintSweep(t, org.grantTree, []uint32{ga.vout}, org.party, borgrec.GrantDerivation, chain)
	outs(t, s.submitTx(s1), 0)
	if s.ls.Counts().Table != 1 {
		t.Fatal("the early spend waits in the table")
	}
	// The organization funds its next grant from the sweep's output:
	// storage now holds the tombstone as spent.
	gb := issueGrant(t, hall, org.party, newParty("funded by the tombstone"), s1, 0, defaultValues, true)
	b := mintCarrier(t, s1, 0, org.party, borgrec.GrantDerivation, encode(t, gb), lockTime0)
	outs(t, s.submitTx(b), 0)
	for _, o := range s.storage.utxos(topic, false) {
		if o.Outpoint.Txid.String() == idOf(s1) {
			t.Fatal("a spent tombstone is among the unspent rows a restore is handed")
		}
	}
	var w []Line
	for _, l := range s.journal.Lines {
		if l.K == "w" {
			w = append(w, l)
		}
	}
	if len(w) != 1 || w[0].Txid != idOf(s1) || w[0].Hall != hall {
		t.Fatalf("the journal names it, once: %+v", w)
	}
	again := s.restored(nil)
	if a, b := again.Counts(), s.ls.Counts(); a.Table != b.Table || a.Sweeps != b.Sweeps {
		t.Fatalf("the table is the same after a restart: %+v %+v", a, b)
	}
	eq(t, formulaTxids(t, again, qm{"hall": hall, "spent": op}), ids(s1), "sweep after a restart")
	// The retracted carrier arrives afterwards, at the live host and at the
	// restored one.
	outs(t, s.submitTx(ga.tx), 0)
	admitDirect(t, again, topic, ga.tx)
	if a, b := status(t, s.ls, cHex(ga.c)), status(t, again, cHex(ga.c)); a != borgrec.StatusRetracted || b != borgrec.StatusRetracted {
		t.Fatalf("%s %s", a, b)
	}
	// A second restart replays the same journal: nothing is named twice.
	s.restored(nil)
	n := 0
	for _, l := range s.journal.Lines {
		if l.K == "w" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d w lines", n)
	}
}

func TestAJournalWithoutSpentSweepLinesRestoresAsItDid(t *testing.T) {
	chain := newTestChain(93500)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "old journal org", chain, orgOpts{})
	ga := org.grant(newParty("old a"))
	outs(t, s.submitTx(ga.tx), 0)
	o0 := org.orgToken(grantsOf(ga), nil, tokenOpts{})
	outs(t, s.submit(o0.beef), 0)
	sw := mintSweep(t, org.grantTree, []uint32{ga.vout}, org.party, borgrec.GrantDerivation, chain)
	outs(t, s.submitTx(sw), 0)
	kinds := map[string]bool{}
	for _, l := range s.journal.Lines {
		kinds[l.K] = true
	}
	if strings.Join(sortedKeys(kinds), ",") != "c,s,t" {
		t.Fatalf("the line kinds every earlier journal holds: %v", kinds)
	}
	again := s.restored(nil)
	c := again.Counts()
	if status(t, again, cHex(ga.c)) != borgrec.StatusRetracted || tokenStatus(t, again, idOf(o0.tx)) != borgrec.StatusConsistent || c.Sweeps != 1 || c.Rows != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestATokenTurnedAwayWhilePendingIsReadAgain(t *testing.T) {
	chain := newTestChain(92000)
	s := setup(t, engineOpts{chain: chain})
	// The real token at seq 2 has to sort above eight others at its seq, and
	// a txid is as good as drawn at random. So the organization's chain is
	// minted again, on new trees, until that token's txid is in the upper
	// half of them all: a rogue then sorts below it at least every second
	// time.
	var org *orgSim
	var o0, o1, o2, o3 *minted
	for tries := 0; ; tries++ {
		if tries == 64 {
			t.Fatal("64 chains were minted and none has a seq 2 token whose txid is in the upper half")
		}
		org = newOrgSim(t, hall, "bound org", chain, orgOpts{})
		o0, o1, o2, o3 = org.orgToken(nil, nil, tokenOpts{}), org.orgToken(nil, nil, tokenOpts{}), org.orgToken(nil, nil, tokenOpts{}), org.orgToken(nil, nil, tokenOpts{})
		if idOf(o2.tx) >= "8" {
			break
		}
	}
	// The organization's own tokens at seq 2, each on a parent no host
	// admits and on a tree of its own, so there is always another to mint,
	// until eight have a txid below the real one's.
	type rogue struct {
		beef []byte
		txid string
	}
	var rogues []rogue
	below := func() int {
		n := 0
		for _, r := range rogues {
			if r.txid < idOf(o2.tx) {
				n++
			}
		}
		return n
	}
	for n := 0; below() < 8; n++ {
		if n >= 512 {
			t.Fatalf("512 rogue tokens were minted and fewer than eight have a txid below the real one's %s", idOf(o2.tx))
		}
		spare := fundingTree(t, org.party, borgrec.ChainDerivation, 1, chain)
		xr := *o1.org
		var p [32]byte
		copy(p[:], unhex(t, fmt.Sprintf("%064x", n+1)))
		xr.Prev = p
		x := mintToken(t, org.party, borgrec.TagOrg, encode(t, &xr), borgrec.OrgDerivation, mintIn{src: spare, vout: 0, d: borgrec.ChainDerivation})
		chain.mine(t, x)
		yr := *o2.org
		yr.Prev = commitment(x)
		y := mintToken(t, org.party, borgrec.TagOrg, encode(t, &yr), borgrec.OrgDerivation, mintIn{src: x, vout: 0, d: borgrec.OrgDerivation})
		chain.mine(t, y)
		rogues = append(rogues, rogue{tokenBEEF(t, y, x), idOf(y)})
	}
	for _, r := range rogues {
		outs(t, s.submit(r.beef), 0)
	}
	// A replay in any order: the last tokens first.
	outs(t, s.submit(o3.beef), 0)
	outs(t, s.submit(o2.beef), 0)
	if _, held := s.ls.TokenStatus(idOf(o2.tx)); held {
		t.Fatal("pending, and above eight lower txids: the seq holds no more")
	}
	outs(t, s.submit(o0.beef), 0)
	outs(t, s.submit(o1.beef), 0)
	// Its predecessor is decided now: the host reads it back from its own
	// storage, and the chain is whole.
	if a, b := tokenStatus(t, s.ls, idOf(o2.tx)), tokenStatus(t, s.ls, idOf(o3.tx)); a != borgrec.StatusConsistent || b != borgrec.StatusConsistent {
		t.Fatalf("%s %s", a, b)
	}
	q := qm{"hall": hall, "org": org.hex()}
	eq(t, s.answer(q), ids(o3.tx), "org")
	at2 := s.answer(with(q, "seq", "2"))
	if !contains(at2, idOf(o2.tx)) || len(at2) != 8 {
		t.Fatalf("seq 2: %v", at2)
	}
	// The same as a host given the chain in order, and as a restart.
	inOrder := setup(t, engineOpts{chain: chain})
	for _, r := range rogues {
		inOrder.submit(r.beef)
	}
	for _, m := range []*minted{o0, o1, o2, o3} {
		outs(t, inOrder.submit(m.beef), 0)
	}
	if tokenStatus(t, inOrder.ls, idOf(o3.tx)) != borgrec.StatusConsistent {
		t.Fatal("in order")
	}
	eq(t, inOrder.answer(with(q, "seq", "2")), at2, "seq 2 in order")
	again := s.restored(nil)
	eq(t, formulaTxids(t, again, q), ids(o3.tx), "org after a restart")
	eq(t, sortedCopy(formulaTxids(t, again, with(q, "seq", "2"))), sortedCopy(at2), "seq 2 after a restart")
	if tokenStatus(t, again, idOf(o3.tx)) != borgrec.StatusConsistent {
		t.Fatal("after a restart")
	}
}

func TestAMemberNamedProtoIsAMemberNoClassDefines(t *testing.T) {
	s := setup(t, engineOpts{})
	org := "03baf7689c0a3558fb604589036a8d1e4b685d909f6e0e2c6018a14049ae64ec26"
	q := `{"hall":"` + hall + `","org":"` + org + `","__proto__":"x"}`
	_, err := borgrec.ParseQuery([]byte(q))
	if borgrec.Reason(err) != "query" {
		t.Fatalf("refused for %q", borgrec.Reason(err))
	}
	if _, err := s.ls.Lookup(context.Background(), &lookup.LookupQuestion{Service: borgrec.LookupService, Query: []byte(q)}); err == nil || !strings.Contains(err.Error(), "no class has these members") {
		t.Fatalf("%v", err)
	}
	// The members a question does carry are its own, whatever they are
	// named.
	got, err := borgrec.ParseQuery([]byte(`{"hall":"` + hall + `","groupsOf":"` + org + `"}`))
	if err != nil || len(got.Members) != 2 || got.Members["groupsOf"] != org {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestASweepAdmittedInTwoHallsAnswersInBoth(t *testing.T) {
	vectorHall(t)
	chain := newTestChain(94000)
	m, err := Mount(newFakeHost(), baseConfig(hall, hall2), []string{topic, topic2}, &MemoryJournal{}, chain, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), nil, newMemStorage()); err != nil {
		t.Fatal(err)
	}
	org := newOrgSim(t, hall, "two hall org", chain, orgOpts{})
	a := org.grant(newParty("two hall a"))
	// The second hall's carrier on the same tree: the funding key does not
	// name a hall.
	vout := org.nextGrantVout()
	b := mintCarrier(t, org.grantTree, vout, org.party, borgrec.GrantDerivation,
		encode(t, issueGrant(t, hall2, org.party, newParty("two hall b"), org.grantTree, vout, defaultValues, true)), lockTime0)
	admitDirect(t, m.LS, topic, a.tx)
	admitDirect(t, m.LS, topic2, b)
	sw := mintSweep(t, org.grantTree, []uint32{a.vout, vout}, org.party, borgrec.GrantDerivation, chain)
	admitDirect(t, m.LS, topic, sw)
	admitDirect(t, m.LS, topic2, sw)
	tree := idOf(org.grantTree)
	asked := func(h string, v uint32) []string {
		return formulaTxids(t, m.LS, qm{"hall": h, "spent": fmt.Sprintf("%s.%d", tree, v)})
	}
	if status(t, m.LS, cHex(a.c)) != borgrec.StatusRetracted || status(t, m.LS, cOf(b)) != borgrec.StatusRetracted {
		t.Fatal("both retracted")
	}
	eq(t, asked(hall, a.vout), ids(sw), "the first hall")
	eq(t, asked(hall2, vout), ids(sw), "the hall whose carrier it retracts")
	if m.LS.Counts().Sweeps != 1 {
		t.Fatal("one sweep")
	}
	// Dropped from one topic, it still answers in the other.
	if err := m.LS.OutputNoLongerRetainedInHistory(context.Background(), &transaction.Outpoint{Txid: *sw.TxID(), Index: 0}, topic); err != nil {
		t.Fatal(err)
	}
	eq(t, asked(hall, a.vout), nil, "dropped from the first hall")
	eq(t, asked(hall2, vout), ids(sw), "still in the second")
	if m.LS.Counts().Sweeps != 1 {
		t.Fatal("still one sweep")
	}
}

func TestWhatTheIndexDropsIsDeletedAtOnce(t *testing.T) {
	chain := newTestChain(97500)
	s := setup(t, engineOpts{chain: chain})
	many := equivocations(t, newOrgSim(t, hall, "delete org", chain, orgOpts{}), "delete", 11)
	for _, x := range many {
		outs(t, s.submitTx(x), 0)
	}
	// Nine kept, two dropped, with no retention pass in between.
	stored := 0
	for _, o := range s.storage.utxos(topic, false) {
		for _, x := range many {
			if o.Outpoint.Txid.String() == idOf(x) {
				stored++
			}
		}
	}
	if c := s.ls.Counts(); c.Wraps != 9 || stored != 9 || c.PendingDeletes != 0 {
		t.Fatalf("%+v, %d stored", c, stored)
	}
	for _, x := range many[9:] {
		if s.storage.find(idOf(x), 0, topic) != nil {
			t.Fatal("a dropped carrier is still stored")
		}
	}
	// A store with no DeleteOutput keeps what was dropped; the module does
	// not keep a list of it.
	chain2 := newTestChain(98000)
	more := equivocations(t, newOrgSim(t, hall, "delete org b", chain2, orgOpts{}), "delete b", 31)
	host := newFakeHost()
	m, err := Mount(host, baseConfig(), []string{topic}, &MemoryJournal{}, chain2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), nil, cannotDelete{newMemStorage()}); err != nil {
		t.Fatal(err)
	}
	for _, x := range more {
		admitDirect(t, m.LS, topic, x)
	}
	m.LS.Prune(context.Background())
	if c := m.LS.Counts(); c.Wraps != 9 || c.PendingDeletes != 0 {
		t.Fatalf("%+v", c)
	}
	if host.logged("cannot delete dropped outputs") != 1 {
		t.Fatal("said once")
	}
}

func TestAnAdmissionDuringARestoreWaitsForIt(t *testing.T) {
	chain := newTestChain(98500)
	s := setup(t, engineOpts{chain: chain})
	org := newOrgSim(t, hall, "in flight org", chain, orgOpts{})
	many := equivocations(t, org, "in flight", 10)
	for _, x := range many[1:] {
		outs(t, s.submitTx(x), 0)
	}
	// A chain of two tokens, and a journal that does not name them: the
	// restore walks back from the second to the first.
	o0, o1 := org.orgToken(nil, nil, tokenOpts{}), org.orgToken(nil, nil, tokenOpts{})
	for _, m := range []*minted{o0, o1} {
		outs(t, s.submit(m.beef), 0)
	}
	journal := &MemoryJournal{}
	for _, l := range s.journal.Lines {
		if l.K != "t" {
			journal.Lines = append(journal.Lines, l)
		}
	}
	m, err := Mount(newFakeHost(), baseConfig(), []string{topic}, journal, chain, nil)
	if err != nil {
		t.Fatal(err)
	}
	ls := m.LS
	// While it reads the first token, the lowest commitment of all is
	// admitted on the full row.
	done := make(chan struct{})
	started, seenDuring := false, false
	s.storage.meanwhileTxid = idOf(o0.tx)
	s.storage.meanwhile = func() {
		started = true
		go func() {
			admitDirect(t, ls, topic, many[0])
			close(done)
		}()
		time.Sleep(20 * time.Millisecond)
		select {
		case <-done:
			seenDuring = true
		default:
		}
	}
	if _, err := ls.Restore(context.Background(), s.storage.utxos(topic, true), s.storage); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("the admission came during the restore")
	}
	if seenDuring {
		t.Fatal("indexed while the restore was in flight")
	}
	<-done
	if status(t, ls, cOf(many[0])) != borgrec.StatusHeld || status(t, ls, cOf(many[1])) != borgrec.StatusSuperseded {
		t.Fatal("the lowest ever admitted is the winner")
	}
	if _, ok := ls.Status(cOf(many[9])); ok {
		t.Fatal("the row keeps eight superseded: the highest is not kept")
	}
	if c := ls.Counts(); c.Wraps != 9 || c.Lines != 9 {
		t.Fatalf("%+v", c)
	}
	if tokenStatus(t, ls, idOf(o0.tx)) != borgrec.StatusConsistent || tokenStatus(t, ls, idOf(o1.tx)) != borgrec.StatusConsistent {
		t.Fatal("both tokens consistent")
	}
}
