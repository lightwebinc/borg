package module

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chaintoken"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/replay"

	"github.com/lightwebinc/borg/borgrec"
)

// Ordered replay (replay.go) between two hosts over the module's own free
// questions: a host that knows an organization is levelled from a sibling,
// tokens on their parents in chain order, then the roll's grants, the
// policy set's records, the wraps of each group's epochs the host keeps,
// and the sweeps of its carriers; bounded calls reach the same; a first
// token needs a chain source; a second walk replays nothing; an epoch whose
// wraps have left every host by their floor is not asked for.

// peerOf asks an engine's lookup service as a peer's route answers: an
// error is the peer's refusal.
func peerOf(e *engine) bmodule.Ask {
	return func(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
		a, err := e.ls.Lookup(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", bmodule.ErrRefused, err)
		}
		return a, nil
	}
}

type walked struct {
	calls, gaps, questions int
	kinds                  map[string]int
	notes                  []string
}

// replayFrom runs whole walks of into from a peer until one is Done,
// submitting each answer's objects in order as the engine does and holding
// every token to its BEEF and its parent (the token and exactly its parent,
// the parent held or admitted earlier in the answer).
func replayFrom(t *testing.T, into, from *engine, maxQ, maxO int) walked {
	t.Helper()
	res := walked{kinds: map[string]int{}}
	var cursor any
	for {
		res.calls++
		if res.calls > 5000 {
			t.Fatal("the walk does not end")
		}
		a, err := into.ls.Replay(context.Background(), &bmodule.ReplayRequest{Topic: topic, Peer: "a", Ask: peerOf(from), Cursor: cursor,
			MaxQuestions: maxQ, MaxObjects: maxO,
			Applied: func(_ context.Context, txid chainhash.Hash) (bool, error) {
				return into.storage.isApplied(txid.String(), topic), nil
			}})
		if err != nil {
			t.Fatal(err)
		}
		if maxQ > 0 && a.Questions > maxQ || maxO > 0 && len(a.Objects) > maxO {
			t.Fatalf("a call asked %d and answered %d, bounds %d and %d", a.Questions, len(a.Objects), maxQ, maxO)
		}
		res.gaps += a.Gaps
		res.questions += a.Questions
		res.notes = append(res.notes, a.Notes...)
		admitted := map[chainhash.Hash]bool{}
		for _, o := range a.Objects {
			for _, dep := range o.After {
				if !admitted[dep] {
					t.Fatalf("%s %s submitted before %s, its After", o.Kind, o.Txid, dep)
				}
			}
			if o.Kind == replay.KindToken {
				wire, err := chaintoken.ReadWire(o.BEEF, 0)
				if err != nil {
					t.Fatal(err)
				}
				tx := wire.SubjectTx()
				parent, err := wire.Token(tx)
				if err != nil {
					t.Fatalf("token %s: %v", o.Txid, err)
				}
				tok, err := borgrec.ReadToken(tx)
				if err != nil {
					t.Fatal(err)
				}
				if tok.Seq() > 0 && !into.storage.isApplied(parent.Txid.String(), topic) && !admitted[parent.Txid] {
					t.Fatalf("token %s (seq %d) submitted before its parent is held", o.Txid, tok.Seq())
				}
			}
			if into.storage.isApplied(o.Txid.String(), topic) {
				t.Fatalf("%s %s answered though applied", o.Kind, o.Txid)
			}
			if _, err := into.admit(o.BEEF, topic); err == nil {
				admitted[o.Txid] = true
			} else {
				t.Fatalf("%s %s refused: %v", o.Kind, o.Txid, err)
			}
			res.kinds[o.Kind]++
		}
		if a.Done {
			return res
		}
		cursor = a.Cursor
	}
}

// treesOf is a chain view holding the given transactions.
type treesOf []*transaction.Transaction

