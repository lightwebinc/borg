package module

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/pushdrop"
	"github.com/lightwebinc/bcommon/termsroute"

	"github.com/lightwebinc/borg/borgrec"
)

// The module's test rig, the twin of the TypeScript tests' testutil.ts,
// testmint.ts and testhost.ts: a host whose counters, gauges and log read
// back, the golden vectors, a local chain and parties that mint funding
// trees, grants, policy records, wraps, chain tokens and sweeps, an
// organization's and a group's tooling over that chain, an in-memory store
// behaving as the Go engine's where the module depends on it, and the
// engine's admission path over it with the module's managers and lookup
// service mounted.

// fakeHost records what the module counts and logs.
type fakeHost struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]func() float64
	lines    []logLine
}

type logLine struct {
	msg   string
	extra map[string]any
}

func newFakeHost() *fakeHost {
	return &fakeHost{counters: map[string]float64{}, gauges: map[string]func() float64{}}
}

func mkey(name string, labels map[string]string) string {
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	if len(parts) == 0 {
		return name
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func (h *fakeHost) Log(msg string, extra map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines = append(h.lines, logLine{msg, extra})
}

func (h *fakeHost) Metrics() bmodule.Metrics { return h }

func (h *fakeHost) Inc(name string, labels map[string]string, by float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counters[mkey(name, labels)] += by
}

func (h *fakeHost) Preset(name string, labels map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.counters[mkey(name, labels)]; !ok {
		h.counters[mkey(name, labels)] = 0
	}
}

func (h *fakeHost) Gauge(name string, fn func() float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gauges[name] = fn
}

// count is a counter's value, 0 when never preset or counted.
func (h *fakeHost) count(name string, labels map[string]string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counters[mkey(name, labels)]
}

func (h *fakeHost) has(name string, labels map[string]string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.counters[mkey(name, labels)]
	return ok
}

func (h *fakeHost) gauge(name string) float64 {
	h.mu.Lock()
	fn := h.gauges[name]
	h.mu.Unlock()
	if fn == nil {
		return -1
	}
	return fn()
}

// logged counts the log lines whose message contains msg.
func (h *fakeHost) logged(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.lines {
		if strings.Contains(l.msg, msg) {
			n++
		}
	}
	return n
}

// loggedWith reports whether a line msg carries key = value.
func (h *fakeHost) loggedWith(msg, key string, value any) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lines {
		if l.msg == msg && fmt.Sprint(l.extra[key]) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

// ---- the golden vectors (testdata/vectors, written by the Go codec)

type txVector struct {
	Name          string `json:"name"`
	Txid          string `json:"txid"`
	Beef          string `json:"beef"`
	PreviousCoins []struct {
		InputIndex int    `json:"inputIndex"`
		Txid       string `json:"txid"`
		Vout       uint32 `json:"vout"`
	} `json:"previousCoins"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason"`
	MaxSweepInputs int    `json:"maxSweepInputs"`
	Admits         *struct {
		Kind           string   `json:"kind"`
		OutputsToAdmit []uint32 `json:"outputsToAdmit"`
		CoinsToRetain  []uint32 `json:"coinsToRetain"`
		Commitment     string   `json:"commitment"`
		Record         string   `json:"record"`
		Spent          []string `json:"spent"`
	} `json:"admits"`
}

type txVectors struct {
	Hall           string `json:"hall"`
	Topic          string `json:"topic"`
	MaxBeef        int    `json:"maxBeef"`
	MaxSweepInputs int    `json:"maxSweepInputs"`
	Headers        []struct {
		Height     uint32 `json:"height"`
		MerkleRoot string `json:"merkleRoot"`
	} `json:"headers"`
	Cases  []txVector `json:"cases"`
	Stored []struct {
		Name       string `json:"name"`
		Txid       string `json:"txid"`
		Stored     string `json:"stored"`
		ReplayBeef string `json:"replayBeef"`
	} `json:"stored"`
}

type refusalVector struct {
	Record string `json:"record"`
	Label  string `json:"label"`
	Hex    string `json:"hex"`
	Reason string `json:"reason"`
	Stage  string `json:"stage"`
}

func readVectors(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../testdata/vectors/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

var (
	vectorsOnce sync.Once
	vectorsVal  *txVectors
)

func loadTxVectors(t *testing.T) *txVectors {
	t.Helper()
	vectorsOnce.Do(func() {
		v := &txVectors{}
		readVectors(t, "transaction-v1.json", v)
		vectorsVal = v
	})
	return vectorsVal
}

func loadRefusalVectors(t *testing.T) []refusalVector {
	t.Helper()
	var v struct {
		Refusals []refusalVector `json:"refusals"`
	}
	readVectors(t, "refusal-v1.json", &v)
	return v.Refusals
}

func (v *txVectors) byName(t *testing.T, name string) *txVector {
	t.Helper()
	for i := range v.Cases {
		if v.Cases[i].Name == name {
			return &v.Cases[i]
		}
	}
	t.Fatalf("no transaction vector %s", name)
	return nil
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (x *txVector) beef(t *testing.T) []byte { return unhex(t, x.Beef) }

func (x *txVector) held() []int {
	out := []int{}
	for _, c := range x.PreviousCoins {
		out = append(out, c.InputIndex)
	}
	return out
}

// ---- a local chain: blocks of two, each a minted transaction beside a fixed sibling

type testChain struct {
	mu     sync.Mutex
	height uint32
	roots  map[uint32]chainhash.Hash
}

func newTestChain(first uint32) *testChain {
	return &testChain{height: first, roots: map[uint32]chainhash.Hash{}}
}

// withVectors adds the golden vectors' headers.
func (c *testChain) withVectors(t *testing.T) *testChain {
	t.Helper()
	v := loadTxVectors(t)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range v.Headers {
		r, err := chainhash.NewHashFromHex(h.MerkleRoot)
		if err != nil {
			t.Fatal(err)
		}
		c.roots[h.Height] = *r
	}
	return c
}

func (c *testChain) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.roots[height]
	return ok && r.Equal(*root), nil
}

func (c *testChain) CurrentHeight(context.Context) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var top uint32
	for h := range c.roots {
		if h > top {
			top = h
		}
	}
	return top, nil
}

func (c *testChain) root(h uint32) (chainhash.Hash, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.roots[h]
	return r, ok
}

func (c *testChain) setRoot(h uint32, r chainhash.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roots[h] = r
}

func (c *testChain) dropRoot(h uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.roots, h)
}

func fill(b byte) chainhash.Hash {
	var h chainhash.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func yes() *bool { b := true; return &b }

// mine gives tx a proof in a new block and returns it.
func (c *testChain) mine(t *testing.T, tx *transaction.Transaction) *transaction.Transaction {
	t.Helper()
	c.mu.Lock()
	h := c.height
	c.height++
	c.mu.Unlock()
	txid := tx.TxID()
	sib := fill(0x55)
	tx.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{{
		{Offset: 0, Hash: &sib},
		{Offset: 1, Hash: txid, Txid: yes()},
	}})
	root, err := tx.MerklePath.ComputeRoot(txid)
	if err != nil {
		t.Fatal(err)
	}
	c.setRoot(h, *root)
	return tx
}

// mineTogether puts a and b in one new block of four, at offsets 1 and 2,
// each with its own minimal path.
func (c *testChain) mineTogether(t *testing.T, a, b *transaction.Transaction) {
	t.Helper()
	c.mu.Lock()
	h := c.height
	c.height++
	c.mu.Unlock()
	s0, s3 := fill(0x66), fill(0x77)
	ida, idb := a.TxID(), b.TxID()
	a.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{
		{{Offset: 0, Hash: &s0}, {Offset: 1, Hash: ida, Txid: yes()}},
		{{Offset: 1, Hash: transaction.MerkleTreeParent(idb, &s3)}},
	})
	b.MerklePath = transaction.NewMerklePath(h, [][]*transaction.PathElement{
		{{Offset: 2, Hash: idb, Txid: yes()}, {Offset: 3, Hash: &s3}},
		{{Offset: 0, Hash: transaction.MerkleTreeParent(&s0, ida)}},
	})
	root, err := a.MerklePath.ComputeRoot(ida)
	if err != nil {
		t.Fatal(err)
	}
	c.setRoot(h, *root)
}

// ---- parties

func sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// party is one identity: its key, its wallet, and its compressed public
// key.
type party struct {
	label string
	key   *ec.PrivateKey
	id    []byte
	hex   string
	w     *wallet.CompletedProtoWallet
}

func newParty(label string) *party { return partyFromKey(label, sha("borg/test/party/"+label)) }

func partyFromKey(label string, k []byte) *party {
	priv, _ := ec.PrivateKeyFromBytes(k)
	w, err := wallet.NewCompletedProtoWallet(priv)
	if err != nil {
		panic(err)
	}
	id := priv.PubKey().Compressed()
	return &party{label: label, key: priv, id: id, hex: hex.EncodeToString(id), w: w}
}

// derived is the key a reader derives for this party's outputs under d.
func (p *party) derived(d pushdrop.Derivation) *ec.PublicKey {
	k, err := d.ExpectedLockingKey(p.key.PubKey())
	if err != nil {
		panic(err)
	}
	return k
}

var nowhere struct {
	mu sync.Mutex
	n  int
}

// fromNowhere is a transaction of the given outputs whose one unsigned
// input spends an outpoint no one holds.
func fromNowhere(outs ...*transaction.TransactionOutput) *transaction.Transaction {
	nowhere.mu.Lock()
	n := nowhere.n
	nowhere.n++
	nowhere.mu.Unlock()
	src, _ := chainhash.NewHash(sha(fmt.Sprintf("borg/test/nowhere/%d/%d", n, time.Now().UnixNano())))
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{SourceTXID: src, SourceTxOutIndex: 0, UnlockingScript: &script.Script{}, SequenceNumber: transaction.MaxTxInSequenceNum})
	for _, o := range outs {
		tx.AddOutput(o)
	}
	return tx
}

