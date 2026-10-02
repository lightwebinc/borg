package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/mint"

	"github.com/lightwebinc/borg/internal/engine"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
)

// A run that stops is a home as its files stood at that moment. These
// tests copy the home at the moment a funding tree reaches the settlement
// leg, after the home recorded it and before anything else, and go on in
// the copy: the home the next command finds.

// stopAt copies the home name to the home as when the next funding tree
// the home recorded reaches the settlement leg, and returns that tree once
// run has returned. land says whether the chain then takes the tree; either
// way the run that sent it hears a refusal, nothing it sends afterwards
// reaches the chain, and its own home is left behind.
func (h *harness) stopAt(name, as string, land bool, run func()) *transaction.Transaction {
	h.t.Helper()
	var sent *transaction.Transaction
	var failed error
	h.mu.Lock()
	h.refuse = func(tx *transaction.Transaction) bool {
		if sent != nil {
			return true
		}
		recorded := false
		for _, p := range h.trees(name).PendingTrees {
			recorded = recorded || p.Tree.Txid == tx.TxID().String()
		}
		if !recorded {
			return false
		}
		sent = tx
		failed = stoppedHome(filepath.Join(h.dir, name), filepath.Join(h.dir, as))
		if land && failed == nil {
			failed = h.chain.Send(tx)
		}
		return true
	}
	h.mu.Unlock()
	run()
	h.mu.Lock()
	h.refuse = nil
	h.mu.Unlock()
	if sent == nil || failed != nil {
		h.t.Fatalf("the run sent %v to the settlement leg: %v", sent, failed)
	}
	return sent
}

// stoppedHome copies a home a command is running in: the lock and the
// temporary files of a save under way are left out.
func stoppedHome(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if d.Name() == "lock" || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
}