func (ts treesOf) TxRaw(_ context.Context, txid string) ([]byte, error) {
	for _, tx := range ts {
		if tx.TxID().String() == txid {
			return tx.Bytes(), nil
		}
	}
	return nil, errors.New("not known")
}

func (ts treesOf) Proof(_ context.Context, txid string) (*transaction.MerklePath, uint32, error) {
	for _, tx := range ts {
		if tx.TxID().String() == txid {
			return tx.MerklePath, tx.MerklePath.BlockHeight, nil
		}
	}
	return nil, 0, errors.New("not mined")
}

// borgWorld is an organization in full on one host: three grants in the
// roll (one of them swept), two policy records in the set, a group with two
// epochs and their wraps, and a second group.
type borgWorld struct {
	chain     *testChain
	org       *orgSim
	g, h      *groupSim
	o0        *minted
	questions []qm
}

func buildWorld(t *testing.T, a *engine) *borgWorld {
	t.Helper()
	w := &borgWorld{chain: a.chain}
	org := newOrgSim(t, hall, "replay world org", a.chain, orgOpts{})
	w.org = org
	alice, carol, dave := newParty("world alice"), newParty("world carol"), newParty("world dave")
	ga, gc, gd := org.grant(alice), org.grant(carol), org.grant(dave)
	for _, g := range []*grantMade{ga, gc, gd} {
		outs(t, a.submitTx(g.tx), 0)
	}
	w.o0 = org.orgToken(grantsOf(ga, gc, gd), nil, tokenOpts{})
	outs(t, a.submit(w.o0.beef), 0)
	w.g = org.group("world group", []*party{alice, carol}, false)
	def := org.policyRecord("releaser", policyOpts{permissions: []string{"bsecret/release/*"}})
	bind := org.policyRecord("releaser", policyOpts{to: w.g.id[:]})
	for _, p := range []*policyMade{def, bind} {
		outs(t, a.submitTx(p.tx), 0)
	}
	outs(t, a.submit(org.orgToken(nil, nil, tokenOpts{define: [][32]byte{def.c, bind.c}}).beef), 0)
	outs(t, a.submit(w.g.tip.beef), 0)
	for _, x := range w.g.wraps(nil, 0) {
		outs(t, a.submitTx(x), 0)
	}
	outs(t, a.submit(w.g.change(changeOpts{rotate: true}).beef), 0)
	for _, x := range w.g.wraps(nil, 0) {
		outs(t, a.submitTx(x), 0)
	}
	w.h = org.group("world second", []*party{carol}, false)
	outs(t, a.submit(w.h.tip.beef), 0)
	for _, x := range w.h.wraps(nil, 0) {
		outs(t, a.submitTx(x), 0)
	}
	sw := mintSweep(t, org.grantTree, []uint32{gd.vout}, org.party, borgrec.GrantDerivation, a.chain)
	outs(t, a.submitTx(sw), 0)
	o, g, h := org.hex(), w.g.idHex(), w.h.idHex()
	w.questions = []qm{
		{"hall": hall, "org": o}, {"hall": hall, "org": o, "seq": "0"}, {"hall": hall, "org": o, "seq": "1"},
		{"hall": hall, "roll": o}, {"hall": hall, "org": o, "member": alice.hex}, {"hall": hall, "org": o, "member": carol.hex},
		{"hall": hall, "org": o, "member": dave.hex}, {"hall": hall, "policy": o}, {"hall": hall, "org": o, "role": "releaser"},
		{"hall": hall, "org": o, "boundTo": g}, {"hall": hall, "groupsOf": o}, {"hall": hall, "org": o, "memberOf": alice.hex},
		{"hall": hall, "org": o, "memberOf": carol.hex}, {"hall": hall, "org": o, "group": g}, {"hall": hall, "org": o, "group": g, "seq": "0"},
		{"hall": hall, "org": o, "group": g, "seq": "1"}, {"hall": hall, "org": o, "group": h},
		{"hall": hall, "org": o, "group": g, "epoch": "0"}, {"hall": hall, "org": o, "group": g, "epoch": "1"},
		{"hall": hall, "org": o, "group": h, "epoch": "0"}, {"hall": hall, "org": o, "group": g, "epoch": "1", "member": alice.hex},
		{"hall": hall, "spent": fmt.Sprintf("%s.%d", idOf(org.grantTree), gd.vout)},
		{"hall": hall, "history": o, "chain": "org"}, {"hall": hall, "history": o, "chain": g}, {"hall": hall, "history": o, "chain": h},
	}
	return w
}

