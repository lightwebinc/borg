package reader

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/vectors"
)

// fakeHost answers the group and group-seq classes from the tokens it is
// given: by seq, and as the latest whatever latest holds.
type fakeHost struct {
	mu     sync.Mutex
	bySeq  map[string][][]byte
	latest [][]byte
	asked  int
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Query map[string]string `json:"query"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &q)
	f.mu.Lock()
	f.asked++
	beefs := f.latest
	if s, ok := q.Query["seq"]; ok {
		beefs = f.bySeq[s]
	}
	f.mu.Unlock()
	type out struct {
		Beef        []byte `json:"beef"`
		OutputIndex int    `json:"outputIndex"`
	}
	outs := []out{}
	for _, b := range beefs {
		outs = append(outs, out{Beef: b})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "output-list", "outputs": outs})
}

// groupChain mines n tokens of one group, seq 0 to n-1, each naming the
// one before it.
func groupChain(t *testing.T, c chain, n int) ([]*transaction.Transaction, *borgrec.Group) {
	t.Helper()
	g := group0()
	tx := token(t, g, nil)
	c.mine(tx)
	txs := []*transaction.Transaction{tx}
	for i := 1; i < n; i++ {
		g = next(g, tx, false)
		tx = token(t, g, tx)
		c.mine(tx)
		txs = append(txs, tx)
	}
	return txs, g
}

// A seq a host claims, or a stray token of the organization's names, never
// sizes anything and never sets how far a reader walks: the chain is asked
// for seq by seq, only while each links to the one before.
func TestAHostileSeqEndsNoWalk(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	txs, g := groupChain(t, c, 2)
	// A token the organization signed at an enormous seq, mined: it links
	// to nothing.
	stray := *g
	stray.Seq, stray.Prev = 1<<50, [32]byte{9}
	strayTx := token(t, &stray, nil)
	c.mine(strayTx)
	f := &fakeHost{bySeq: map[string][][]byte{"0": {stored(t, txs[0])}, "1": {stored(t, txs[1])}}, latest: [][]byte{stored(t, strayTx)}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	rd := &Client{Hosts: []string{srv.URL}, Headers: c}
	v, err := rd.Chain(ctx, vectors.Hall, vectors.Org.Pub(), GroupHex(g.Group))
	if err != nil || len(v.Tokens) != 2 {
		t.Fatalf("the chain under a claimed seq of 2^50: %v, %d token(s)", err, len(v.Tokens))
	}
	if b := v.Beyond(); len(b) != 1 || b[0].Seq() != 1<<50 {
		t.Fatalf("the stray token is not reported above the chain: %+v", b)
	}
	// The latest class, seqs 0 and 1, and the seq nothing answered.
	if f.asked != 4 {
		t.Fatalf("%d questions were asked for a chain of 2 tokens", f.asked)
	}
}

// Hosts that withhold a token in the middle of a chain cannot hide that
// later ones exist: what they answer above the gap verifies and does not
// link, and is reported.
func TestTokensAboveAGapAreReported(t *testing.T) {
	c := chain{}
	txs, g := groupChain(t, c, 3)
	f := &fakeHost{bySeq: map[string][][]byte{"0": {stored(t, txs[0])}, "2": {stored(t, txs[2])}}, latest: [][]byte{stored(t, txs[2])}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	rd := &Client{Hosts: []string{srv.URL}, Headers: c}
	v, err := rd.Chain(context.Background(), vectors.Hall, vectors.Org.Pub(), GroupHex(g.Group))
	if err != nil || len(v.Tokens) != 1 {
		t.Fatalf("%v, %d token(s)", err, len(v.Tokens))
	}
	if b := v.Beyond(); len(b) != 1 || b[0].Txid != txs[2].TxID().String() {
		t.Fatalf("the token above the gap is not reported: %+v", b)
	}
	// A whole chain has nothing beyond it.
	f.bySeq["1"] = [][]byte{stored(t, txs[1])}
	if v, err = rd.Chain(context.Background(), vectors.Hall, vectors.Org.Pub(), GroupHex(g.Group)); err != nil || len(v.Tokens) != 3 || len(v.Beyond()) != 0 {
		t.Fatalf("the whole chain: %v, %d token(s), %d beyond", err, len(v.Tokens), len(v.Beyond()))
	}
}

// A paged walk holds what the contract's pages hold and no more, whatever a
// host answers.
func TestPagesAreBounded(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[`))
		for i := 0; i < 5000; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"beef":[1,2,3],"outputIndex":0}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	c := &Client{Hosts: []string{srv.URL}}
	ho := c.Pages(context.Background(), srv.URL, Query{"hall": vectors.Hall, "roll": "00"}, true, func(lookup.Output) string { return "a" })
	if ho.Err != nil || !ho.Truncated || len(ho.Outputs) > pageOutputs || asked != 1 {
		t.Fatalf("a page of 5000 outputs: %d kept, truncated %v, %d page(s) asked, %v", len(ho.Outputs), ho.Truncated, asked, ho.Err)
	}
}