func scriptOf(b []byte) *script.Script { s := script.Script(b); return &s }

// fundingTree is a mined funding tree of n outputs of 1000 satoshis locked
// to derive(owner, d).
func fundingTree(t *testing.T, owner *party, d pushdrop.Derivation, n int, chain *testChain) *transaction.Transaction {
	t.Helper()
	lock := borgrec.FundingScript(owner.derived(d))
	outs := make([]*transaction.TransactionOutput, n)
	for i := range outs {
		outs[i] = &transaction.TransactionOutput{Satoshis: 1000, LockingScript: scriptOf(lock)}
	}
	return chain.mine(t, fromNowhere(outs...))
}

// mintCarrier is a carrier of record spending output vout of owner's tree,
// locked and signed under d.
func mintCarrier(t *testing.T, tree *transaction.Transaction, vout uint32, owner *party, d pushdrop.Derivation, record []byte, lockTime uint32) *transaction.Transaction {
	t.Helper()
	ctx := context.Background()
	lock, err := d.Lock(ctx, owner.w, "", [][]byte{record}, true)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	tx.LockTime = lockTime
	tx.AddInputFromTx(tree, vout, d.Unlocker(ctx, owner.w, ""))
	tx.Inputs[0].SequenceNumber = 0
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: tree.Outputs[vout].Satoshis, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

// mintIn is one input of a minted token: an output of src, signed under d.
type mintIn struct {
	src  *transaction.Transaction
	vout uint32
	d    pushdrop.Derivation
}

// mintToken is an unmined chain token: output 0 the signed two-field
// PushDrop of tag and record under d, 1 satoshi; each input spends one of
// inputs with the owner's key under its derivation; what the inputs hold
// beyond 1 satoshi goes to change.
func mintToken(t *testing.T, owner *party, tag, record []byte, d pushdrop.Derivation, inputs ...mintIn) *transaction.Transaction {
	t.Helper()
	ctx := context.Background()
	lock, err := d.Lock(ctx, owner.w, "", [][]byte{tag, record}, true)
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	var total uint64
	for _, in := range inputs {
		tx.AddInputFromTx(in.src, in.vout, in.d.Unlocker(ctx, owner.w, ""))
		total += in.src.Outputs[in.vout].Satoshis
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	if total > 1 {
		addr, _ := script.NewAddressFromPublicKey(owner.key.PubKey(), false)
		change, _ := p2pkh.Lock(addr)
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: total - 1, LockingScript: change})
	}
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx
}

