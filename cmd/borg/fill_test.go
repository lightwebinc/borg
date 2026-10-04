package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/borg/internal/state"
)

// chainTrees are the chain trees the seq 0 tokens of the organisation's
// chains spend, as the home name records them.
func (h *harness) chainTrees(name string) []string {
	h.t.Helper()
	st, err := state.Load(filepath.Join(h.dir, name), "")
	if err != nil || st.Org == nil {
		h.t.Fatalf("state of %s: %v", name, err)
	}
	seen := map[string]bool{}
	var out []string
	add := func(ts []state.Token) {
		if len(ts) > 0 && !seen[ts[0].Parent] {
			seen[ts[0].Parent] = true
			out = append(out, ts[0].Parent)
		}
	}
	add(st.Org.Tokens)
	for _, g := range st.Org.Groups {
		add(g.Tokens)
	}
	return out
}

// hasTree reports whether the home name keeps a record of tree txid.
func (h *harness) hasTree(name, txid string) bool {
	h.t.Helper()
	st, err := state.Load(filepath.Join(h.dir, name), "")
	if err != nil {
		h.t.Fatal(err)
	}
	_, _, ok := st.TreeOf(txid)
	return ok
}

// editChainTrees applies edit to every record of a chain tree the home
// name keeps.
func (h *harness) editChainTrees(name string, edit func(tr map[string]any)) {
	h.t.Helper()
	h.editState(name, func(st map[string]any) {
		set, _ := st["trees"].(map[string]any)["chain"].(map[string]any)
		if set == nil {
			h.t.Fatalf("%s keeps no chain trees", name)
		}
		if tr, ok := set["tree"].(map[string]any); ok {
			edit(tr)
		}
		if all, ok := set["trees"].([]any); ok {
			for _, tr := range all {
				edit(tr.(map[string]any))
			}
		}
	})
}

// forgetTokens makes host b lose every token it holds.
func (h *harness) forgetTokens() []string {
	ids := h.b.Txids("token")
	for _, id := range ids {
		h.b.Forget(id)
	}
	return ids
}

// Once the node drops the chain trees (fully spent), a host that lost the
// first tokens is filled from the organisation's home, which keeps each
// tree with its proof; a second home records the trees when it catches
// up. A home that is not the organisation's, and a home made before the
// record was kept, name the tree and fail.
func TestFillTakesAPrunedChainTreeFromTheOwnerHome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["BORG_SIGNERS"] = "2"
	o := h.orgWithGroup()
	h.block()
	h.secondHome("org2")
	h.must("org", "org", "sync")
	h.must("org2", "org", "sync")
	trees := h.chainTrees("org")
	if len(trees) == 0 {
		t.Fatal("no chain tree recorded")
	}
	for _, id := range trees {
		if !h.hasTree("org2", id) {
			t.Fatalf("the second home did not record chain tree %s when it caught up", id)
		}
	}
	h.prune(trees...)

	// A member's home keeps no chain tree: the fill names the tree and
	// where to run it.
	lost := h.forgetTokens()
	r := h.run("alice", "", "fill")
	h.want(r, exitIncomplete, "this home is not the organisation's")
	if !strings.Contains(r.stderr, "its chain tree "+trees[0]) || !strings.Contains(r.stderr, "run borg fill from the organisation's home") {
		t.Fatalf("fill from a member's home:\n%s\n%s", r.stdout, r.stderr)
	}

	// The organisation's home supplies the trees.
	r = h.must("org", "fill")
	if !strings.Contains(r.stdout, "organisation chain seq 0") || !strings.Contains(r.stdout, "group "+o.group+" seq 0") {
		t.Fatalf("fill from the organisation's home:\n%s\n%s", r.stdout, r.stderr)
	}
	for _, id := range lost {
		if !h.b.Holds(id) {
			t.Fatalf("host b still lacks %s after the fill:\n%s", id, r.stdout)
		}
	}

	// So does the second home, from the record it made at its catch-up.
	h.forgetTokens()
	if r := h.must("org2", "fill"); !strings.Contains(r.stdout, "organisation chain seq 0") {
		t.Fatalf("fill from the second home:\n%s\n%s", r.stdout, r.stderr)
	}

	// A home made by org sync before the record was kept has none.
	h.editState("org2", func(st map[string]any) {
		delete(st["trees"].(map[string]any), "chain")
	})
	h.forgetTokens()
	r = h.run("org2", "", "fill")
	h.want(r, exitIncomplete, "this home keeps no record of it")
	if !strings.Contains(r.stderr, "its chain tree "+trees[0]) || strings.Contains(r.stdout, "filled  organisation chain seq 0") {
		t.Fatalf("fill from a home without the record:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A chain tree in the home is held to what the node's answer would be: a
// record whose bytes are another transaction is refused, and a kept proof
// a reorganisation left stale is replaced by the node's current one, or
// named when the node gives none.
func TestFillChecksTheHomeChainTree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.orgWithGroup()
	trees := h.chainTrees("org")

	// A kept proof of another block: the node's current proof replaces
	// it.
	h.editChainTrees("org", func(tr map[string]any) {
		mp, err := transaction.NewMerklePathFromHex(tr["bumpHex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		mp.BlockHeight += 3
		tr["bumpHex"] = mp.Hex()
	})
	h.forgetTokens()
	r := h.must("org", "fill")
	if !strings.Contains(r.stdout, "organisation chain seq 0") || !strings.Contains(r.stderr, "the node's current proof") {
		t.Fatalf("fill with a stale kept proof:\n%s\n%s", r.stdout, r.stderr)
	}
	// With the node pruned there is none: the message says so.
	h.prune(trees...)
	h.forgetTokens()
	r = h.run("org", "", "fill")
	h.want(r, exitIncomplete, "its kept proof does not verify against the header source (a reorganisation, or no proof kept), and the node gives no current one")

	// Bytes that are not the token's parent are refused.
	h.editChainTrees("org", func(tr map[string]any) {
		raw := tr["rawHex"].(string)
		last := "0"
		if raw[len(raw)-1] == '0' {
			last = "1"
		}
		tr["rawHex"] = raw[:len(raw)-1] + last
	})
	h.forgetTokens()
	r = h.run("org", "", "fill")
	h.want(r, exitIncomplete, "this home's record of the chain tree "+trees[0]+" is REFUSED: its bytes are transaction")
	if !strings.Contains(r.stderr, "the node does not answer it") {
		t.Fatalf("fill with a tampered tree:\n%s\n%s", r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout, "filled  organisation chain seq 0") {
		t.Fatalf("a token was filled from a tampered tree:\n%s", r.stdout)
	}
}