// A host that answers full pages that never move past the cursor is asked
// no further.
func TestAPageThatDoesNotAdvanceEndsTheWalk(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[`))
		for i := 0; i < 64; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"beef":[1,2,3],"outputIndex":0}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	c := &Client{Hosts: []string{srv.URL}}
	ho := c.Pages(context.Background(), srv.URL, Query{"hall": vectors.Hall, "roll": "00"}, true, func(lookup.Output) string { return "a" })
	if ho.Err != nil || !ho.Truncated || asked != 2 || len(ho.Outputs) != 128 {
		t.Fatalf("a host repeating one page: %d kept, truncated %v, %d page(s) asked, %v", len(ho.Outputs), ho.Truncated, asked, ho.Err)
	}
}

// An answer over the bound is not read.
func TestAnAnswerIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[`))
		for i := 0; i < 2000; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"beef":[1,2,3],"outputIndex":0}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	c := &Client{MaxAnswer: 1 << 10}
	if _, err := c.Ask(context.Background(), srv.URL, Query{}); err == nil {
		t.Fatal("an answer of 60 KB was read under a bound of 1 KB")
	}
	if DefaultMaxAnswer > 16<<20 {
		t.Fatalf("the default bound is %d bytes", DefaultMaxAnswer)
	}
	c = &Client{}
	if outs, err := c.Ask(context.Background(), srv.URL, Query{}); err != nil || len(outs) != 2000 {
		t.Fatalf("under the default bound: %d outputs, %v", len(outs), err)
	}
}