// mintSweep is a mined sweep of outputs vouts of owner's tree under d: its
// tombstone, funding-shaped, at output 0.
func mintSweep(t *testing.T, tree *transaction.Transaction, vouts []uint32, owner *party, d pushdrop.Derivation, chain *testChain) *transaction.Transaction {
	t.Helper()
	ctx := context.Background()
	tx := transaction.NewTransaction()
	var value uint64
	for _, v := range vouts {
		tx.AddInputFromTx(tree, v, d.Unlocker(ctx, owner.w, ""))
		value += tree.Outputs[v].Satoshis
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: value, LockingScript: scriptOf(borgrec.FundingScript(owner.derived(d)))})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return chain.mine(t, tx)
}

// atomicOf is a transaction's Atomic BEEF, as a publisher submits a
// carrier or a sweep.
func atomicOf(t *testing.T, tx *transaction.Transaction) []byte {
	t.Helper()
	b, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tokenBEEF is a token's BEEF with its parent (spec section 11.1).
func tokenBEEF(t *testing.T, token, parent *transaction.Transaction) []byte {
	t.Helper()
	b, err := borgrec.TokenBEEF(token, parent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// commitment is C = txid(K), hash byte order.
func commitment(tx *transaction.Transaction) [32]byte { return [32]byte(*tx.TxID()) }

func cOf(tx *transaction.Transaction) string {
	c := commitment(tx)
	return hex.EncodeToString(c[:])
}

func cHex(c [32]byte) string { return hex.EncodeToString(c[:]) }

func idOf(tx *transaction.Transaction) string { return tx.TxID().String() }

// byTxid are the txids of txs, sorted.
func byTxid(txs ...*transaction.Transaction) []string {
	out := make([]string, 0, len(txs))
	for _, tx := range txs {
		out = append(out, idOf(tx))
	}
	sort.Strings(out)
	return out
}

func ids(txs ...*transaction.Transaction) []string {
	out := make([]string, 0, len(txs))
	for _, tx := range txs {
		out = append(out, idOf(tx))
	}
	return out
}

// byC sorts transactions by commitment, lowest first.
func byC(txs []*transaction.Transaction) []*transaction.Transaction {
	out := append([]*transaction.Transaction(nil), txs...)
	sort.Slice(out, func(i, j int) bool { return cOf(out[i]) < cOf(out[j]) })
	return out
}

func random32(t *testing.T) [32]byte {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return b
}

// grantValues are a grant's fields in the clear.
type grantValues struct{ expiry, role, scope string }

var defaultValues = grantValues{expiry: "2099-12-31T23:59:59Z", role: "member", scope: "member"}

// issueGrant is a grant from org to subject whose revocation outpoint is
// tree:vout, issued and signed by the Go core.
func issueGrant(t *testing.T, hall string, org, subject *party, tree *transaction.Transaction, vout uint32, values grantValues, reveal bool) *borgrec.Grant {
	t.Helper()
	spec := borgrec.GrantSpec{Hall: hall, Subject: subject.key.PubKey(), Serial: random32(t), RevTxid: commitment(tree), RevVout: vout,
		Values:    map[string]string{"expiry": values.expiry, "role": values.role, "scope": values.scope},
		FieldKeys: map[string][32]byte{}, FieldIVs: map[string][32]byte{}, KeyringIVs: map[string][32]byte{}, Reveal: reveal}
	for _, n := range borgrec.GrantFieldNames {
		spec.FieldKeys[n], spec.FieldIVs[n], spec.KeyringIVs[n] = random32(t), random32(t), random32(t)
	}
	g, err := borgrec.BuildGrant(context.Background(), org.key, spec)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func encode(t *testing.T, r interface{ Encode() ([]byte, error) }) []byte {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wrapEntry is one release of k to member, sealed by org (spec section
// 6.3).
func wrapEntry(t *testing.T, org *party, member []byte, epochID [32]byte, epoch uint64, k [32]byte) []byte {
	t.Helper()
	pub, err := ec.PublicKeyFromBytes(member)
	if err != nil {
		t.Fatal(err)
	}
	b, err := borgrec.WrapWithIV(org.key, pub, epochID, uint32(epoch), k, random32(t)) //nolint:gosec // a test epoch
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// epochKey is a fresh epoch key: a scalar in [1, n-1].
func epochKey(t *testing.T) [32]byte {
	t.Helper()
	k, err := borgrec.SampleKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const t0 = 1798761600

func sortKeys(keys [][]byte) [][]byte {
	out := append([][]byte(nil), keys...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

func sort32(cs [][32]byte) [][32]byte {
	out := append([][32]byte(nil), cs...)
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i][:], out[j][:]) < 0 })
	return out
}

func without32(cs [][32]byte, drop [][32]byte) [][32]byte {
	var out [][32]byte
	for _, c := range cs {
		keep := true
		for _, d := range drop {
			keep = keep && c != d
		}
		if keep {
			out = append(out, c)
		}
	}
	return out
}

func withoutKeys(keys [][]byte, drop [][]byte) [][]byte {
	var out [][]byte
	for _, k := range keys {
		keep := true
		for _, d := range drop {
			keep = keep && !bytes.Equal(k, d)
		}
		if keep {
			out = append(out, k)
		}
	}
	return out
}

// minted is a mined token and the BEEF it is published in.
type minted struct {
	tx     *transaction.Transaction
	beef   []byte
	org    *borgrec.Org
	grp    *borgrec.Group
	record []byte
	parent *transaction.Transaction
}

// grantMade is a grant carrier and what it carries.
type grantMade struct {
	tx    *transaction.Transaction
	c     [32]byte
	grant *borgrec.Grant
	vout  uint32
}

// orgSim is a minimal organization's tooling over a local chain: its
// trees, its roll, its policy set and its groups. Each token is mined in a
// block of its own and published with its parent.
type orgSim struct {
	t                                         *testing.T
	hall                                      string
	party                                     *party
	chain                                     *testChain
	opaque                                    bool
	grantTree, policyTree, recordTree, chainT *transaction.Transaction
	grantAt, policyAt, recordAt, chainAt      uint32
	tip                                       *minted
	history                                   []*minted
	roll, policy                              [][32]byte
}

type orgOpts struct {
	opaque bool
	trees  int
	key    []byte
}

func newOrgSim(t *testing.T, hall, label string, chain *testChain, o orgOpts) *orgSim {
	t.Helper()
	p := newParty(label)
	if o.key != nil {
		p = partyFromKey(label, o.key)
	}
	n := o.trees
	if n == 0 {
		n = 32
	}
	return &orgSim{t: t, hall: hall, party: p, chain: chain, opaque: o.opaque,
		grantTree:  fundingTree(t, p, borgrec.GrantDerivation, n, chain),
		policyTree: fundingTree(t, p, borgrec.PolicyDerivation, n, chain),
		recordTree: fundingTree(t, p, borgrec.RecordDerivation, n, chain),
		chainT:     fundingTree(t, p, borgrec.ChainDerivation, 8, chain)}
}

func (o *orgSim) hex() string { return o.party.hex }

func (o *orgSim) nextGrantVout() uint32  { v := o.grantAt; o.grantAt++; return v }
func (o *orgSim) nextRecordVout() uint32 { v := o.recordAt; o.recordAt++; return v }
func (o *orgSim) nextPolicyVout() uint32 { v := o.policyAt; o.policyAt++; return v }
func (o *orgSim) nextChainVout() uint32  { v := o.chainAt; o.chainAt++; return v }

// grant is a grant carrier to subject on the next grant-tree output.
func (o *orgSim) grant(subject *party) *grantMade {
	return o.grantWith(subject, defaultValues, o.nextGrantVout(), carrier.LockTime)
}

func (o *orgSim) grantWith(subject *party, values grantValues, vout, lockTime uint32) *grantMade {
	t := o.t
	g := issueGrant(t, o.hall, o.party, subject, o.grantTree, vout, values, !o.opaque)
	tx := mintCarrier(t, o.grantTree, vout, o.party, borgrec.GrantDerivation, encode(t, g), lockTime)
	return &grantMade{tx: tx, c: commitment(tx), grant: g, vout: vout}
}

// policyMade is a policy carrier and its record.
type policyMade struct {
	tx  *transaction.Transaction
	c   [32]byte
	rec *borgrec.Policy
}

type policyOpts struct {
	to          []byte
	permissions []string
	expiry      string
	vout        *uint32
}

// policyRecord is a policy carrier on the next policy-tree output: a role
// definition by default, or a binding where to is given.
func (o *orgSim) policyRecord(role string, po policyOpts) *policyMade {
	t := o.t
	vout := o.nextPolicyVout()
	if po.vout != nil {
		vout = *po.vout
	}
	kind := borgrec.PolicyRole
	if po.to != nil {
		kind = borgrec.PolicyBinding
	}
	perms := po.permissions
	if perms == nil {
		perms = []string{}
	}
	rec := &borgrec.Policy{Hall: o.hall, Org: o.party.id, Kind: kind, Role: role, Permissions: perms, To: po.to, Expiry: po.expiry, Created: t0}
	tx := mintCarrier(t, o.policyTree, vout, o.party, borgrec.PolicyDerivation, encode(t, rec), carrier.LockTime)
	return &policyMade{tx: tx, c: commitment(tx), rec: rec}
}

type tokenOpts struct {
	mutate         func(r *borgrec.Org)
	unmined        bool
	define, retire [][32]byte
}

// orgToken is the next organization token, admitting and revoking
// commitments, mined. mutate may change the record before it is encoded
// (to build what a host must refuse or the index must flag); the roll kept
// here follows the change lists as given.
func (o *orgSim) orgToken(admit, revoke [][32]byte, to tokenOpts) *minted {
	t := o.t
	roll := sort32(append(without32(o.roll, revoke), admit...))
	policy := sort32(append(without32(o.policy, to.retire), to.define...))
	var seq uint64
	var prev [32]byte
	if o.tip != nil {
		seq, prev = o.tip.org.Seq+1, commitment(o.tip.tx)
	}
	rec := &borgrec.Org{Hall: o.hall, Org: o.party.id, Seq: seq, Prev: prev, Name: "Example Organization", Admit: sort32(admit), Revoke: sort32(revoke),
		Count: uint64(len(roll)), Roll: borgrec.RollRoot(roll), Opaque: o.opaque, Created: t0 + seq*600, Define: sort32(to.define), Retire: sort32(to.retire),
		PolicyCount: uint64(len(policy)), Policy: borgrec.PolicyRoot(policy)}
	if rec.Admit == nil {
		rec.Admit = [][32]byte{}
	}
	if rec.Revoke == nil {
		rec.Revoke = [][32]byte{}
	}
	if rec.Define == nil {
		rec.Define = [][32]byte{}
	}
	if rec.Retire == nil {
		rec.Retire = [][32]byte{}
	}
	if to.mutate != nil {
		to.mutate(rec)
	}
	record := encode(t, rec)
	var in mintIn
	if o.tip != nil {
		in = mintIn{src: o.tip.tx, vout: 0, d: borgrec.OrgDerivation}
	} else {
		in = mintIn{src: o.chainT, vout: o.nextChainVout(), d: borgrec.ChainDerivation}
	}
	tx := mintToken(t, o.party, borgrec.TagOrg, record, borgrec.OrgDerivation, in)
	m := &minted{tx: tx, org: rec, record: record, parent: in.src}
	if !to.unmined {
		o.chain.mine(t, tx)
		m.beef = tokenBEEF(t, tx, in.src)
	}
	o.tip = m
	o.history = append(o.history, m)
	o.roll, o.policy = roll, policy
	return m
}

// group is a new group of members at epoch 0, mined, with its id drawn at
// random.
func (o *orgSim) group(name string, members []*party, rotateOnJoin bool) *groupSim {
	var id [borgrec.GroupIDLen]byte
	if _, err := rand.Read(id[:]); err != nil {
		o.t.Fatal(err)
	}
	g := &groupSim{org: o, name: name, id: id, rotateOnJoin: rotateOnJoin, epochs: map[uint64]epochKeyID{}}
	g.mint(keysOf(members), nil, true, nil, nil)
	return g
}

func keysOf(ps []*party) [][]byte {
	out := make([][]byte, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.id)
	}
	return out
}

type epochKeyID struct {
	key     [32]byte
	epochID [32]byte
}

// groupSim is one group of an orgSim: its chain, members, the keys removed
// from it, and every epoch's key.
type groupSim struct {
	org          *orgSim
	name         string
	id           [borgrec.GroupIDLen]byte
	rotateOnJoin bool
	tip          *minted
	history      []*minted
	members      [][]byte
	removed      [][]byte
	epochs       map[uint64]epochKeyID
}

func (g *groupSim) idHex() string { return hex.EncodeToString(g.id[:]) }
func (g *groupSim) epoch() uint64 { return g.tip.grp.Epoch }

type changeOpts struct {
	add, remove []*party
	rotate      bool
	keepEpoch   bool
	reuse       *uint64
	mutate      func(r *borgrec.Group)
}

// change is the next transition. rotate begins an epoch with a fresh key;
// it is forced when anything is removed unless keepEpoch is set (to build
// what a host must refuse). reuse begins an epoch under an earlier epoch's
// key.
func (g *groupSim) change(co changeOpts) *minted {
	add, remove := keysOf(co.add), keysOf(co.remove)
	must := len(remove) > 0 || (g.tip.grp.RotateOnJoin && len(add) > 0)
	rotate := !co.keepEpoch && (co.rotate || must || co.reuse != nil)
	return g.mint(add, remove, rotate, co.reuse, co.mutate)
}

func (g *groupSim) mint(add, remove [][]byte, rotate bool, reuse *uint64, mutate func(*borgrec.Group)) *minted {
	t := g.org.t
	prev := g.tip
	if len(g.epochs) == 0 {
		prev = nil
	}
	var seq, epoch uint64
	var prevID [32]byte
	if prev != nil {
		seq, prevID, epoch = prev.grp.Seq+1, commitment(prev.tx), prev.grp.Epoch
		if rotate {
			epoch++
		}
	}
	members := sortKeys(append(withoutKeys(g.members, remove), add...))
	var key [32]byte
	switch {
	case prev == nil || rotate:
		if reuse != nil {
			key = g.epochs[*reuse].key
		} else {
			key = epochKey(t)
		}
	default:
		key = g.epochs[prev.grp.Epoch].key
	}
	epochSeq := seq
	if prev != nil && !rotate {
		epochSeq = prev.grp.EpochSeq
	}
	addS, removeS := sortKeys(add), sortKeys(remove)
	if addS == nil {
		addS = [][]byte{}
	}
	if removeS == nil {
		removeS = [][]byte{}
	}
	rec := &borgrec.Group{Hall: g.org.hall, Org: g.org.party.id, Group: g.id, Seq: seq, Prev: prevID, Name: g.name, Epoch: epoch, EpochSeq: epochSeq,
		Commitment: borgrec.Commitment(key), Add: addS, Remove: removeS, Count: uint64(len(members)), Members: borgrec.MembersRoot(members),
		RotateOnJoin: g.rotateOnJoin, Created: t0 + 60 + seq*600}
	if mutate != nil {
		mutate(rec)
	}
	record := encode(t, rec)
	var in mintIn
	if prev != nil {
		in = mintIn{src: prev.tx, vout: 0, d: borgrec.GroupDerivation}
	} else {
		in = mintIn{src: g.org.chainT, vout: g.org.nextChainVout(), d: borgrec.ChainDerivation}
	}
	tx := mintToken(t, g.org.party, borgrec.TagGroup, record, borgrec.GroupDerivation, in)
	g.org.chain.mine(t, tx)
	m := &minted{tx: tx, beef: tokenBEEF(t, tx, in.src), grp: rec, record: record, parent: in.src}
	if prev == nil || rotate {
		g.epochs[epoch] = epochKeyID{key: key, epochID: borgrec.EpochID(record)}
	}
	g.tip = m
	g.history = append(g.history, m)
	for _, k := range remove {
		if len(withoutKeys(g.removed, [][]byte{k})) == len(g.removed) {
			g.removed = append(g.removed, k)
		}
	}
	g.members = members
	return m
}

// wraps are wrap carriers of the current epoch's key to recipients (every
// member when nil), at most per entries each.
func (g *groupSim) wraps(recipients [][]byte, per int) []*transaction.Transaction {
	t := g.org.t
	if recipients == nil {
		recipients = g.members
	}
	if per == 0 {
		per = 16
	}
	ek := g.epochs[g.epoch()]
	sorted := sortKeys(recipients)
	var out []*transaction.Transaction
	for i := 0; i < len(sorted); i += per {
		end := i + per
		if end > len(sorted) {
			end = len(sorted)
		}
		var entries [][]byte
		for _, m := range sorted[i:end] {
			entries = append(entries, wrapEntry(t, g.org.party, m, ek.epochID, g.epoch(), ek.key))
		}
		rec := &borgrec.WrapRecord{Hall: g.org.hall, Org: g.org.party.id, Group: g.id, Epoch: g.epoch(), EpochID: ek.epochID, Entries: entries}
		out = append(out, mintCarrier(t, g.org.recordTree, g.org.nextRecordVout(), g.org.party, borgrec.RecordDerivation, encode(t, rec), carrier.LockTime))
	}
	return out
}

// ---- the engine's storage, in memory

// memStorage is the engine's storage in memory, as the Go engine's store
// behaves where the module depends on it: FindOutput matches spent and
// unspent outputs, the unspent rows are what a restore is handed, and
// DeleteOutput deletes one output and leaves the applied transactions.
type memStorage struct {
	mu      sync.Mutex
	outputs map[string]*bmodule.Output
	applied map[string]bool
	n       int
	// meanwhile, when set, runs once at the first FindOutput of the txid
	// meanwhileTxid, before it answers.
	meanwhile     func()
	meanwhileTxid string
}

func newMemStorage() *memStorage {
	return &memStorage{outputs: map[string]*bmodule.Output{}, applied: map[string]bool{}}
}

func skey(topic string, op *transaction.Outpoint) string { return topic + "|" + op.String() }

func (m *memStorage) FindOutput(_ context.Context, op *transaction.Outpoint, topic string) (*bmodule.Output, error) {
	m.mu.Lock()
	var f func()
	if m.meanwhile != nil && (m.meanwhileTxid == "" || m.meanwhileTxid == op.Txid.String()) {
		f = m.meanwhile
		m.meanwhile = nil
	}
	m.mu.Unlock()
	if f != nil {
		f()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.outputs))
	for k := range m.outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o := m.outputs[k]
		if o.Outpoint == *op && (topic == "" || o.Topic == topic) {
			c := *o
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memStorage) DeleteOutput(_ context.Context, op *transaction.Outpoint, topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.outputs, skey(topic, op))
	return nil
}

// find is an output of topic, spent or not; nil when the store does not
// hold it.
func (m *memStorage) find(txid string, vout uint32, topic string) *bmodule.Output {
	h, _ := chainhash.NewHashFromHex(txid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if o := m.outputs[skey(topic, &transaction.Outpoint{Txid: *h, Index: vout})]; o != nil {
		c := *o
		return &c
	}
	return nil
}

func (m *memStorage) isApplied(txid, topic string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied[topic+"|"+txid]
}

// utxos are the unspent rows of topic, in the order they were stored, with
// their BEEF or without.
func (m *memStorage) utxos(topic string, beef bool) []bmodule.Output {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []bmodule.Output
	for _, o := range m.outputs {
		if o.Topic == topic && !o.Spent {
			c := *o
			if !beef {
				c.Beef = nil
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out
}

// cannotDelete is a store with no DeleteOutput.
type cannotDelete struct{ m *memStorage }

func (c cannotDelete) FindOutput(ctx context.Context, op *transaction.Outpoint, topic string) (*bmodule.Output, error) {
	return c.m.FindOutput(ctx, op, topic)
}

// ---- the module and the engine's admission path

var (
	hall   string
	topic  string
	hall2  = "other_hall_abcdefghij"
	topic2 = borgrec.TopicPrefix + "other_hall_abcdefghij"
)

// The vectors' hall, which every test's host carries.
func init() {
	b, err := os.ReadFile("../testdata/vectors/transaction-v1.json")
	if err != nil {
		panic(err)
	}
	var v struct {
		Hall  string `json:"hall"`
		Topic string `json:"topic"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		panic(err)
	}
	hall, topic = v.Hall, v.Topic
}

func vectorHall(t *testing.T) { t.Helper() }

// baseConfig is the configuration the tests use.
func baseConfig(halls ...string) *Config {
	if len(halls) == 0 {
		halls = []string{hall}
	}
	return &Config{Halls: halls, StateDir: "/nonexistent", HeadersURL: "http://127.0.0.1:1", RetentionDays: 31, MaxBEEF: borgrec.DefaultMaxBEEF,
		MaxSweepInputs: borgrec.DefaultMaxSweepInputs, SpendTable: MinSpendTable, Prices: map[string]uint64{},
		Sessions: termsroute.SessionBound{Max: 100, TTL: 600 * time.Second}, Budget: termsroute.DefaultBudget, Response: termsroute.DefaultResponseBudget}
}

// clock is a clock the tests move, in seconds.
type clock struct {
	mu  sync.Mutex
	now int64
}

func (c *clock) time() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(c.now, 0)
}

func (c *clock) add(s int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += s
}

// engine is the module and the engine's admission path over a store.
type engine struct {
	t       *testing.T
	host    *fakeHost
	m       *Mounted
	ls      *Service
	storage *memStorage
	journal *MemoryJournal
	chain   *testChain
	clock   *clock
	// naive returns every refusal as empty instructions, as a manager that
	// does not raise would; calls counts the managers' calls.
	naive bool
	calls int
	// noSPV skips the engine's SPV (scripts and proofs against chain),
	// which otherwise runs before any manager.
	noSPV bool
}

type engineOpts struct {
	naive      bool
	noSPV      bool
	journal    *MemoryJournal
	chain      *testChain
	storage    *memStorage
	config     *Config
	spendTable int
	now        int64
}

func setup(t *testing.T, o engineOpts) *engine {
	t.Helper()
	vectorHall(t)
	host := newFakeHost()
	if o.chain == nil {
		o.chain = newTestChain(70000).withVectors(t)
	}
	if o.journal == nil {
		o.journal = &MemoryJournal{}
	}
	if o.storage == nil {
		o.storage = newMemStorage()
	}
	if o.config == nil {
		o.config = baseConfig()
	}
	if o.spendTable != 0 {
		o.config.SpendTable = o.spendTable
	}
	cl := &clock{now: o.now}
	if cl.now == 0 {
		cl.now = time.Now().Unix()
	}
	var topics []string
	for _, h := range o.config.Halls {
		topics = append(topics, borgrec.Topic(h))
	}
	m, err := Mount(host, o.config, topics, o.journal, o.chain, cl.time)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), o.storage.utxos(topic, true), o.storage); err != nil {
		t.Fatal(err)
	}
	return &engine{t: t, host: host, m: m, ls: m.LS, storage: o.storage, journal: o.journal, chain: o.chain, clock: cl, naive: o.naive, noSPV: o.noSPV}
}

// restored is a fresh module over the engine's journal and storage,
// restored from the unspent rows as a host restart does.
func (e *engine) restored(journal *MemoryJournal) *Service {
	e.t.Helper()
	if journal == nil {
		journal = e.journal
	}
	m, err := Mount(newFakeHost(), e.m.config, []string{topic}, journal, e.chain, e.clock.time)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := m.LS.Restore(context.Background(), e.storage.utxos(topic, true), e.storage); err != nil {
		e.t.Fatal(err)
	}
	return m.LS
}

// errSPV is the engine's own SPV refusing a submission before any manager
// runs.
type errSPV struct{ err error }

func (e errSPV) Error() string { return fmt.Sprintf("the engine's SPV refused it: %v", e.err) }

// admit is the engine's admission path: the outputs admitted, or an error
// when the engine or the manager refused. A transaction the topic applied
// is a duplicate the manager is never shown. The order is the Go engine's:
// outputs stored, held outputs it spends marked spent (retained) or
// deleted, the lookup services told, then the applied transaction
// recorded.
func (e *engine) admit(raw []byte, t string) ([]uint32, error) {
	ctx := bmodule.WithSubmission(context.Background(), raw)
	beef, tx, txid, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, errSPV{fmt.Errorf("the BEEF: %v", err)}
	}
	st := e.storage
	if st.isApplied(txid.String(), t) {
		return []uint32{}, nil
	}
	if !e.noSPV {
		if ok, err := spv.Verify(context.Background(), tx, e.chain, nil); err != nil || !ok {
			return nil, errSPV{err}
		}
	}
	tm := e.m.Module.Topics[t]
	if tm == nil {
		return nil, fmt.Errorf("no manager for %s", t)
	}
	st.mu.Lock()
	var coins []uint32
	held := map[uint32]*bmodule.Output{}
	for i, in := range tx.Inputs {
		if in.SourceTXID == nil {
			continue
		}
		if o := st.outputs[skey(t, &transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex})]; o != nil && !o.Spent {
			coins = append(coins, uint32(i)) //nolint:gosec // test
			held[uint32(i)] = o              //nolint:gosec // test
		}
	}
	st.mu.Unlock()
	e.calls++
	ai, err := tm.IdentifyAdmissibleOutputs(ctx, beef, txid, coins)
	if err != nil {
		if _, refused := err.(*Refused); !refused || !e.naive {
			return nil, err
		}
		ai.OutputsToAdmit, ai.CoinsToRetain = nil, coins
	}
	atomicBytes, err := beef.AtomicBytes(txid)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	retain := map[uint32]bool{}
	for _, c := range ai.CoinsToRetain {
		retain[c] = true
	}
	for vin, o := range held {
		if retain[vin] {
			o.Spent = true
			o.ConsumedBy = append(o.ConsumedBy, &transaction.Outpoint{Txid: *txid, Index: 0})
		} else {
			delete(st.outputs, skey(t, &o.Outpoint))
		}
	}
	for _, vout := range ai.OutputsToAdmit {
		op := transaction.Outpoint{Txid: *txid, Index: vout}
		st.n++
		st.outputs[skey(t, &op)] = &bmodule.Output{Outpoint: op, Topic: t, Beef: atomicBytes, Score: float64(st.n),
			OutputScript: tx.Outputs[vout].LockingScript, Satoshis: tx.Outputs[vout].Satoshis}
	}
	st.mu.Unlock()
	for _, vout := range ai.OutputsToAdmit {
		if err := e.ls.OutputAdmittedByTopic(ctx, &bmodule.OutputAdmittedByTopic{Mode: bmodule.AdmitWholeTx, Outpoint: transaction.Outpoint{Txid: *txid, Index: vout},
			Topic: t, AtomicBEEF: atomicBytes}); err != nil {
			return nil, err
		}
	}
	st.mu.Lock()
	st.applied[t+"|"+txid.String()] = true
	st.mu.Unlock()
	if ai.OutputsToAdmit == nil {
		return []uint32{}, nil
	}
	return ai.OutputsToAdmit, nil
}

// submit submits to the vectors' hall: the outputs admitted, empty when the
// manager refused, nil when the engine's own SPV did.
func (e *engine) submit(raw []byte) []uint32 {
	e.t.Helper()
	got, err := e.admit(raw, topic)
	if err != nil {
		if _, spvRefused := err.(errSPV); spvRefused {
			return nil
		}
		return []uint32{}
	}
	return got
}

func (e *engine) submitTx(tx *transaction.Transaction) []uint32 {
	e.t.Helper()
	return e.submit(atomicOf(e.t, tx))
}

func question(q any) *lookup.LookupQuestion {
	b, _ := json.Marshal(q)
	return &lookup.LookupQuestion{Service: borgrec.LookupService, Query: b}
}

// answerOf asks ls a question as the engine does and reads the subject
// txid of each output it hydrated.
func answerOf(t *testing.T, ls *Service, q any) []string {
	t.Helper()
	a, err := ls.Lookup(context.Background(), question(q))
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, o := range a.Outputs {
		_, _, txid, err := guard.ParseBEEF(o.Beef, guard.DefaultBound)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, txid.String())
	}
	return out
}

func (e *engine) answer(q any) []string {
	e.t.Helper()
	return answerOf(e.t, e.ls, q)
}

// contexts are the contexts of an answer's outputs, in order.
func contexts(t *testing.T, ls *Service, q any) [][]byte {
	t.Helper()
	a, err := ls.Lookup(context.Background(), question(q))
	if err != nil {
		t.Fatal(err)
	}
	hs, _ := a.Result.([]bmodule.HydratedOutput)
	out := make([][]byte, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Context)
	}
	return out
}

type qm = map[string]string

func eq(t *testing.T, got, want []string, msg string) {
	t.Helper()
	if got == nil {
		got = []string{}
	}
	if want == nil {
		want = []string{}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s:\n got  %v\n want %v", msg, got, want)
	}
}

func outs(t *testing.T, got []uint32, want ...uint32) {
	t.Helper()
	if want == nil {
		want = []uint32{}
	}
	if got == nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admitted %v, want %v", got, want)
	}
}

func status(t *testing.T, ls *Service, c string) string {
	t.Helper()
	st, _ := ls.Status(c)
	return st
}

func tokenStatus(t *testing.T, ls *Service, txid string) string {
	t.Helper()
	st, _ := ls.TokenStatus(txid)
	return st
}

func u32(v uint32) *uint32 { return &v }
func u64(v uint64) *uint64 { return &v }
