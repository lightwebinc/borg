// Package limits measures what borg puts on the plane, so that
// docs/limits.md quotes sizes a program produced rather than estimates.
package limits

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/vectors"
)

// fragment is the object bytes one plane packet carries at the 1280-byte
// IPv6 minimum MTU, less the IPv6, UDP and object fragment headers.
const fragment = 1128

// depth is the Merkle path depth of a mined transaction's block: 2^16 is a
// block of about 65,000 transactions.
const depth = 16

func prove(tx *transaction.Transaction) {
	isTxid := true
	path := make([][]*transaction.PathElement, depth)
	for l := 0; l < depth; l++ {
		sib := chainhash.Hash{byte(l + 1)}
		path[l] = []*transaction.PathElement{{Offset: 1, Hash: &sib}}
	}
	path[0] = append([]*transaction.PathElement{{Offset: 0, Hash: tx.TxID(), Txid: &isTxid}}, path[0]...)
	tx.MerklePath = transaction.NewMerklePath(900_000, path)
}

func coin(k *ec.PrivateKey, sats uint64) (*transaction.Transaction, *script.Script) {
	addr, _ := script.NewAddressFromPublicKey(k.PubKey(), false)
	payTo, _ := p2pkh.Lock(addr)
	c := transaction.NewTransaction()
	nowhere := chainhash.Hash{0x11}
	c.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	c.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: payTo})
	return c, payTo
}

// carrierBEEF mints a real carrier for rec from a mined funding tree of
// count outputs and returns the Atomic BEEF a publisher sends.
func carrierBEEF(t *testing.T, rec []byte, count int, deriv pushdrop.Derivation) int {
	t.Helper()
	ctx := context.Background()
	k := vectors.Org.Priv
	w, err := wallet.NewCompletedProtoWallet(k)
	if err != nil {
		t.Fatal(err)
	}
	p := carrier.Params{Derivation: deriv, FundingTag: borgrec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }}
	c, payTo := coin(k, 10_000_000)
	payer, _ := p2pkh.Unlock(k, nil)
	lock, err := carrier.FundingLock(ctx, w, "", p)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mint.FundingTree(lock, count, 1000, mint.Input{Tx: c, Vout: 0, Unlocker: payer}, payTo, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	prove(tree)
	tx, err := carrier.Mint(ctx, w, "", p, rec, tree, 0)
	if err != nil {
		t.Fatal(err)
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return len(beef)
}

// tokenBEEF builds a mined chain-token transaction (the token, one
// predecessor input, a fee input and change) and returns the Atomic BEEF a
// publisher sends: the token and its predecessor's transaction, each proven.
func tokenBEEF(t *testing.T, tag, rec []byte, d interface {
	Lock(context.Context, wallet.Interface, string, [][]byte, bool) (*script.Script, error)
}) int {
	t.Helper()
	ctx := context.Background()
	k := vectors.Org.Priv
	w, _ := wallet.NewCompletedProtoWallet(k)
	lock, err := d.Lock(ctx, w, "", [][]byte{tag, rec}, true)
	if err != nil {
		t.Fatal(err)
	}
	c, payTo := coin(k, 100_000)
	payer, _ := p2pkh.Unlock(k, nil)
	tx := transaction.NewTransaction()
	if err := tx.AddInputFrom(c.TxID().String(), 0, c.Outputs[0].LockingScript.String(), 100_000, payer); err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].SourceTransaction = c
	// A second input stands for the predecessor token (one push of a
	// signature, the funding-output unlocking shape).
	prevTok := chainhash.Hash{0x22}
	sig := bytes.Repeat([]byte{0x30}, 72)
	unlock := script.Script(append([]byte{byte(len(sig))}, sig...))
	tx.AddInput(&transaction.TransactionInput{SourceTXID: &prevTok, UnlockingScript: &unlock, SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 99_000, LockingScript: payTo})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	prove(tx)
	// The published BEEF carries the token and the transaction its
	// predecessor input spends, each with its own path (spec section 11.1).
	// A previous token of the same shape stands for that transaction.
	parent := transaction.NewTransaction()
	nowhere := chainhash.Hash{0x33}
	parent.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, UnlockingScript: &unlock, SequenceNumber: transaction.MaxTxInSequenceNum})
	parent.AddInput(&transaction.TransactionInput{SourceTXID: &nowhere, SourceTxOutIndex: 1, UnlockingScript: &unlock, SequenceNumber: transaction.MaxTxInSequenceNum})
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 99_000, LockingScript: payTo})
	prove(parent)
	b := transaction.NewBeefV1()
	if _, err := b.MergeTransaction(parent); err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].SourceTransaction = nil
	if _, err := b.MergeTransaction(tx); err != nil {
		t.Fatal(err)
	}
	// Not Atomic BEEF: BRC-95 holds only the subject's dependencies, and a
	// proven subject has none, so the predecessor rides in a BEEF V1.
	beef, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	back, err := transaction.NewBeefFromBytes(beef)
	if err != nil || back.FindTransaction(parent.TxID().String()) == nil || back.FindTransaction(tx.TxID().String()) == nil {
		t.Fatalf("the token's BEEF lost a transaction: %v", err)
	}
	return len(beef)
}