// An unpaged answer over a page is a host fault: cut, and the host named.
// Held does not scan it.
func TestAnAnswerOverAPageIsNamed(t *testing.T) {
	outs := make([]lookup.Output, 2000)
	got, over := capAnswer("h", outs, pageOutputs)
	if len(got) != pageOutputs || over == nil || over.Host != "h" || !errors.Is(over.Err, ErrOverfull) {
		t.Fatalf("an answer of 2000: %d read, refusal %+v", len(got), over)
	}
	if got, over = capAnswer("h", outs[:pageOutputs], pageOutputs); len(got) != pageOutputs || over != nil {
		t.Fatalf("an answer of a page: %d read, refusal %+v", len(got), over)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"output-list","outputs":[`))
		for i := 0; i < 2000; i++ {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(`{"beef":[1,2,3],"outputIndex":0}`))
		}
		_, _ = w.Write([]byte(`]}`))
	}))
	defer srv.Close()
	if ok, err := (&Client{}).Held(context.Background(), srv.URL, Query{}, "00"); ok || !errors.Is(err, ErrOverfull) {
		t.Fatalf("Held over a page: %v, %v", ok, err)
	}
}

// source answers a chain source's current proof of one transaction.
type source struct {
	mp  *transaction.MerklePath
	err error
}

func (s source) MerkleProof(context.Context, string) (*transaction.MerklePath, error) {
	return s.mp, s.err
}

// A host stores a token with the proof it was given. After a
// reorganization that proof may name a block that is no longer in the best
// chain: a reader with a chain source takes the transaction's current
// proof from it, and one without refuses the token as unmined.
func TestAStaleProofIsReplacedFromTheChainSource(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	tx := token(t, group0(), nil)
	c.mine(tx)
	current := tx.MerklePath
	// The proof the host stored names another block.
	old := *current
	old.BlockHeight += 3
	tx.MerklePath = &old
	stale := stored(t, tx)
	if _, err := VerifyToken(ctx, stale, vectors.Hall, vectors.Org.Pub(), c); !errors.Is(err, borgrec.ErrUnmined) {
		t.Fatalf("a stale proof: %v", err)
	}
	rd := &Client{Headers: c}
	if _, err := rd.Token(ctx, stale, vectors.Hall, vectors.Org.Pub()); !errors.Is(err, borgrec.ErrUnmined) {
		t.Fatalf("a stale proof with no chain source: %v", err)
	}
	rd.Source = source{mp: current}
	tok, err := rd.Token(ctx, stale, vectors.Hall, vectors.Org.Pub())
	if err != nil || tok.Height != current.BlockHeight || tok.Txid != tx.TxID().String() {
		t.Fatalf("a stale proof with a chain source: %v %+v", err, tok)
	}
	// A source that fails, or whose proof does not verify either, replaces
	// nothing.
	rd.Source = source{err: errors.New("down")}
	if _, err := rd.Token(ctx, stale, vectors.Hall, vectors.Org.Pub()); !errors.Is(err, borgrec.ErrUnmined) {
		t.Fatalf("a chain source that is down: %v", err)
	}
	rd.Source = source{mp: &old}
	if _, err := rd.Token(ctx, stale, vectors.Hall, vectors.Org.Pub()); !errors.Is(err, borgrec.ErrUnmined) {
		t.Fatalf("a chain source with a proof that does not verify: %v", err)
	}
}

// memory is a Floor in memory.
type memory map[string]struct {
	seq  uint64
	txid string
}

func (m memory) Seen(hall, org, chain string) (uint64, string, bool) {
	s, ok := m[hall+"/"+org+"/"+chain]
	return s.seq, s.txid, ok
}

func (m memory) See(hall, org, chain string, seq uint64, txid string) {
	m[hall+"/"+org+"/"+chain] = struct {
		seq  uint64
		txid string
	}{seq, txid}
}

// A reader with a Floor refuses hosts that answer a chain only below the
// seq it verified before, and remembers what it verifies.
func TestTheFloorRefusesAnOlderState(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	txs, g := groupChain(t, c, 3)
	gid := GroupHex(g.Group)
	whole := map[string][][]byte{"0": {stored(t, txs[0])}, "1": {stored(t, txs[1])}, "2": {stored(t, txs[2])}}
	f := &fakeHost{bySeq: whole, latest: whole["2"]}
	srv := httptest.NewServer(f)
	defer srv.Close()
	m := memory{}
	orgHex := hex.EncodeToString(vectors.Org.Pub())
	rd := &Client{Hosts: []string{srv.URL}, Headers: c, Floor: m}
	if _, err := rd.Chain(ctx, vectors.Hall, vectors.Org.Pub(), gid); err != nil {
		t.Fatal(err)
	}
	if seq, txid, ok := m.Seen(vectors.Hall, orgHex, gid); !ok || seq != 2 || txid != txs[2].TxID().String() {
		t.Fatalf("the floor after a read: seq %d %s %v", seq, txid, ok)
	}
	// The hosts roll the chain back: seq 0 alone, answered as the latest.
	f.mu.Lock()
	f.bySeq, f.latest = map[string][][]byte{"0": whole["0"][:1]}, whole["0"]
	f.mu.Unlock()
	var back *RollbackError
	if _, err := rd.Chain(ctx, vectors.Hall, vectors.Org.Pub(), gid); !errors.As(err, &back) || back.Answered != 0 || back.Seq != 2 {
		t.Fatalf("an older state: %v", err)
	}
	if seq, _, _ := m.Seen(vectors.Hall, orgHex, gid); seq != 2 {
		t.Fatalf("the floor was lowered to seq %d", seq)
	}
	// A reader with no floor believes them.
	if v, err := (&Client{Hosts: []string{srv.URL}, Headers: c}).Chain(ctx, vectors.Hall, vectors.Org.Pub(), gid); err != nil || len(v.Tokens) != 1 {
		t.Fatalf("with no floor: %v", err)
	}
	// Another token at the seq remembered is reported, not refused.
	f.mu.Lock()
	f.bySeq, f.latest = whole, whole["2"]
	f.mu.Unlock()
	m.See(vectors.Hall, orgHex, gid, 1, "another")
	v, err := rd.Chain(ctx, vectors.Hall, vectors.Org.Pub(), gid)
	if err != nil || v.Replaced != "another" {
		t.Fatalf("a token replaced at the floor: %v %q", err, v.Replaced)
	}
}
