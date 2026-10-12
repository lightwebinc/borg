package reader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/vectors"
)

// chain proves each transaction alone in a block at the next height.
type chain map[uint32]chainhash.Hash

func (c chain) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, h uint32) (bool, error) {
	r, ok := c[h]
	return ok && root != nil && *root == r, nil
}

func (c chain) CurrentHeight(context.Context) (uint32, error) { return uint32(len(c)) + 800, nil }

func (c chain) mine(tx *transaction.Transaction) {
	h := uint32(len(c)) + 801
	t := true
	sib := chainhash.Hash(bytes.Repeat([]byte{0x33}, 32))
	tx.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{{{Offset: 0, Hash: &sib}, {Offset: 1, Hash: tx.TxID(), Txid: &t}}})
	root, err := tx.MerklePath.ComputeRoot(tx.TxID())
	if err != nil {
		panic(err)
	}
	c[h] = *root
}

// token is a transaction holding one well-formed group token of rec.
func token(t *testing.T, rec *borgrec.Group, prev *transaction.Transaction) *transaction.Transaction {
	t.Helper()
	b, err := rec.Encode()
	if err != nil {
		t.Fatal(err)
	}
	lock, err := borgrec.GroupDerivation.Lock(context.Background(), vectors.Org.Wallet(), "", [][]byte{borgrec.TagGroup, b}, true)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	src := chainhash.Hash(bytes.Repeat([]byte{0x11}, 32))
	if prev != nil {
		src = *prev.TxID()
	}
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &src, SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	return tx
}

func group0() *borgrec.Group {
	ms := [][]byte{vectors.Alice.Pub(), vectors.Bob.Pub()}
	if bytes.Compare(ms[0], ms[1]) > 0 {
		ms[0], ms[1] = ms[1], ms[0]
	}
	return &borgrec.Group{Hall: vectors.Hall, Org: vectors.Org.Pub(), Group: vectors.GroupID(), Name: "g",
		Commitment: [32]byte{1}, Add: ms, Count: 2, Members: borgrec.MembersRoot(ms), Created: 1}
}

func next(g *borgrec.Group, prev *transaction.Transaction, rotate bool) *borgrec.Group {
	n := *g
	n.Seq, n.Prev, n.Add, n.Remove = g.Seq+1, [32]byte(*prev.TxID()), nil, nil
	if rotate {
		n.Epoch, n.EpochSeq, n.Commitment = g.Epoch+1, n.Seq, [32]byte{byte(g.Epoch + 2)}
	}
	return &n
}

func stored(t *testing.T, tx *transaction.Transaction) []byte {
	t.Helper()
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyToken(t *testing.T) {
	c := chain{}
	tx := token(t, group0(), nil)
	chain{}.mine(tx) // a proof of a block this reader's headers lack
	if _, err := VerifyToken(context.Background(), stored(t, tx), vectors.Hall, vectors.Org.Pub(), c); !errors.Is(err, borgrec.ErrUnmined) {
		t.Fatalf("an unmined token: %v", err)
	}
	c.mine(tx)
	tok, err := VerifyToken(context.Background(), stored(t, tx), vectors.Hall, vectors.Org.Pub(), c)
	if err != nil || tok.Seq() != 0 || tok.Height != 801 || tok.Pos != 1 {
		t.Fatalf("%v %+v", err, tok)
	}
	if _, err := VerifyToken(context.Background(), stored(t, tx), vectors.OtherHall, vectors.Org.Pub(), c); !errors.Is(err, borgrec.ErrHall) {
		t.Fatalf("another hall: %v", err)
	}
	if _, err := VerifyToken(context.Background(), stored(t, tx), vectors.Hall, vectors.Alice.Pub(), c); err == nil {
		t.Fatal("another organization's token")
	}
	if _, err := VerifyToken(context.Background(), stored(t, tx), vectors.Hall, nil, nil); !errors.Is(err, ErrNoHeaders) {
		t.Fatalf("no headers: %v", err)
	}
}

// spends answers the node's view of one outpoint.
type spends struct {
	by  string
	err error
}

func (s spends) Spender(context.Context, string, uint32) (string, error) { return s.by, s.err }

func TestLatestIsTheSpendIndex(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	g0 := group0()
	t0 := token(t, g0, nil)
	c.mine(t0)
	tok, err := VerifyToken(ctx, stored(t, t0), vectors.Hall, vectors.Org.Pub(), c)
	if err != nil {
		t.Fatal(err)
	}
	cv := &ChainView{Hall: vectors.Hall, Org: vectors.Org.Pub(), Chain: GroupHex(g0.Group), Tokens: []*Tok{tok}, Statuses: []string{borgrec.StatusConsistent}}
	gv, err := NewGroupView(cv)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if f, err := Latest(ctx, spends{}, nil, gv, now); err != nil || !f.OK() || !f.Unspent {
		t.Fatalf("unspent: %v %+v", err, f)
	}
	if f, err := Latest(ctx, spends{err: errors.New("down")}, nil, gv, now); err == nil || f.OK() {
		t.Fatal("a source that cannot answer is evidence")
	}
	if f, err := Latest(ctx, nil, nil, gv, now); err == nil || f.OK() {
		t.Fatal("no source is evidence")
	}
	keep := token(t, next(g0, t0, false), t0)
	rot := token(t, next(g0, t0, true), t0)
	raw := func(b *transaction.Transaction) func(context.Context, string) ([]byte, error) {
		return func(context.Context, string) ([]byte, error) { return b.Bytes(), nil }
	}
	f, err := Latest(ctx, spends{by: keep.TxID().String()}, raw(keep), gv, now)
	if err != nil || !f.OK() || f.Next == nil {
		t.Fatalf("spent by the next token keeping the epoch: %v %+v", err, f)
	}
	if _, err := Latest(ctx, spends{by: rot.TxID().String()}, raw(rot), gv, now); err == nil || !strings.Contains(err.Error(), "begins epoch 1") {
		t.Fatalf("spent by the next token beginning an epoch: %v", err)
	}
	other := transaction.NewTransaction()
	other.AddInput(&transaction.TransactionInput{SourceTXID: t0.TxID(), SequenceNumber: transaction.MaxTxInSequenceNum})
	other.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: t0.Outputs[0].LockingScript})
	if _, err := Latest(ctx, spends{by: other.TxID().String()}, raw(t0), gv, now); err == nil {
		t.Fatal("a spender whose bytes are another transaction's")
	}
}