func packets(n int) int { return (n + fragment - 1) / fragment }

// wrapRecord builds a wrap record of n entries for one epoch.
func wrapRecord(t *testing.T, n int) []byte {
	t.Helper()
	org := vectors.Org.Priv
	id := [32]byte{1}
	k := [32]byte{2}
	w := &borgrec.WrapRecord{Hall: vectors.Hall, Org: org.PubKey().Compressed(), Group: vectors.GroupID(), Epoch: 1, EpochID: id}
	var keys []*ec.PrivateKey
	for i := 0; i < n; i++ {
		b := bytes.Repeat([]byte{byte(0x20 + i)}, 32)
		mk, _ := ec.PrivateKeyFromBytes(b)
		keys = append(keys, mk)
	}
	// Entries ascend by recipient key.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && bytes.Compare(keys[j-1].PubKey().Compressed(), keys[j].PubKey().Compressed()) > 0; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, mk := range keys {
		e, err := borgrec.WrapWithIV(org, mk.PubKey(), id, 1, k, [32]byte{3})
		if err != nil {
			t.Fatal(err)
		}
		w.Entries = append(w.Entries, e)
	}
	rec, err := w.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// groupRecord builds a group record with n additions and m removals.
func groupRecord(t *testing.T, add, remove int) []byte {
	t.Helper()
	g := &borgrec.Group{Hall: vectors.Hall, Org: vectors.Org.Pub(), Group: vectors.GroupID(), Seq: 9, Prev: [32]byte{9},
		Name: "Engineering platform team", Epoch: 4, EpochSeq: 9, Commitment: [32]byte{4}, Count: uint64(add + 100),
		Members: [32]byte{5}, Created: vectors.Created}
	var keys [][]byte
	for i := 0; i < add+remove; i++ {
		b := bytes.Repeat([]byte{byte(0x30 + i%200)}, 31)
		mk, _ := ec.PrivateKeyFromBytes(append(b, byte(i)))
		keys = append(keys, mk.PubKey().Compressed())
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && bytes.Compare(keys[j-1], keys[j]) > 0; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for i, k := range keys {
		if i%2 == 0 && len(g.Add) < add || len(g.Remove) >= remove {
			g.Add = append(g.Add, k)
		} else {
			g.Remove = append(g.Remove, k)
		}
	}
	g.Count = 100 + uint64(len(g.Add)) - uint64(len(g.Remove))
	rec, err := g.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestSizes measures every object borg publishes and holds the plane
// recommendations of docs/limits.md to them.
func TestSizes(t *testing.T) {
	s, err := vectors.Build()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	row := func(what string, rec, beef int) {
		lines = append(lines, fmt.Sprintf("%-46s record %5d  BEEF %6d  packets %2d", what, rec, beef, packets(beef)))
	}
	g := s.GrantRecs["bob"]
	for _, tree := range []int{32, 100} {
		n := carrierBEEF(t, g, tree, borgrec.GrantDerivation)
		row(fmt.Sprintf("grant carrier, tree %d", tree), len(g), n)
		if tree == 32 && packets(n) > 4 {
			t.Errorf("a grant carrier is %d packets", packets(n))
		}
	}
	for _, e := range []int{1, 8, 16} {
		rec := wrapRecord(t, e)
		n := carrierBEEF(t, rec, 32, borgrec.RecordDerivation)
		row(fmt.Sprintf("wrap carrier, %d entries, tree 32", e), len(rec), n)
		if e == 8 && packets(n) > 4 {
			t.Errorf("an 8-entry wrap carrier is %d packets", packets(n))
		}
		if e == 16 && packets(n) > 6 {
			t.Errorf("a 16-entry wrap carrier is %d packets", packets(n))
		}
	}
	for _, c := range []struct{ add, remove int }{{0, 0}, {2, 0}, {0, 1}, {16, 0}, {64, 64}} {
		rec := groupRecord(t, c.add, c.remove)
		n := tokenBEEF(t, borgrec.TagGroup, rec, borgrec.GroupDerivation)
		row(fmt.Sprintf("group token, add %d remove %d", c.add, c.remove), len(rec), n)
		if c.add <= 16 && c.remove <= 16 && packets(n) > 4 {
			t.Errorf("a group token of a small change is %d packets", packets(n))
		}
	}
	o := s.OrgRecs[0]
	n := tokenBEEF(t, borgrec.TagOrg, o, borgrec.OrgDerivation)
	row("organisation token, admit 3", len(o), n)
	for _, tree := range []int{8, 32} {
		n := genesisBEEF(t, s.GroupRecs[0], tree)
		row(fmt.Sprintf("group token seq 0, chain tree %d", tree), len(s.GroupRecs[0]), n)
		if tree == 8 && packets(n) > 4 {
			t.Errorf("a first token on a chain tree of 8 is %d packets", packets(n))
		}
	}
	for _, l := range lines {
		t.Log(l)
	}
}

// genesisBEEF builds a group's first token spending output 0 of a mined
// chain tree of count outputs, and returns the BEEF V1 of the token and the
// tree, each proven, as it is published.
func genesisBEEF(t *testing.T, rec []byte, count int) int {
	t.Helper()
	ctx := context.Background()
	k := vectors.Org.Priv
	w, _ := wallet.NewCompletedProtoWallet(k)
	p := carrier.Params{Derivation: borgrec.ChainDerivation, FundingTag: borgrec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }}
	c, payTo := coin(k, 10_000_000)
	payer, _ := p2pkh.Unlock(k, nil)
	flock, err := carrier.FundingLock(ctx, w, "", p)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := mint.FundingTree(flock, count, 1000, mint.Input{Tx: c, Vout: 0, Unlocker: payer}, payTo, mint.DefaultFees)
	if err != nil {
		t.Fatal(err)
	}
	prove(tree)
	lock, err := borgrec.GroupDerivation.Lock(ctx, w, "", [][]byte{borgrec.TagGroup, rec}, true)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	sig := bytes.Repeat([]byte{0x30}, 72)
	unlock := script.Script(append([]byte{byte(len(sig))}, sig...))
	tx.AddInput(&transaction.TransactionInput{SourceTXID: tree.TxID(), SourceTxOutIndex: 0, UnlockingScript: &unlock, SequenceNumber: transaction.MaxTxInSequenceNum})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 500, LockingScript: payTo})
	prove(tx)
	b := transaction.NewBeefV1()
	if _, err := b.MergeTransaction(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := b.MergeTransaction(tx); err != nil {
		t.Fatal(err)
	}
	beef, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return len(beef)
}
