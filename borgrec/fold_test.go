package borgrec

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden vectors of the example group: its four tokens, and the epochs
// as the vectors record them.

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(name, ".hex") {
		if b, err = hex.DecodeString(strings.TrimSpace(string(b))); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

// goldenLinks is the example group's chain: each token's txid is the one
// its successor names as prev.
func goldenLinks(t *testing.T) []Link {
	t.Helper()
	var out []Link
	for i, f := range []string{"group-create.hex", "group-add.hex", "group-remove.hex", "group-rotate.hex"} {
		rec := golden(t, f)
		g, err := DecodeGroup(rec)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if i > 0 {
			out[i-1].Txid = g.Prev
		}
		out = append(out, Link{Record: rec, Height: uint32(1000 + 10*i)}) //nolint:gosec // four tokens
	}
	return out
}

func TestFoldGroupGivesTheGoldenEpochs(t *testing.T) {
	var want struct {
		Epochs []struct {
			Epoch      uint64 `json:"epoch"`
			Commitment string `json:"commitment"`
			EpochID    string `json:"epochId"`
			Began      uint64 `json:"beganAtSeq"`
		} `json:"epochs"`
	}
	if err := json.Unmarshal(golden(t, "epoch-v1.json"), &want); err != nil {
		t.Fatal(err)
	}
	l := goldenLinks(t)
	c, err := FoldGroup(l)
	if err != nil {
		t.Fatal(err)
	}
	if c.Usable() != 4 {
		t.Fatalf("usable %d, statuses %v", c.Usable(), c.Statuses)
	}
	if len(c.Epochs) != len(want.Epochs) || len(want.Epochs) != 3 {
		t.Fatalf("%d epochs, want %d", len(c.Epochs), len(want.Epochs))
	}
	for i, e := range c.Epochs {
		w := want.Epochs[i]
		if e.Epoch != w.Epoch || hex.EncodeToString(e.ID[:]) != w.EpochID || hex.EncodeToString(e.Commitment[:]) != w.Commitment || e.Seq != w.Began {
			t.Errorf("epoch %d: %+v, want %+v", i, e, w)
		}
		if e.Height != l[e.Seq].Height || e.Txid != l[e.Seq].Txid {
			t.Errorf("epoch %d: height %d and txid are not its token's", i, e.Height)
		}
	}
	if c.Epochs[0].Removed || !c.Epochs[1].Removed || c.Epochs[2].Removed {
		t.Errorf("removed flags %+v", c.Epochs)
	}
	if len(c.Members()) != 2 || c.Latest().Epoch != 2 {
		t.Errorf("members %d, latest epoch %d", len(c.Members()), c.Latest().Epoch)
	}
	if e, ok := c.EpochByNumber(1); !ok || e.Seq != 2 {
		t.Errorf("epoch 1: %+v %v", e, ok)
	}
	if _, ok := c.EpochByNumber(3); ok {
		t.Error("epoch 3 was never begun")
	}
	// The fold is GroupFold's, token by token.
	f := NewGroupFold()
	for i, g := range c.Tokens {
		if s := f.Apply(g); s != c.Statuses[i] {
			t.Errorf("seq %d: status %s, GroupFold gives %s", i, c.Statuses[i], s)
		}
	}
}

func TestFoldGroupRefuses(t *testing.T) {
	l := goldenLinks(t)
	if _, err := FoldGroup(l[1:]); !errors.Is(err, ErrChain) {
		t.Errorf("a chain that starts at seq 1: %v", err)
	}
	if _, err := FoldGroup([]Link{l[0], l[2]}); !errors.Is(err, ErrTransition) {
		t.Errorf("a missing token: %v", err)
	}
	wrong := append([]Link(nil), l...)
	wrong[0].Txid[0] ^= 1
	if _, err := FoldGroup(wrong); !errors.Is(err, ErrTransition) {
		t.Errorf("a prev that is not the predecessor's txid: %v", err)
	}
	low := append([]Link(nil), l...)
	low[2].Height = 5
	if _, err := FoldGroup(low); !errors.Is(err, ErrChain) {
		t.Errorf("a token mined below its predecessor: %v", err)
	}
	cut := l[0].Record
	if _, err := FoldGroup([]Link{{Record: cut[:len(cut)-1]}}); Reason(err) != "cbor" {
		t.Errorf("a cut record: %v", err)
	}
	if c, err := FoldGroup(nil); err != nil || c.Usable() != 0 || c.Latest() != nil || len(c.Epochs) != 0 {
		t.Errorf("no tokens: %+v %v", c, err)
	}
}

// A first token whose members root is not its members' is admitted by a
// host, which cannot hold the set, and unusable to a reader: no epoch, no
// member, no latest token.
func TestFoldGroupFailsClosedOnAMismatch(t *testing.T) {
	g, err := DecodeGroup(golden(t, "group-create.hex"))
	if err != nil {
		t.Fatal(err)
	}
	g.Members[0] ^= 1
	rec, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	c, err := FoldGroup([]Link{{Record: rec, Height: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Statuses[0] != StatusMembersMismatch || c.Usable() != 0 || c.Latest() != nil || len(c.Epochs) != 0 || len(c.Members()) != 0 {
		t.Errorf("a mismatched first token: %+v", c)
	}
}

// TestFoldCloneIsIndependent: a clone folds a second successor without
// touching the fold it was taken from, and Holds reads the members and the
// roll after the last token folded.
func TestFoldCloneIsIndependent(t *testing.T) {
	recs := []string{"group-create.hex", "group-add.hex", "group-remove.hex"}
	f := NewGroupFold()
	var gs []*Group
	for _, n := range recs {
		g, err := DecodeGroup(golden(t, n))
		if err != nil {
			t.Fatal(err)
		}
		gs = append(gs, g)
	}
	if s := f.Apply(gs[0]); s != StatusConsistent {
		t.Fatal(s)
	}
	c := f.Clone()
	if s := c.Apply(gs[1]); s != StatusConsistent {
		t.Fatal(s)
	}
	for _, k := range gs[1].Add {
		if f.Holds(k) || !c.Holds(k) {
			t.Fatal("the clone's member reached the fold it was taken from")
		}
	}
	for _, k := range gs[0].Add {
		if !f.Holds(k) || !c.Holds(k) {
			t.Fatal("a member of seq 0 is not held")
		}
	}
	o := NewOrgFold()
	var a, b [32]byte
	a[0], b[0] = 1, 2
	roll := [][32]byte{a}
	if s := o.Apply(&Org{Admit: roll, Count: 1, Roll: RollRoot(roll), Policy: PolicyRoot(nil)}); s != StatusConsistent {
		t.Fatal(s)
	}
	oc := o.Clone()
	both := [][32]byte{a, b}
	if s := oc.Apply(&Org{Admit: [][32]byte{b}, Count: 2, Roll: RollRoot(both), Policy: PolicyRoot(nil)}); s != StatusConsistent {
		t.Fatal(s)
	}
	if !o.Holds(a) || o.Holds(b) || !oc.Holds(b) {
		t.Fatal("the organization's clone is not independent")
	}
}
