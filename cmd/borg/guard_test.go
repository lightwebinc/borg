package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/state"
)

// groupTip is the txid of a group's latest token, as a member reads it.
func (h *harness) groupTip(member, group string) string {
	h.t.Helper()
	m := regexpLatest.FindStringSubmatch(h.must(member, "open", group).stdout)
	if m == nil {
		h.t.Fatal("open printed no latest token")
	}
	return m[1]
}

// kindOf is the kind of a transaction's first output.
func kindOf(tx *transaction.Transaction) borgrec.Kind {
	if len(tx.Outputs) == 0 || tx.Outputs[0].LockingScript == nil {
		return borgrec.KindNone
	}
	return borgrec.Classify(*tx.Outputs[0].LockingScript)
}

// isSweep reports a sweep: a funding-shaped tombstone and at most a change
// output, where a funding tree has an output a carrier.
func isSweep(tx *transaction.Transaction) bool {
	return kindOf(tx) == borgrec.KindFunding && len(tx.Outputs) <= 2
}

// editState rewrites a home's state file as JSON.
func (h *harness) editState(name string, edit func(st map[string]any)) {
	h.t.Helper()
	path := filepath.Join(h.dir, name, state.File)
	raw, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	edit(st)
	out, _ := json.Marshal(st)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// namedGroup creates a group of the organisation under any name, as
// tooling that does not check the name would, and returns its id.
func (h *harness) namedGroup(name string, members ...string) string {
	h.t.Helper()
	ctx := context.Background()
	g, _, errb := h.global("org")
	r, err := g.openOrg(ctx, true)
	if err != nil {
		h.t.Fatalf("%v\n%s", err, errb)
	}
	defer r.close(ctx, g)
	var raw [][]byte
	for _, m := range members {
		b, _ := hex.DecodeString(m)
		raw = append(raw, b)
	}
	gr, err := r.eng.CreateGroup(ctx, name, raw, false, 0)
	if err != nil {
		h.t.Fatalf("%v\n%s", err, errb)
	}
	return gr.ID
}

// A persisted transition is given up only when the node names ANOTHER
// transaction as the spender of its predecessor or its fee coin. A leg
// that refuses it while the node shows the predecessor spent by this very
// transaction (it was taken before, and has not mined) changes nothing: it
// stays persisted, and the epoch key it sampled is not thrown away.
func TestATransitionIsKeptUnlessAnotherSpends(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	tip := h.groupTip("alice", o.group)
	h.mu.Lock()
	h.onSend = func(tx *transaction.Transaction) { h.chain.SpendElsewhere(tip, 0, tx.TxID().String()) }
	h.mu.Unlock()
	r := h.run("org", "", "epoch", "rotate", o.group)
	if r.code != exitUsage || strings.Contains(r.stderr, "lost") || !strings.Contains(r.stderr, "it is persisted; the next command sends it again") {
		t.Fatalf("a transition the leg refused while the node names it as the spender: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = h.must("org", "doctor")
	if !strings.Contains(r.stdout, "transition  ") || !strings.Contains(r.stdout, "persisted and not yet mined") {
		t.Fatalf("the transition was discarded:\n%s", r.stdout)
	}
}

// A spender of the predecessor that the node cannot show settles nothing:
// it may be this very transition under another txid (a re-encoded copy,
// which is the same transition). The transition is kept until the spender
// is read.
func TestASpenderTheNodeCannotShowDiscardsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	tip := h.groupTip("alice", o.group)
	other := strings.Repeat("ab", 32)
	h.mu.Lock()
	h.onSend = func(*transaction.Transaction) { h.chain.SpendElsewhere(tip, 0, other) }
	h.mu.Unlock()
	r := h.run("org", "", "epoch", "rotate", o.group)
	if r.code != exitUsage || strings.Contains(r.stderr, "lost") || !strings.Contains(r.stderr, "which the node could not show") {
		t.Fatalf("a spender the node cannot show: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := h.must("org", "doctor"); !strings.Contains(r.stdout, "transition  ") {
		t.Fatalf("the transition was discarded:\n%s", r.stdout)
	}
}

// A sweep is marked failed only when the node names another transaction
// as the spender of one of its inputs. A leg's refusal alone leaves it in
// flight: the next command sends the same bytes again.
func TestASweepIsKeptUnlessAnotherSpends(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	refused := false
	// The sweep is refused once by the leg, with every input unspent.
	h.chain.Refuse = func(tx *transaction.Transaction) string {
		if refused || !isSweep(tx) {
			return ""
		}
		refused = true
		return "txn-mempool-conflict"
	}
	r := h.run("org", "", "grant", "revoke", o.carol, "-by", "sweep")
	if !refused || r.code == exitOK || strings.Contains(r.stdout+r.stderr, "marked failed") || !strings.Contains(r.stderr, "it is persisted; the next command sends it again") {
		t.Fatalf("a sweep the leg refused once: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := h.must("org", "doctor"); !strings.Contains(r.stdout, "in flight") || strings.Contains(r.stdout, "FAILED") {
		t.Fatalf("the sweep is not in flight:\n%s", r.stdout)
	}
	// The next command finishes it: the same bytes, and carol is revoked.
	h.must("org", "grant", "revoke", o.carol, "-by", "sweep")
	h.want(h.run("carol", "", "member", o.carol), exitRefused, "revoked: its revocation outpoint is spent by")
}

// After a reorganisation a proof kept from before may name a block that is
// no longer in the best chain. A reader handed such a proof by a host, and
// an organisation holding one in its home, each take the transaction's
// current proof from the node and verify that against their own headers.
func TestStaleProofsAreReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	// The hosts answer every token and carrier under a proof of another
	// block.
	h.a.Stale, h.b.Stale = true, true
	if r := h.must("alice", "open", o.group); !strings.Contains(r.stdout, "epoch   0 OPENED") || strings.Contains(r.stderr, "REFUSED") {
		t.Fatalf("a member given stale proofs:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("bob", "member", o.bob); !strings.Contains(r.stdout, "CURRENT member") {
		t.Fatalf("a membership check given stale proofs:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("carol", "roll"); strings.Count(r.stdout, ": CURRENT") != 3 || strings.Contains(r.stderr, "REFUSED") {
		t.Fatalf("a roll given stale proofs:\n%s\n%s", r.stdout, r.stderr)
	}
	// With no chain source there is nothing to replace them with: nothing
	// verifies, and nothing is taken.
	delete(h.env, "BORG_ASSET")
	h.want(h.run("bob", "", "open", o.group), exitIncomplete, "no host answers this chain")
	h.env["BORG_ASSET"] = h.chainS.URL
	h.a.Stale, h.b.Stale = false, false

	// The organisation's home: the proofs it kept of its tokens and of its
	// funding trees name another block.
	shift := func(m map[string]any) {
		mp, err := transaction.NewMerklePathFromHex(m["bumpHex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		mp.BlockHeight += 3
		m["bumpHex"] = mp.Hex()
	}
	h.editState("org", func(st map[string]any) {
		org := st["org"].(map[string]any)
		for _, tok := range org["tokens"].([]any) {
			shift(tok.(map[string]any))
		}
		for _, gr := range org["groups"].([]any) {
			for _, tok := range gr.(map[string]any)["tokens"].([]any) {
				shift(tok.(map[string]any))
			}
		}
		for _, ts := range st["trees"].(map[string]any) {
			set := ts.(map[string]any)
			shift(set["tree"].(map[string]any))
			for _, tr := range set["trees"].([]any) {
				shift(tr.(map[string]any))
			}
		}
	})
	r := h.must("org", "org", "remove", o.bob, "-group", o.group)
	if !strings.Contains(r.stderr, "the node's current proof") || !strings.Contains(r.stdout, "epoch 1 (a new epoch") {
		t.Fatalf("a transition from a home with stale proofs:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("alice", "open", o.group); !strings.Contains(r.stdout, "epoch   1 OPENED") || strings.Contains(r.stderr, "DISAGREES") {
		t.Fatalf("after the transition:\n%s\n%s", r.stdout, r.stderr)
	}
	// The home is healed: the next transition says nothing of proofs.
	if r := h.must("org", "epoch", "rotate", o.group); strings.Contains(r.stderr, "the node's current proof") {
		t.Fatalf("the home kept its stale proofs:\n%s", r.stderr)
	}
}

// One question is paid for once, and a payment that left this process is
// never given back to the pool: a host that keeps answering 402 is handed
// one payment and no more, and a host that took a payment and did not
// answer leaves the payer's next payment on other coins, so both settle.
func TestOnePaymentAQuestionAndNoRefundOnceSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.must("bob", "fund", "-blocks", "102")
	h.startPaid(t, "payee", 7)
	// A host that asks again and again.
	h.paid.Again = true
	before := h.chain.Sent
	r := h.run("bob", "", "history", "-chain", o.group)
	h.want(r, exitRefused, "asked for a payment again after one was sent")
	if strings.Count(r.stderr, "was sent to") != 1 {
		t.Fatalf("a host that keeps answering 402:\n%s\n%s", r.stdout, r.stderr)
	}
	// Exactly one payment of this home's reached the settlement leg: the
	// one it made, kept as made.
	if n := h.chain.Sent - before; n != 1 {
		t.Fatalf("%d transactions were sent for one question", n)
	}
	h.paid.Again = false
	// A host that takes the payment and does not answer.
	h.paid.FailPaid = true
	r = h.run("bob", "", "history", "-chain", o.group)
	if r.code != exitUsage || !strings.Contains(r.stderr, "it left this process, so it is kept as made") || !strings.Contains(r.stderr, "status 500") {
		t.Fatalf("a host that took a payment and answered 500: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	h.paid.FailPaid = false
	// The next payment spends other coin, and the question is answered.
	r = h.must("bob", "history", "-chain", o.group)
	if !strings.Contains(r.stdout, "paid 7 sat to ") || !strings.Contains(r.stdout, "seq 0  ") {
		t.Fatalf("the history after:\n%s\n%s", r.stdout, r.stderr)
	}
	// Both payments the host recorded settle: neither was made on a coin
	// that another spent.
	ledger := filepath.Join(h.dir, "payments.jsonl")
	if err := os.WriteFile(ledger, h.paid.Ledger(), 0o600); err != nil {
		t.Fatal(err)
	}
	r = h.must("payee", "payee", "settle", ledger)
	if !strings.Contains(r.stdout, "2 payment(s) settled, 14 sat") || strings.Contains(r.stdout+r.stderr, "REFUSED") {
		t.Fatalf("payee settle:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A payment is recorded in the home before it is sent: when it reaches the
// host, the payer's state already holds it.
func TestAPaymentIsRecordedBeforeItIsSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.must("bob", "fund", "-blocks", "102")
	h.startPaid(t, "payee", 7)
	recorded := -1
	h.paid.OnPayment = func() {
		raw, err := os.ReadFile(filepath.Join(h.dir, "bob", state.File))
		if err != nil {
			recorded = 0
			return
		}
		var st struct {
			Payments []json.RawMessage `json:"payments"`
		}
		_ = json.Unmarshal(raw, &st)
		recorded = len(st.Payments)
	}
	if r := h.must("bob", "history", "-chain", o.group); !strings.Contains(r.stdout, "paid 7 sat") {
		t.Fatalf("history:\n%s\n%s", r.stdout, r.stderr)
	}
	if recorded != 1 {
		t.Fatalf("when the payment reached the host the payer's home recorded %d payment(s)", recorded)
	}
}

// What a command that asks several priced questions pays is bounded in
// all, whatever a host's pages and prices add up to.
func TestABudgetBoundsEveryPage(t *testing.T) {
	b := &budget{per: 20, total: 50}
	for i, want := range []uint64{20, 20, 10} {
		got, err := b.next()
		if err != nil || got != want {
			t.Fatalf("question %d may be paid %d, want %d: %v", i, got, want, err)
		}
		b.paid(got)
	}
	if _, err := b.next(); err == nil || !strings.Contains(err.Error(), "has paid 50 sat") {
		t.Fatalf("past the budget: %v", err)
	}
}

// history -all walks a chain page after page, one payment a page, and
// stops paying at its budget: the pages are the host's, their sum is the
// command's.
func TestHistoryWalksPagesWithinItsBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	h.must("org", "epoch", "rotate", o.group)
	h.must("org", "epoch", "rotate", o.group)
	h.identity("payee", false)
	h.must("carol", "fund", "-blocks", "102")
	h.startPaid(t, "payee", 7)
	// The host answers pages of 3 tokens: the group's 4 are two pages.
	h.a.HistoryPage = 3
	r := h.must("carol", "history", "-chain", o.group)
	if strings.Count(r.stdout, "paid 7 sat") != 1 || !strings.Contains(r.stdout, "seq 2  ") || strings.Contains(r.stdout, "seq 3  ") {
		t.Fatalf("one page:\n%s\n%s", r.stdout, r.stderr)
	}
	r = h.must("carol", "history", "-chain", o.group, "-all")
	if strings.Count(r.stdout, "paid 7 sat") != 2 || !strings.Contains(r.stdout, "seq 3  ") || !strings.Contains(r.stderr, "4 token(s)") {
		t.Fatalf("every page:\n%s\n%s", r.stdout, r.stderr)
	}
	// A budget of one page and a half pays one page, and says the chain
	// was not read to its end.
	paid := len(h.paid.Payments)
	r = h.run("carol", "", "history", "-chain", o.group, "-all", "-budget", "10")
	h.want(r, exitIncomplete, "the chain is not read to its end")
	if strings.Count(r.stdout, "paid 7 sat") != 1 || len(h.paid.Payments) != paid+1 {
		t.Fatalf("a budget of 10 at 7 a page: %d payment(s)\n%s\n%s", len(h.paid.Payments)-paid, r.stdout, r.stderr)
	}
	// A budget below the first page's price pays nothing.
	paid = len(h.paid.Payments)
	r = h.run("carol", "", "history", "-chain", o.group, "-budget", "5")
	if r.code == exitOK || strings.Contains(r.stdout, "paid ") || len(h.paid.Payments) != paid || !strings.Contains(r.stderr, "-budget") {
		t.Fatalf("a budget below the price: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

// On the plane a carrier counts as published only once the hosts named
// answer it by lookup: the facade's "nothing admitted" is a duplicate and a
// refusal alike. A wrap fewer hosts than the quorum hold stays in the
// outbox.
func TestAnUnconfirmedCarrierIsNotPublished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	wrap := func(tx *transaction.Transaction) bool { return kindOf(tx) == borgrec.KindWrap }
	h.a.Drop, h.b.Drop = wrap, wrap
	r := h.run("org", "", "epoch", "rotate", o.group)
	if r.code != exitUsage || !strings.Contains(r.stderr, "0 of 2 host(s) named answer it") || !strings.Contains(r.stderr, "it is persisted") {
		t.Fatalf("a wrap no host took: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := h.must("org", "doctor"); !strings.Contains(r.stdout, "outbox      wrap") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	// One host of two, with a quorum of all: still not published. With a
	// quorum of one it is.
	h.b.Drop = nil
	h.want(h.run("org", "", "org", "sync"), exitUsage, "1 of 2 host(s) named answer it and 2 must")
	h.env["BORG_QUORUM"] = "one"
	r = h.must("org", "org", "sync")
	if !strings.Contains(r.stderr, "publishing what a previous run persisted") {
		t.Fatalf("with a quorum of one:\n%s\n%s", r.stdout, r.stderr)
	}
	if r := h.must("org", "doctor"); strings.Contains(r.stdout, "outbox      ") {
		t.Fatalf("doctor after:\n%s", r.stdout)
	}
	h.a.Drop = nil
	if r := h.must("alice", "open", o.group); !strings.Contains(r.stdout, "epoch   1 OPENED") {
		t.Fatalf("alice:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A grant fewer hosts than the quorum answer once the roll admits it is
// not published either: it goes back to the outbox.
func TestAGrantNoHostHoldsIsNotPublished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	dave := h.identity("dave", false)
	grant := func(tx *transaction.Transaction) bool { return kindOf(tx) == borgrec.KindGrant }
	h.a.Drop, h.b.Drop = grant, grant
	r := h.run("org", "", "org", "add", dave, "-group", o.group)
	if r.code != exitUsage || !strings.Contains(r.stderr, "0 of 2 host(s) named answer it and 2 must") {
		t.Fatalf("a grant no host took: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := h.must("org", "doctor"); !strings.Contains(r.stdout, "outbox      grant") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	h.a.Drop, h.b.Drop = nil, nil
	h.must("org", "org", "sync")
	h.must("dave", "member", dave)
}

// Hosts that withhold a token in the middle of a chain cannot hide that
// later ones exist: the token they answer as the latest verifies and does
// not link, and a reader with no memory is told so (exit 3).
func TestWithheldTokensAreNotANote(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.must("org", "org", "remove", o.bob, "-group", o.group)
	h.must("org", "epoch", "rotate", o.group)
	withhold := func(q *borgrec.Question, outs []testOutput) []testOutput {
		if q.Class.Name == "group-seq" && q.Members["seq"] == "1" {
			return []testOutput{}
		}
		return outs
	}
	h.a.Rewrite, h.b.Rewrite = withhold, withhold
	// dave has no home of the group: nothing remembers a later state.
	h.identity("dave", false)
	r := h.run("dave", "", "group", "show", o.group)
	h.want(r, exitIncomplete, "WITHHELD: 1 verified token(s)")
	if !strings.Contains(r.stderr, "the highest at seq 2") {
		t.Fatalf("group show:\n%s\n%s", r.stdout, r.stderr)
	}
	// A member opens what the chain read reaches, and is told it may be an
	// older state: the command is incomplete.
	r = h.run("alice", "", "open", o.group)
	h.want(r, exitIncomplete, "exist above the chain read")
	if !strings.Contains(r.stdout, "epoch   0 OPENED") {
		t.Fatalf("open:\n%s\n%s", r.stdout, r.stderr)
	}
}

// An output of a tree that another instance of the tooling already swept
// does not make the kill switch's sweep of the rest fail for good: it is
// left out, and the transaction that spends it is taken as its sweep.
func TestKillPassesOverASpentOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	if err := copyDir(filepath.Join(h.dir, "org"), filepath.Join(h.dir, "org2")); err != nil {
		t.Fatal(err)
	}
	h.must("org2", "grant", "revoke", o.carol, "-by", "sweep")
	r := h.must("org", "kill", "-confirm", "KILL", "-yes")
	if !strings.Contains(r.stdout, "killed  ") || !strings.Contains(r.stderr, "already spent by") || !strings.Contains(r.stderr, "taken as their sweep") {
		t.Fatalf("kill with an output already swept:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, host := range []*testHost{h.a, h.b} {
		for _, id := range append(host.Txids("grant"), host.Txids("wrap")...) {
			if st := host.Status(id); st != borgrec.StatusRetracted {
				t.Fatalf("carrier %s is %s at a host after the kill", id, st)
			}
		}
	}
	if r := h.must("org", "kill", "-confirm", "KILL"); !strings.Contains(r.stdout, "0 tree(s) to sweep") {
		t.Fatalf("after the kill:\n%s", r.stdout)
	}
}

// Work an earlier command left unfinished does not stop the kill switch.
func TestKillIsNotBlockedByOldWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	// A group token one host never takes: it stays in the outbox.
	h.b.Drop = func(tx *transaction.Transaction) bool { return kindOf(tx) == borgrec.KindGroup }
	h.want(h.run("org", "", "epoch", "rotate", o.group), exitUsage, "it is persisted")
	r := h.must("org", "kill", "-confirm", "KILL", "-yes")
	if !strings.Contains(r.stdout, "killed  ") || !strings.Contains(r.stderr, "is left for the next command") {
		t.Fatalf("kill behind an object a host does not take:\n%s\n%s", r.stdout, r.stderr)
	}
	h.want(h.run("carol", "", "member", o.carol), exitRefused, "revoked")
}

// An object that cannot reach a host does not keep the rest of what the
// home owes from the hosts: a token that is mined and not yet published is
// published by the same run.
func TestStuckWorkDoesNotStopOtherWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.b.Drop = func(tx *transaction.Transaction) bool { return kindOf(tx) == borgrec.KindGroup }
	h.want(h.run("org", "", "epoch", "rotate", o.group), exitUsage, "it is persisted")
	// The organisation's latest token is mined, and no host holds it: the
	// home owes it.
	var tip string
	h.editState("org", func(st map[string]any) {
		toks := st["org"].(map[string]any)["tokens"].([]any)
		last := toks[len(toks)-1].(map[string]any)
		delete(last, "published")
		tip = last["txid"].(string)
	})
	h.a.Forget(tip)
	h.b.Forget(tip)
	r := h.run("org", "", "org", "sync")
	if r.code != exitUsage || !strings.Contains(r.stderr, "it is persisted and the next command publishes it") {
		t.Fatalf("the group token is still stuck: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !h.a.Holds(tip) || !h.b.Holds(tip) {
		t.Fatalf("the token the home owed was not published by the run whose earlier object was stuck:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A coin a run took from the pool and never recorded a spend of (the run
// stopped in between) is found by the next command and put back, when the
// node shows it unspent.
func TestACoinARunTookAndNeverSpentGoesBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	// The home as a run that stopped left it: the state records the pool
	// with a coin, and the pool's own file no longer holds it.
	statePath := filepath.Join(h.dir, "org", state.File)
	poolPath := filepath.Join(h.dir, "org", "wallet.json")
	var pool struct {
		Outputs []json.RawMessage `json:"outputs"`
	}
	raw, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var whole map[string]json.RawMessage
	if err := json.Unmarshal(raw, &whole); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &pool); err != nil || len(pool.Outputs) < 2 {
		t.Fatalf("the pool: %d coins, %v", len(pool.Outputs), err)
	}
	h.editState("org", func(st map[string]any) { st["taking"] = pool.Outputs })
	before := len(pool.Outputs)
	whole["outputs"], _ = json.Marshal(pool.Outputs[1:])
	out, _ := json.Marshal(whole)
	if err := os.WriteFile(poolPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stderr, "is back in the pool: a run stopped between taking it and recording what spent it") {
		t.Fatalf("the next command:\n%s\n%s", r.stdout, r.stderr)
	}
	raw, _ = os.ReadFile(poolPath)
	if err := json.Unmarshal(raw, &pool); err != nil || len(pool.Outputs) < before {
		t.Fatalf("the pool after: %d coins, %d before the run that stopped, %v", len(pool.Outputs), before, err)
	}
	// A command that ended leaves nothing recorded as being taken.
	raw, _ = os.ReadFile(statePath)
	if strings.Contains(string(raw), `"taking"`) {
		t.Fatal("the journal outlived the command")
	}
}

// The home's lock is taken before its wallet is opened: the coin pool is
// read whole and written back whole, so a pool read before the lock could
// put back coins another command spent. A command that finds the home in
// use says so before it has read anything of the wallet.
func TestTheLockComesBeforeTheWallet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.identity("payee", false)
	h.startPaid(t, "payee", 7)
	home := filepath.Join(h.dir, "org")
	unlock, err := state.Lock(filepath.Join(home, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	// A wallet file that does not read: a command that opened the wallet
	// before the lock would trip on it instead of on the lock.
	pool := filepath.Join(home, "wallet.json")
	raw, err := os.ReadFile(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pool, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	txid := strings.Repeat("ab", 32)
	for _, args := range [][]string{{"fund"}, {"fund", "-txid", txid}, {"hall", "new", "other"}, {"init"}, {"org", "sync"},
		{"kill", "-confirm", "KILL", "-yes"}, {"epoch", "rotate", o.group}, {"payee", "settle", pool}, {"group", "create", "v"},
		{"history"}, {"open", o.group}, {"org", "due", "-apply"}} {
		r := h.run("org", "", args...)
		if r.code != exitUsage || !strings.Contains(r.stderr, "another borg command is using the home") {
			t.Errorf("%v with the home locked: exit %d\n%s", args, r.code, r.stderr)
		}
	}
	if err := os.WriteFile(pool, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock()
	h.must("org", "epoch", "rotate", o.group)
}

// A name is any UTF-8, and the organisation chooses it: one with a line
// break in it writes no line of its own in a report, and one with a
// character a terminal acts on reaches no terminal. This tooling refuses
// to write such a name.
func TestAHostileNameForgesNoLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	h.want(h.run("org", "", "group", "create", "ops\nepoch   9 OPENED", "-member", o.alice), exitUsage, "a name is one line of plain text")
	h.want(h.run("org", "", "group", "create", "ops\x1b[31m", "-member", o.alice), exitUsage, "a name is one line of plain text")
	h.identity("other", true)
	h.want(h.run("other", "", "org", "create", "Example\r\norg     forged"), exitUsage, "a name is one line of plain text")
	// Another tooling would not: the organisation's key signs one all the
	// same.
	name := "ops\nepoch   9 OPENED: forged\r\nlatest  seq 9 ‮forged\u0085group   x"
	id := h.namedGroup(name, o.alice)
	for _, args := range [][]string{{"group", "show", id}, {"open", id}, {"member-of", o.alice}, {"group", "list"}} {
		r := h.run("alice", "", args...)
		for _, out := range []string{r.stdout, r.stderr} {
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "epoch   9 OPENED") || strings.HasPrefix(line, "latest  seq 9") || strings.ContainsAny(line, "\r‮\u0085") {
					t.Fatalf("%v: the name wrote a line of its own, or reached the terminal:\n%s\n%s", args, r.stdout, r.stderr)
				}
			}
		}
	}
	for _, args := range [][]string{{"doctor"}, {"org", "show"}, {"group", "list"}, {"org", "due"}} {
		r := h.run("org", "", args...)
		for _, line := range strings.Split(r.stdout+"\n"+r.stderr, "\n") {
			if strings.HasPrefix(line, "epoch   9 OPENED") || strings.HasPrefix(line, "latest  seq 9") || strings.ContainsAny(line, "\r‮\u0085") {
				t.Fatalf("%v: the name wrote a line of its own, or reached the terminal:\n%s\n%s", args, r.stdout, r.stderr)
			}
		}
	}
}

// What a node names as a spender is a transaction id or it is refused: it
// is never printed as the node wrote it.
func TestANodeSuppliedSpenderIsNotPrinted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	tip := h.groupTip("alice", o.group)
	evil := "\x1b[2J\x1b]0;owned\x07evil"
	// carol's revocation outpoint, and the group's latest token.
	var tree string
	var vout uint32
	h.editState("org", func(st map[string]any) {
		for _, g := range st["org"].(map[string]any)["grants"].([]any) {
			gr := g.(map[string]any)
			if gr["subject"] == o.carol {
				tree, vout = gr["tree"].(string), uint32(gr["vout"].(float64))
			}
		}
	})
	h.chain.SpendElsewhere(tree, vout, evil)
	h.chain.SpendElsewhere(tip, 0, evil)
	for _, args := range [][]string{{"member", o.carol}, {"roll"}, {"open", o.group, "-write"}} {
		r := h.run("carol", "", args...)
		if strings.ContainsAny(r.stdout+r.stderr, "\x1b\x07") || strings.Contains(r.stdout+r.stderr, "owned") {
			t.Errorf("%v printed what the node named as a spender: %q", args, r.stdout+r.stderr)
		}
	}
	// A spender that is no transaction id is no answer: carol is neither
	// current nor revoked on its word.
	h.want(h.run("carol", "", "member", o.carol), exitIncomplete, "no source could say")
	// The organisation's tooling prints nothing of it either.
	for _, args := range [][]string{{"epoch", "rotate", o.group}, {"grant", "revoke", o.carol, "-by", "sweep"}, {"kill", "-confirm", "KILL", "-yes"}} {
		r := h.run("org", "", args...)
		if strings.ContainsAny(r.stdout+r.stderr, "\x1b\x07") || strings.Contains(r.stdout+r.stderr, "owned") {
			t.Errorf("%v printed what the node named as a spender: %q", args, r.stdout+r.stderr)
		}
	}
}

// A note is filtered for the terminal whole: a long one loses nothing, and
// one that quotes what a node or a host wrote carries no control character.
func TestANoteIsFilteredAndWhole(t *testing.T) {
	long := strings.Repeat("a refusal with two transaction ids in it; ", 20) + "run the command again"
	if got := plain(long); got != long {
		t.Fatalf("a note of %d bytes was cut to %d", len(long), len(got))
	}
	if got := plain("spent by \x1b[2J\x1b]0;owned\x07evil\nnext"); strings.ContainsAny(got, "\x1b\x07") || !strings.HasSuffix(got, "\nnext") {
		t.Fatalf("%q", got)
	}
}