// The writer's evidence fails closed on the node's answer: only status OK
// is unspent. Every other status, a SPENT naming no spender and a
// transaction the node does not serve is nodeapi.ErrSpendUnknown, and no
// evidence.
func TestLatestFailsClosedOnAnUnknownSpend(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	g0 := group0()
	t0 := token(t, g0, nil)
	c.mine(t0)
	tok, err := VerifyToken(ctx, stored(t, t0), vectors.Hall, vectors.Org.Pub(), c)
	if err != nil {
		t.Fatal(err)
	}
	cv := &ChainView{Hall: vectors.Hall, Org: vectors.Org.Pub(), Chain: GroupHex(g0.Group), Tokens: []*Tok{tok}, Statuses: []string{borgrec.StatusConsistent}}
	gv, err := NewGroupView(cv)
	if err != nil {
		t.Fatal(err)
	}
	vout := gv.Latest().Token.Vout
	answer := ""
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answer == "" || r.URL.Path != "/api/v1/utxos/"+t0.TxID().String()+"/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"vout":%d,%s}]`, vout, answer)
	}))
	defer node.Close()
	asset := &nodeapi.Asset{Base: node.URL}
	now := time.Now()
	answer = `"status":"OK"`
	if f, err := Latest(ctx, asset, nil, gv, now); err != nil || !f.OK() || !f.Unspent {
		t.Fatalf("status OK: %v %+v", err, f)
	}
	for _, a := range []string{`"status":"NOT_FOUND"`, `"status":"IMMATURE"`, `"status":"FROZEN"`, `"status":"SPENT"`, `"status":""`, ""} {
		answer = a
		f, err := Latest(ctx, asset, nil, gv, now)
		if err == nil || f.OK() || f.Unspent || !errors.Is(err, nodeapi.ErrSpendUnknown) || !strings.Contains(err.Error(), "could not say whether") {
			t.Fatalf("answer %q is evidence: %v %+v", a, err, f)
		}
	}
}

func TestGroupViewFolds(t *testing.T) {
	c := chain{}
	ctx := context.Background()
	g0 := group0()
	t0 := token(t, g0, nil)
	c.mine(t0)
	g1 := next(g0, t0, true)
	g1.Remove = [][]byte{vectors.Bob.Pub()}
	g1.Count, g1.Members = 1, borgrec.MembersRoot([][]byte{vectors.Alice.Pub()})
	t1 := token(t, g1, t0)
	c.mine(t1)
	var toks []*Tok
	for _, tx := range []*transaction.Transaction{t0, t1} {
		tok, err := VerifyToken(ctx, stored(t, tx), vectors.Hall, vectors.Org.Pub(), c)
		if err != nil {
			t.Fatal(err)
		}
		toks = append(toks, tok)
	}
	cv := &ChainView{Hall: vectors.Hall, Org: vectors.Org.Pub(), Chain: GroupHex(g0.Group), Tokens: toks}
	cv.fold()
	gv, err := NewGroupView(cv)
	if err != nil {
		t.Fatal(err)
	}
	if gv.Epoch() != 1 || !gv.IsMember(vectors.Alice.Pub()) || gv.IsMember(vectors.Bob.Pub()) || gv.Begins[1] != toks[1] {
		t.Fatalf("%+v", gv)
	}
	if e := gv.EpochsOf(vectors.Bob.Pub()); len(e) != 1 || e[0] != 0 {
		t.Fatalf("bob's epochs %v", e)
	}
	if e := gv.EpochsOf(vectors.Alice.Pub()); len(e) != 2 {
		t.Fatalf("alice's epochs %v", e)
	}
	if !toks[0].Before(toks[1]) || toks[1].Prev() != toks[0].Txid {
		t.Fatal("order and prev")
	}
}

// A spend error keeps its chain and a status word, and loses every other
// name the node chose.
func TestQuietSpendLeavesOutWhatTheNodeNamed(t *testing.T) {
	if QuietSpend(nil) != nil {
		t.Fatal("nil is an error")
	}
	evil := fmt.Errorf("%w: output 0 of x: the node names the spender %q, which is not a txid", nodeapi.ErrSpendUnknown, "\x1b]0;owned\aevil")
	q := QuietSpend(evil)
	if !errors.Is(q, nodeapi.ErrSpendUnknown) || strings.Contains(q.Error(), "owned") || !strings.Contains(q.Error(), "not repeated") {
		t.Fatalf("%v", q)
	}
	st := fmt.Errorf("%w: the node answers status %q", nodeapi.ErrSpendUnknown, "NOT_FOUND")
	if q := QuietSpend(st); q.Error() != st.Error() || !errors.Is(q, nodeapi.ErrSpendUnknown) {
		t.Fatalf("%v", q)
	}
}
