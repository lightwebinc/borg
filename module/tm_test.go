package module

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/headers"

	"github.com/lightwebinc/borg/borgrec"
)

// tm_borg on its own (tm_borg.test.ts): every golden transaction admitted
// or refused through the topic manager exactly as the vectors say, every
// refusal record of refusal-v1.json refused inside a real carrier or token
// for its reason, a BEEF over the host's bound, a sweep over the host's
// input bound, a header source that cannot answer, and every refusal label
// a host counts shown to be reachable.

var reasonsSeen = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

type verdictOf struct {
	outputs, retain []uint32
	reason          string
}

func verdict(t *testing.T, tm *TopicManager, beef []byte, held []int) verdictOf {
	t.Helper()
	ai, err := tm.Admit(context.Background(), beef, held)
	if err != nil {
		r, ok := err.(*Refused)
		if !ok {
			t.Fatalf("not a refusal: %v", err)
		}
		reasonsSeen.Lock()
		reasonsSeen.m[r.Reason] = true
		reasonsSeen.Unlock()
		return verdictOf{reason: r.Reason}
	}
	return verdictOf{outputs: ai.OutputsToAdmit, retain: ai.CoinsToRetain}
}

func TestTransactionVectorsThroughTheManager(t *testing.T) {
	v := loadTxVectors(t)
	host := newFakeHost()
	chain := newTestChain(1).withVectors(t)
	managers := map[int]*TopicManager{}
	admitted, refused := 0, 0
	for i := range v.Cases {
		x := &v.Cases[i]
		bound := v.MaxSweepInputs
		if x.MaxSweepInputs != 0 {
			bound = x.MaxSweepInputs
		}
		tm := managers[bound]
		if tm == nil {
			var err error
			if tm, err = NewTopicManager(v.Topic, host, chain, v.MaxBeef, bound); err != nil {
				t.Fatal(err)
			}
			managers[bound] = tm
		}
		got := verdict(t, tm, x.beef(t), x.held())
		if x.Verdict == "admit" {
			if got.reason != "" || fmt.Sprint(got.outputs) != fmt.Sprint(x.Admits.OutputsToAdmit) || fmt.Sprint(got.retain) != fmt.Sprint(x.Admits.CoinsToRetain) {
				t.Fatalf("%s: got %+v, want admit %v retain %v", x.Name, got, x.Admits.OutputsToAdmit, x.Admits.CoinsToRetain)
			}
			admitted++
		} else {
			if got.reason != x.Reason {
				t.Fatalf("%s: refused %q, want %q", x.Name, got.reason, x.Reason)
			}
			refused++
		}
	}
	if admitted+refused != 100 || len(v.Cases) != 100 {
		t.Fatalf("%d admitted and %d refused of %d", admitted, refused, len(v.Cases))
	}
	// A txid admitted before in another BEEF is counted once, and once as a
	// repeat. The sweep at its own bound is decided by a second manager,
	// which counts it apart.
	for _, w := range []struct {
		kind              string
		distinct, repeats float64
	}{{"grant", 4, 1}, {"wrap", 2, 0}, {"org-token", 2, 1}, {"group-token", 3, 1}, {"sweep", 3, 0}} {
		if got := host.count("borg_admitted_total", map[string]string{"kind": w.kind}); got != w.distinct {
			t.Fatalf("borg_admitted_total %s %v, want %v", w.kind, got, w.distinct)
		}
		if got := host.count("borg_admitted_repeats_total", map[string]string{"kind": w.kind}); got != w.repeats {
			t.Fatalf("borg_admitted_repeats_total %s %v, want %v", w.kind, got, w.repeats)
		}
	}
	if got := host.count("borg_refused_total", map[string]string{"reason": "beef"}); got != 16 {
		t.Fatalf("borg_refused_total beef %v, want 16", got)
	}
}

// vectorOrg is the vectors' organization: 32 bytes of 0x51, a public test
// key.
func vectorOrg() *party {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x51
	}
	return partyFromKey("vector org", k)
}