// treeRecords is what a home's state says of its funding trees.
type treeRecords struct {
	Trees map[string]struct {
		Tree *struct {
			Txid string `json:"txid"`
		} `json:"tree"`
		Ahead []struct {
			Txid string `json:"txid"`
		} `json:"ahead"`
	} `json:"trees"`
	PendingTrees []struct {
		Kind string `json:"kind"`
		Tree struct {
			Txid string `json:"txid"`
		} `json:"tree"`
		Coin struct {
			TxID string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"coin"`
		ReturnedOnce bool `json:"returned_once"`
	} `json:"pendingTrees"`
	Org *struct {
		Groups []struct {
			ID string `json:"id"`
		} `json:"groups"`
	} `json:"org"`
}

// coin is the outpoint of the first record's coin.
func (r treeRecords) coin() string {
	return fmt.Sprintf("%s.%d", r.PendingTrees[0].Coin.TxID, r.PendingTrees[0].Coin.Vout)
}

// current is the txid of the current tree of kind, or "".
func (r treeRecords) current(kind string) string {
	if ts, ok := r.Trees[kind]; ok && ts.Tree != nil {
		return ts.Tree.Txid
	}
	return ""
}

// trees reads a home's state. It is read from the chain's handler too, so
// it reports nothing through the test.
func (h *harness) trees(name string) treeRecords {
	var st treeRecords
	if raw, err := os.ReadFile(filepath.Join(h.dir, name, state.File)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	return st
}

// poolCoins are the outpoints a home's pool holds, as txid.vout.
func (h *harness) poolCoins(name string) map[string]bool {
	h.t.Helper()
	g, _, _ := h.global(name)
	e, err := g.openWallet()
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, o := range e.Pool.Outputs() {
		out[o.Outpoint()] = true
	}
	return out
}

// start runs what every command that publishes runs first, and nothing
// else: the trees are settled, then whatever an earlier run owed is
// finished. It returns what it wrote to standard error.
func (h *harness) start(name string) string {
	h.t.Helper()
	g, _, errb := h.global(name)
	hm, err := g.openHome()
	if err != nil {
		h.t.Fatal(err)
	}
	defer hm.close()
	eng, err := g.engine(context.Background(), hm)
	if err != nil {
		h.t.Fatalf("start: %v\n%s", err, errb.String())
	}
	if err := eng.Close(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return errb.String()
}

// settle settles the home's tree records and does nothing else: a command
// that neither mints nor finishes earlier work.
func (h *harness) settle(name string) string {
	h.t.Helper()
	g, _, errb := h.global(name)
	hm, err := g.openHome()
	if err != nil {
		h.t.Fatal(err)
	}
	defer hm.close()
	legs, err := g.legs()
	if err != nil {
		h.t.Fatal(err)
	}
	eng, err := engine.New(hm.st, hm.e.Signer(), hm.e.Pool, legs, engine.Options{TreeCount: 8, ChainTreeCount: 8,
		TreeSats: limits.DefaultTreeSats, Fees: mint.DefaultFees, Poll: poll, Wait: time.Minute})
	if err != nil {
		h.t.Fatal(err)
	}
	eng.Note = g.say
	if err := eng.RecoverTrees(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return errb.String()
}

// orgOnly is an organisation with three members and no group yet: the next
// group mints the first record tree.
func (h *harness) orgOnly() org {
	h.t.Helper()
	var o org
	o.key = h.identity("org", true)
	o.alice = h.identity("alice", false)
	o.bob = h.identity("bob", false)
	o.carol = h.identity("carol", false)
	h.newHall("org")
	h.must("org", "org", "create", "Example Organisation", "-member", o.alice, "-member", o.bob, "-member", o.carol)
	h.env["BORG_ORG"] = o.key
	return o
}

// stoppedGroup is a group create that stopped when its record tree reached
// the settlement leg: the group's token is mined, its wraps are owed, and
// the tree is recorded and not adopted. It returns the tree and the group.
func (h *harness) stoppedGroup(o org, land bool) (*transaction.Transaction, string) {
	h.t.Helper()
	tree := h.stopAt("org", "stopped", land, func() {
		if r := h.run("org", "", "group", "create", "engineering", "-member", o.alice, "-member", o.bob, "-member", o.carol); r.code == 0 {
			h.t.Fatalf("a group create whose record tree the leg refused:\n%s\n%s", r.stdout, r.stderr)
		}
	})
	st := h.trees("stopped")
	if st.current(state.TreeRecord) != "" || len(st.PendingTrees) != 1 || st.PendingTrees[0].Kind != state.TreeRecord ||
		st.PendingTrees[0].Tree.Txid != tree.TxID().String() || st.Org == nil || len(st.Org.Groups) != 1 {
		h.t.Fatalf("the home the run left: %+v", st)
	}
	return tree, st.Org.Groups[0].ID
}

// A run that stops after it signed a tree and before the settlement leg
// took it loses nothing: the next command finds the record, the node knows
// no such tree and shows the coin unspent, and the coin is back in the
// pool before anything is minted.
func TestARunStoppedBeforeTheTreeWasSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgOnly()
	before := h.poolCoins("org")
	_, group := h.stoppedGroup(o, false)
	coin := h.trees("stopped").coin()
	if !before[coin] || h.poolCoins("stopped")[coin] {
		t.Fatalf("the coin %s: in the pool before %v, after the run stopped %v", coin, before[coin], h.poolCoins("stopped")[coin])
	}
	if r := h.must("stopped", "doctor"); !strings.Contains(r.stdout, "signed for coin "+coin+" and not recorded as minted") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	errs := h.start("stopped")
	if !strings.Contains(errs, "never reached the chain: its fee coin "+coin+" is unspent and back in the pool") {
		t.Fatalf("the next command:\n%s", errs)
	}
	// The wraps the stopped run owed are published on a record tree, and
	// no record of an adopted tree is left.
	st := h.trees("stopped")
	cur := st.current(state.TreeRecord)
	if cur == "" || !h.chain.Mined(cur) {
		t.Fatalf("no record tree after the recovery: %+v\n%s", st, errs)
	}
	for _, p := range st.PendingTrees {
		if p.Tree.Txid == cur {
			t.Fatalf("the adopted tree still has a record: %+v", st)
		}
	}
	if r := h.must("alice", "open", group); !strings.Contains(r.stdout, "epoch") {
		t.Fatalf("open:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A tree the node does not know, with its coin unspent, keeps its record
// for one more command: the coin is back at once, and the record goes only
// when a second command hears the same answer.
func TestAReturnedCoinKeepsItsRecordForOneMoreCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgOnly()
	h.stoppedGroup(o, false)
	coin := h.trees("stopped").coin()
	errs := h.settle("stopped")
	st := h.trees("stopped")
	if len(st.PendingTrees) != 1 || !st.PendingTrees[0].ReturnedOnce || !h.poolCoins("stopped")[coin] ||
		!strings.Contains(errs, "its record is kept for one more command") {
		t.Fatalf("after the first command: %+v, coin in the pool %v\n%s", st, h.poolCoins("stopped")[coin], errs)
	}
	errs = h.settle("stopped")
	st = h.trees("stopped")
	if len(st.PendingTrees) != 0 || !h.poolCoins("stopped")[coin] || !strings.Contains(errs, "its record is dropped") {
		t.Fatalf("after the second command: %+v, coin in the pool %v\n%s", st, h.poolCoins("stopped")[coin], errs)
	}
	if errs = h.settle("stopped"); strings.Contains(errs, "funding tree") {
		t.Fatalf("a third command still speaks of the tree:\n%s", errs)
	}
}

// A tree that reaches the node after a command put its coin back is found
// by the next command, which adopts it and takes the coin, now spent, out
// of the pool: nothing is built on a spent coin.
func TestATreeThatLandsAfterItsCoinWentBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgOnly()
	tree, group := h.stoppedGroup(o, false)
	id := tree.TxID().String()
	coin := h.trees("stopped").coin()
	h.settle("stopped")
	if !h.poolCoins("stopped")[coin] {
		t.Fatal("the coin did not go back")
	}
	// The tree lands after all.
	if err := h.chain.Send(tree); err != nil {
		t.Fatal(err)
	}
	errs := h.settle("stopped")
	st := h.trees("stopped")
	if st.current(state.TreeRecord) != id || len(st.PendingTrees) != 0 {
		t.Fatalf("the tree that landed late: %+v\n%s", st, errs)
	}
	pool := h.poolCoins("stopped")
	if pool[coin] || !strings.Contains(errs, "coin "+coin+" is spent on the chain and is taken out of the pool") {
		t.Fatalf("the spent coin is still in the pool (%v):\n%s", pool[coin], errs)
	}
	if !pool[id+".8"] {
		t.Fatalf("the tree's change is not in the pool: %v", pool)
	}
	// The wraps owed are published on the tree that landed.
	h.start("stopped")
	if got := h.trees("stopped").current(state.TreeRecord); got != id {
		t.Fatalf("the next command minted another record tree: %s", got)
	}
	if r := h.must("alice", "open", group); !strings.Contains(r.stdout, "epoch") {
		t.Fatalf("open:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A run that stops after the settlement leg took a tree and before the
// home adopted it loses neither the tree nor its change: the next command
// finds the tree on the chain and adopts it.
func TestARunStoppedAfterTheTreeWasSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgOnly()
	tree, group := h.stoppedGroup(o, true)
	id := tree.TxID().String()
	coins := len(h.poolCoins("stopped"))
	errs := h.start("stopped")
	if !strings.Contains(errs, "funding tree "+id+" is recovered") {
		t.Fatalf("the next command:\n%s", errs)
	}
	st := h.trees("stopped")
	if st.current(state.TreeRecord) != id || len(st.PendingTrees) != 0 {
		t.Fatalf("after the recovery: %+v", st)
	}
	// The tree's change is in the pool, beside every coin the run left.
	pool := h.poolCoins("stopped")
	if len(pool) != coins+1 || !pool[id+".8"] {
		t.Fatalf("the pool after the recovery: %d coin(s), %d when the run stopped, the tree's change %v", len(pool), coins, pool[id+".8"])
	}
	if r := h.must("alice", "open", group); !strings.Contains(r.stdout, "epoch") {
		t.Fatalf("open:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A run that stops while a tree minted ahead is on its way to the chain
// leaves a tree no list of the home names. The next command finds it, holds
// it as the tree minted ahead, mints no other, and the switch adopts it and
// drops its record.
func TestARunStoppedWhileATreeWasMintedAhead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.orgWithGroup()
	// Five grant outputs are left of eight: one more grant leaves four, and
	// the next grant tree is minted ahead. The grant of the run that stops
	// reaches no host: the home that goes on is the copy.
	h.a.SetDown(true)
	h.b.SetDown(true)
	dave := h.identity("dave", false)
	tree := h.stopAt("org", "stopped", true, func() { h.run("org", "", "grant", "issue", dave) })
	h.a.SetDown(false)
	h.b.SetDown(false)
	id := tree.TxID().String()
	st := h.trees("stopped")
	first := st.current(state.TreeGrant)
	if first == "" || first == id || len(st.Trees[state.TreeGrant].Ahead) != 0 || len(st.PendingTrees) != 1 ||
		st.PendingTrees[0].Kind != state.TreeGrant || st.PendingTrees[0].Tree.Txid != id {
		t.Fatalf("the home the run left: %+v", st)
	}
	errs := h.start("stopped")
	if !strings.Contains(errs, "funding tree "+id+" is recovered and waits for the switch") || strings.Contains(errs, "so the next is minted ahead") {
		t.Fatalf("the next command:\n%s", errs)
	}
	st = h.trees("stopped")
	ahead := st.Trees[state.TreeGrant].Ahead
	if st.current(state.TreeGrant) != first || len(ahead) != 1 || ahead[0].Txid != id || len(st.PendingTrees) != 1 {
		t.Fatalf("after the recovery: %+v", st)
	}
	if !h.poolCoins("stopped")[id+".8"] {
		t.Fatal("the change of the tree minted ahead is not in the pool")
	}
	// The record stays until the switch, and no command mints another
	// grant tree or speaks of the recovery again.
	switched := false
	for i := 0; i < 6 && !switched; i++ {
		key := h.identity(fmt.Sprintf("member%d", i), false)
		r := h.must("stopped", "grant", "issue", key)
		if strings.Contains(r.stderr, "so the next is minted ahead") || strings.Contains(r.stderr, "is recovered") {
			t.Fatalf("grant %d:\n%s", i, r.stderr)
		}
		switched = strings.Contains(r.stderr, "funding tree "+id)
		if st = h.trees("stopped"); !switched && (st.current(state.TreeGrant) != first || len(st.PendingTrees) != 1) {
			t.Fatalf("before the switch: %+v", st)
		}
	}
	if st = h.trees("stopped"); !switched || st.current(state.TreeGrant) != id || len(st.Trees[state.TreeGrant].Ahead) != 0 || len(st.PendingTrees) != 0 {
		t.Fatalf("after the switch (%v): %+v", switched, st)
	}
}

// A node that cannot answer decides nothing: the record stays, the command
// says so and goes on, and a command that only reads asks nothing.
func TestARecoveryTheNodeCannotAnswerKeepsTheRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	o := h.orgWithGroup()
	// A record whose bytes are not the tree it names: nothing the node
	// says can settle it.
	h.editState("org", func(st map[string]any) {
		tree := st["trees"].(map[string]any)[state.TreeGrant].(map[string]any)["tree"].(map[string]any)
		st["pendingTrees"] = []any{map[string]any{
			"kind": state.TreeGrant,
			"tree": map[string]any{"identityKey": tree["identityKey"], "txid": strings.Repeat("ab", 32), "rawHex": tree["rawHex"], "count": 8, "sats": 1},
			"coin": map[string]any{"txid": strings.Repeat("cd", 32), "vout": 0, "satoshis": 5},
		}}
	})
	r := h.must("org", "epoch", "rotate", o.group)
	if !strings.Contains(r.stderr, "could not be settled now") || !strings.Contains(r.stderr, "its record is kept and the next command asks again") {
		t.Fatalf("the command:\n%s\n%s", r.stdout, r.stderr)
	}
	if st := h.trees("org"); len(st.PendingTrees) == 0 || st.PendingTrees[0].Tree.Txid != strings.Repeat("ab", 32) {
		t.Fatalf("the record: %+v", st)
	}
	if r := h.must("org", "roll"); strings.Contains(r.stderr, "funding tree") {
		t.Fatalf("roll:\n%s\n%s", r.stdout, r.stderr)
	}
}