// level is "" when b answers every question of the world as a does, holds
// the same unspent outputs and the same statuses; otherwise what differs.
func (w *borgWorld) level(t *testing.T, a, b *engine) string {
	t.Helper()
	var diffs []string
	for _, q := range w.questions {
		x, y := answerOf(t, a.ls, q), answerOf(t, b.ls, q)
		if strings.Join(x, ",") != strings.Join(y, ",") {
			diffs = append(diffs, fmt.Sprintf("%v: %v, want %v", q, y, x))
		}
	}
	ua, ub := utxoSet(a), utxoSet(b)
	if ua != ub {
		diffs = append(diffs, fmt.Sprintf("unspent outputs:\n%s\nwant\n%s", ub, ua))
	}
	return strings.Join(diffs, "\n")
}

func utxoSet(e *engine) string {
	var out []string
	for _, o := range e.storage.utxos(topic, false) {
		out = append(out, o.Outpoint.String())
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// knowing is a host that holds only the organization's first token, from
// which it knows the organization.
func (w *borgWorld) knowing(t *testing.T, trees bool) *engine {
	t.Helper()
	b := setup(t, engineOpts{chain: w.chain})
	outs(t, b.submit(w.o0.beef), 0)
	if trees {
		b.ls.trees = treesOf{w.org.chainT}
	}
	return b
}

func TestReplayLevelsAHost(t *testing.T) {
	a := setup(t, engineOpts{chain: newTestChain(96000)})
	w := buildWorld(t, a)
	b := w.knowing(t, true)
	if w.level(t, a, b) == "" {
		t.Fatal("host B is level before any replay")
	}
	res := replayFrom(t, b, a, 0, 0)
	if d := w.level(t, a, b); d != "" {
		t.Fatalf("after the walk:\n%s\nnotes %v", d, res.notes)
	}
	if res.kinds["token"] != 4 || res.kinds["grant"] != 3 || res.kinds["policy"] != 2 || res.kinds["wrap"] != 3 || res.kinds["sweep"] != 1 || res.gaps != 0 {
		t.Fatalf("replayed %v, gaps %d, notes %v", res.kinds, res.gaps, res.notes)
	}
	if again := replayFrom(t, b, a, 0, 0); len(again.kinds) != 0 {
		t.Fatalf("a second walk replayed %v", again.kinds)
	}
	if back := replayFrom(t, a, b, 0, 0); len(back.kinds) != 0 {
		t.Fatalf("A replayed %v from B", back.kinds)
	}
	// The statuses B decides are A's.
	for _, m := range append(append([]*minted{}, w.g.history...), w.h.history...) {
		if x, y := tokenStatus(t, a.ls, idOf(m.tx)), tokenStatus(t, b.ls, idOf(m.tx)); x != y || y != borgrec.StatusConsistent {
			t.Fatalf("%s: %s, want %s", idOf(m.tx), y, x)
		}
	}
}

// TestReplayBounded: two questions and one object a call, and the walks
// together level the host all the same.
func TestReplayBounded(t *testing.T) {
	a := setup(t, engineOpts{chain: newTestChain(96500)})
	w := buildWorld(t, a)
	b := w.knowing(t, true)
	res := replayFrom(t, b, a, 2, 1)
	if d := w.level(t, a, b); d != "" {
		t.Fatalf("after the bounded walk (%d calls): %s", res.calls, d)
	}
	if res.calls < 13 {
		t.Fatalf("%d calls for thirteen objects at one a call", res.calls)
	}
}

// TestReplayFirstToken: a group's first token needs its parent, the chain
// tree, which no host keeps: without a chain view each group is a gap and
// nothing of it is answered (its wraps are asked only of groups the host
// holds a token of); with one the hall is levelled.
func TestReplayFirstToken(t *testing.T) {
	a := setup(t, engineOpts{chain: newTestChain(97000)})
	w := buildWorld(t, a)
	b := w.knowing(t, false)
	res := replayFrom(t, b, a, 0, 0)
	if res.kinds["token"] != 1 || res.kinds["wrap"] != 0 || res.gaps < 2 {
		t.Fatalf("with no chain view: %v, gaps %d, notes %v", res.kinds, res.gaps, res.notes)
	}
	b.ls.trees = treesOf{w.org.chainT}
	res = replayFrom(t, b, a, 0, 0)
	if d := w.level(t, a, b); d != "" {
		t.Fatalf("with a chain view (%v, notes %v): %s", res.kinds, res.notes, d)
	}
}

// TestReplayAsksNoOldEpochWraps: the wraps of an epoch whose ending token
// the host first saw longer ago than the retention floor less a day have
// left every host by their floor, so a walk does not ask for them; the
// current epoch's it always asks for.
func TestReplayAsksNoOldEpochWraps(t *testing.T) {
	a := setup(t, engineOpts{chain: newTestChain(97500)})
	w := buildWorld(t, a)
	b := w.knowing(t, true)
	// B holds the first group's chain, its epoch 1 begun 30 days ago.
	for _, m := range w.g.history {
		outs(t, b.submit(m.beef), 0)
	}
	asked := func() (epochs map[string]int) {
		epochs = map[string]int{}
		ask := peerOf(a)
		var cursor any
		for {
			ans, err := b.ls.Replay(context.Background(), &bmodule.ReplayRequest{Topic: topic, Peer: "a", Cursor: cursor,
				Ask: func(ctx context.Context, q *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
					if s := string(q.Query); strings.Contains(s, `"epoch"`) && strings.Contains(s, w.g.idHex()) {
						for _, e := range []string{"0", "1"} {
							if strings.Contains(s, `"epoch":"`+e+`"`) {
								epochs[e]++
							}
						}
					}
					return ask(ctx, q)
				},
				Applied: func(_ context.Context, txid chainhash.Hash) (bool, error) {
					return b.storage.isApplied(txid.String(), topic), nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			if ans.Done {
				return epochs
			}
			cursor = ans.Cursor
		}
	}
	b.clock.add(30 * day)
	if got := asked(); got["0"] != 0 || got["1"] == 0 {
		t.Fatalf("past the floor less a day: epochs asked %v", got)
	}
	b2 := w.knowing(t, true)
	for _, m := range w.g.history {
		outs(t, b2.submit(m.beef), 0)
	}
	b2.clock.add(29 * day)
	b = b2
	if got := asked(); got["0"] == 0 || got["1"] == 0 {
		t.Fatalf("inside the floor less a day: epochs asked %v", got)
	}
}

// TestReplayRefusesAnotherTopic: a topic the module does not carry is not
// replayed, and nothing is replayed before the index is restored.
func TestReplayRefusesAnotherTopic(t *testing.T) {
	a := setup(t, engineOpts{chain: newTestChain(98800)})
	if _, err := a.ls.Replay(context.Background(), &bmodule.ReplayRequest{Topic: "tm_borg_another_abcdefghij", Ask: peerOf(a)}); !errors.Is(err, bmodule.ErrNoReplay) {
		t.Fatalf("%v", err)
	}
	ls, err := NewService(Options{Halls: []string{hall}, Host: newFakeHost(), Journal: &MemoryJournal{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Replay(context.Background(), &bmodule.ReplayRequest{Topic: topic, Ask: peerOf(a)}); err == nil {
		t.Fatal("a walk before the restore")
	}
}