func TestEveryRefusalRecordInAValidCarrierOrToken(t *testing.T) {
	v := loadTxVectors(t)
	chain := newTestChain(60000).withVectors(t)
	tm, err := NewTopicManager(v.Topic, newFakeHost(), chain, borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	if err != nil {
		t.Fatal(err)
	}
	vs := loadRefusalVectors(t)
	org := vectorOrg()
	n := len(vs)
	grants := fundingTree(t, org, borgrec.GrantDerivation, n, chain)
	records := fundingTree(t, org, borgrec.RecordDerivation, n, chain)
	policies := fundingTree(t, org, borgrec.PolicyDerivation, n, chain)
	chains := fundingTree(t, org, borgrec.ChainDerivation, n, chain)
	for i, r := range vs {
		rec := unhex(t, r.Hex)
		vout := uint32(i) //nolint:gosec // test
		var beef []byte
		switch r.Record {
		case "grant":
			beef = atomicOf(t, mintCarrier(t, grants, vout, org, borgrec.GrantDerivation, rec, carrier.LockTime))
		case "wrap":
			beef = atomicOf(t, mintCarrier(t, records, vout, org, borgrec.RecordDerivation, rec, carrier.LockTime))
		case "policy":
			beef = atomicOf(t, mintCarrier(t, policies, vout, org, borgrec.PolicyDerivation, rec, carrier.LockTime))
		default:
			tag, d := borgrec.TagOrg, borgrec.OrgDerivation
			if r.Record == "group" {
				tag, d = borgrec.TagGroup, borgrec.GroupDerivation
			}
			tk := mintToken(t, org, tag, rec, d, mintIn{src: chains, vout: vout, d: borgrec.ChainDerivation})
			chain.mine(t, tk)
			beef = tokenBEEF(t, tk, chains)
		}
		// A content-stage grant names a stand-in revocation outpoint no
		// transaction spends, so a carrier refuses it at the first content
		// rule; the transaction vectors carry grant-signature and
		// grant-reveal refusals on the outpoints their grants name.
		want := r.Reason
		if r.Stage == "content" {
			want = "grant-revocation"
		}
		if got := verdict(t, tm, beef, nil).reason; got != want {
			t.Fatalf("%s: %s: refused %q, want %q", r.Record, r.Label, got, want)
		}
	}
}

func TestBEEFOverTheBound(t *testing.T) {
	v := loadTxVectors(t)
	chain := newTestChain(61000)
	org := newOrgSim(t, "example_hall_vkqzmtrwxp", "big tree owner", chain, orgOpts{trees: 1})
	// 6000 funding outputs of 49 bytes and more: the carrier's BEEF is over
	// 256 KiB.
	tree := fundingTree(t, org.party, borgrec.GrantDerivation, 6000, chain)
	g := org.grantWith(newParty("subject"), defaultValues, 0, carrier.LockTime)
	beef := atomicOf(t, mintCarrier(t, tree, 0, org.party, borgrec.GrantDerivation, encode(t, g.grant), carrier.LockTime))
	if len(beef) <= borgrec.DefaultMaxBEEF {
		t.Fatalf("a BEEF of %d bytes", len(beef))
	}
	tm, _ := NewTopicManager(v.Topic, newFakeHost(), chain, borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	if got := verdict(t, tm, beef, nil).reason; got != "beef" {
		t.Fatalf("over the bound: %q", got)
	}
	if got := verdict(t, tm, []byte{1, 2, 3}, nil).reason; got != "beef" {
		t.Fatalf("no BEEF: %q", got)
	}
}

func TestSweepOverTheInputBoundAndAPlainPayment(t *testing.T) {
	v := loadTxVectors(t)
	chain := newTestChain(62000)
	owner := newParty("sweeper")
	tree := fundingTree(t, owner, borgrec.RecordDerivation, 1025, chain)
	all := make([]uint32, 1025)
	for i := range all {
		all[i] = uint32(i) //nolint:gosec // test
	}
	big := mintSweep(t, tree, all, owner, borgrec.RecordDerivation, chain)
	tm, _ := NewTopicManager(v.Topic, newFakeHost(), chain, borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	if got := verdict(t, tm, atomicOf(t, big), nil).reason; got != "too-large" {
		t.Fatalf("a sweep of 1025 inputs: %q", got)
	}
	payer := newParty("payer")
	paddr, _ := script.NewAddressFromPublicKey(payer.key.PubKey(), false)
	plock, _ := p2pkh.Lock(paddr)
	coin := chain.mine(t, fromNowhere(&transaction.TransactionOutput{Satoshis: 5000, LockingScript: plock}))
	plain := transaction.NewTransaction()
	unlock, _ := p2pkh.Unlock(payer.key, nil)
	plain.AddInputFromTx(coin, 0, unlock)
	oaddr, _ := script.NewAddressFromPublicKey(owner.key.PubKey(), false)
	olock, _ := p2pkh.Lock(oaddr)
	plain.AddOutput(&transaction.TransactionOutput{Satoshis: 4000, LockingScript: olock})
	if err := plain.Sign(); err != nil {
		t.Fatal(err)
	}
	if got := verdict(t, tm, atomicOf(t, plain), nil).reason; got != "not-borg" {
		t.Fatalf("a plain payment: %q", got)
	}
}

func TestHeaderSourceDownRefusesUnmined(t *testing.T) {
	v := loadTxVectors(t)
	host := newFakeHost()
	tm, _ := NewTopicManager(v.Topic, host, headers.New("http://127.0.0.1:9"), borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	x := v.byName(t, "org-token-genesis")
	if got := verdict(t, tm, x.beef(t), x.held()).reason; got != "unmined" {
		t.Fatalf("headers down: %q", got)
	}
	if host.count("borg_refused_total", map[string]string{"reason": "unmined"}) != 1 {
		t.Fatal("the refusal is counted")
	}
	ok, _ := NewTopicManager(v.Topic, host, newTestChain(1).withVectors(t), borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	if got := verdict(t, ok, x.beef(t), x.held()); got.reason != "" || fmt.Sprint(got.outputs) != "[0]" || len(got.retain) != 0 {
		t.Fatalf("headers up: %+v", got)
	}
}

func TestEveryRefusalLabelIsReachable(t *testing.T) {
	// The tests above reach every label; when this one runs alone, they run
	// first.
	reasonsSeen.Lock()
	alone := len(reasonsSeen.m) == 0
	reasonsSeen.Unlock()
	if alone {
		TestTransactionVectorsThroughTheManager(t)
		TestEveryRefusalRecordInAValidCarrierOrToken(t)
		TestBEEFOverTheBound(t)
		TestSweepOverTheInputBoundAndAPlainPayment(t)
		TestHeaderSourceDownRefusesUnmined(t)
	}
	reasonsSeen.Lock()
	defer reasonsSeen.Unlock()
	var missing []string
	for _, r := range Reasons {
		if !reasonsSeen.m[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("labels no test reaches: %v", missing)
	}
}

func TestManagerDocumentsItselfAndRefusesAnotherTopic(t *testing.T) {
	v := loadTxVectors(t)
	chain := newTestChain(1).withVectors(t)
	tm, err := NewTopicManager(v.Topic, newFakeHost(), chain, borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Hall != v.Hall {
		t.Fatalf("hall %s", tm.Hall)
	}
	if !strings.Contains(tm.GetDocumentation(), "raised") {
		t.Fatal("the documentation says every refusal is raised")
	}
	if md := tm.GetMetaData(); md.Name != v.Topic || md.Description != "Admits the borg hall "+v.Hall+"." {
		t.Fatalf("metadata %+v", md)
	}
	for _, bad := range []string{"tm_other_x_abcdefghij", "tm_borg_Bad_abcdefghij"} {
		if _, err := NewTopicManager(bad, newFakeHost(), chain, borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, err := NewTopicManager(v.Topic, newFakeHost(), chain, 0, borgrec.DefaultMaxSweepInputs); err == nil {
		t.Fatal("a BEEF bound of 0 accepted")
	}
	// The manager decides on the submission's bytes, which the host puts in
	// the context.
	if _, err := tm.IdentifyAdmissibleOutputs(context.Background(), nil, nil, nil); err != ErrNoSubmission {
		t.Fatalf("no submission bytes: %v", err)
	}
}

func TestAnAdmissionIsCountedOncePerTransaction(t *testing.T) {
	v := loadTxVectors(t)
	host := newFakeHost()
	tm, _ := NewTopicManager(v.Topic, host, newTestChain(1).withVectors(t), borgrec.DefaultMaxBEEF, borgrec.DefaultMaxSweepInputs)
	s := v.byName(t, "sweep")
	for i := 0; i < 3; i++ {
		verdict(t, tm, s.beef(t), s.held())
	}
	if host.count("borg_admitted_total", map[string]string{"kind": "sweep"}) != 1 || host.count("borg_admitted_repeats_total", map[string]string{"kind": "sweep"}) != 2 {
		t.Fatal("counted once, and twice as a repeat")
	}
}
