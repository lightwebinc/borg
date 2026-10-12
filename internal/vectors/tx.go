package vectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/util"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/borgrec"
)

// OtherHall is a well-formed hall that is not the vectors' topic.
const OtherHall = "other_hall_qwertyuiop"

// Headers is the test chain's block headers: a merkle root per height. It is
// the chain tracker the transaction vectors are admitted against.
type Headers map[uint32]chainhash.Hash

// IsValidRootForHeight holds root to the one the test chain has at height.
func (h Headers) IsValidRootForHeight(_ context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	want, ok := h[height]
	return ok && root != nil && *root == want, nil
}

// CurrentHeight is the highest height held.
func (h Headers) CurrentHeight(context.Context) (uint32, error) {
	var top uint32
	for k := range h {
		top = max(top, k)
	}
	return top, nil
}

// unproven is a height nothing is mined at: a proof there verifies against no
// header the test chain holds.
const unproven = 999

// chain mines transactions into blocks at successive heights.
type chain struct {
	height  uint32
	headers Headers
}

func fixedHash(b byte) *chainhash.Hash {
	h := chainhash.Hash(bytes.Repeat([]byte{b}, 32))
	return &h
}

// mine proves tx at offset 1 of a block of two beside a fixed sibling.
func (c *chain) mine(tx *transaction.Transaction) *transaction.Transaction {
	c.height++
	t := true
	tx.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{{
		{Offset: 0, Hash: fixedHash(0x33)},
		{Offset: 1, Hash: tx.TxID(), Txid: &t},
	}})
	c.root(tx)
	return tx
}

// mineTogether proves a and b in one block of four, at offsets 1 and 2, each
// with its own minimal path.
func (c *chain) mineTogether(a, b *transaction.Transaction) {
	c.height++
	t := true
	s0, s3 := fixedHash(0x35), fixedHash(0x36)
	left := chainhash.DoubleHashH(append(append([]byte{}, s0[:]...), a.TxID()[:]...))
	right := chainhash.DoubleHashH(append(append([]byte{}, b.TxID()[:]...), s3[:]...))
	a.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{
		{{Offset: 0, Hash: s0}, {Offset: 1, Hash: a.TxID(), Txid: &t}},
		{{Offset: 1, Hash: &right}},
	})
	b.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{
		{{Offset: 2, Hash: b.TxID(), Txid: &t}, {Offset: 3, Hash: s3}},
		{{Offset: 0, Hash: &left}},
	})
	c.root(a)
}

// wide proves tx at offset 2 of a block of four with a path that also holds
// the leaf at offset 0, which the proof does not need.
func (c *chain) wide(tx *transaction.Transaction) *transaction.Transaction {
	c.height++
	l0, l1, l3 := fixedHash(0x30), fixedHash(0x31), fixedHash(0x34)
	pair := chainhash.DoubleHashH(append(append([]byte{}, l0[:]...), l1[:]...))
	t := true
	tx.MerklePath = transaction.NewMerklePath(c.height, [][]*transaction.PathElement{
		{{Offset: 0, Hash: l0}, {Offset: 2, Hash: tx.TxID(), Txid: &t}, {Offset: 3, Hash: l3}},
		{{Offset: 0, Hash: &pair}},
	})
	c.root(tx)
	return tx
}

func (c *chain) root(tx *transaction.Transaction) {
	root, err := tx.MerklePath.ComputeRoot(tx.TxID())
	if err != nil {
		panic(err)
	}
	c.headers[c.height] = *root
}

// actor is one test identity's wallet and keys for transactions.
type actor struct {
	p       Party
	w       wallet.Interface
	fee     *ec.PrivateKey // [1, "borg organisation"] fund, counterparty self
	feeLock *script.Script
}

func newActor(p Party) (*actor, error) {
	a := &actor{p: p, w: p.Wallet()}
	var err error
	d := wallet.NewKeyDeriver(p.Priv)
	if a.fee, err = d.DerivePrivateKey(borgrec.Protocol, borgrec.KeyFund, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}); err != nil {
		return nil, err
	}
	addr, err := script.NewAddressFromPublicKey(a.fee.PubKey(), false)
	if err != nil {
		return nil, err
	}
	a.feeLock, err = p2pkh.Lock(addr)
	return a, err
}

func (a *actor) feeUnlocker() transaction.UnlockingScriptTemplate {
	u, err := p2pkh.Unlock(a.fee, nil)
	if err != nil {
		panic(err)
	}
	return u
}

func (a *actor) unlocker(d pushdrop.Derivation) transaction.UnlockingScriptTemplate {
	return d.Unlocker(context.Background(), a.w, "")
}

func (a *actor) lock(d pushdrop.Derivation, sign bool, fields ...[]byte) *script.Script {
	s, err := d.Lock(context.Background(), a.w, "", fields, sign)
	if err != nil {
		panic(err)
	}
	return s
}

func (a *actor) key(d pushdrop.Derivation) *ec.PublicKey { return mustKey(d, a.p.Priv.PubKey()) }

// derivedPriv is the actor's private key under d, counterparty anyone.
func (a *actor) derivedPriv(d pushdrop.Derivation) *ec.PrivateKey {
	k, err := wallet.NewKeyDeriver(a.p.Priv).DerivePrivateKey(d.Protocol, d.KeyID, pushdrop.Anyone())
	if err != nil {
		panic(err)
	}
	return k
}

func mustKey(d pushdrop.Derivation, identity *ec.PublicKey) *ec.PublicKey {
	k, err := d.ExpectedLockingKey(identity)
	if err != nil {
		panic(err)
	}
	return k
}

func scr(b []byte) *script.Script { s := script.Script(b); return &s }

func out(sats uint64, s *script.Script) *transaction.TransactionOutput {
	return &transaction.TransactionOutput{Satoshis: sats, LockingScript: s}
}

func params(d pushdrop.Derivation) carrier.Params {
	return carrier.Params{Derivation: d, FundingTag: borgrec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }, ErrIdentity: borgrec.ErrIdentity}
}

// txEnv is everything the transaction vectors are built from.
type txEnv struct {
	ctx    context.Context
	o, d   *actor // the organization, and a stranger
	c      *chain
	coin   *transaction.Transaction
	grant  *transaction.Transaction // grant tree
	policy *transaction.Transaction // policy tree
	rec    *transaction.Transaction // record tree, funding wraps
	chain  *transaction.Transaction // chain tree
	misc   *transaction.Transaction // outputs nothing may be funded from

	grants   map[string]*borgrec.Grant
	carriers map[string]*transaction.Transaction // grant carriers by member
	wraps    []*transaction.Transaction          // epoch 0 and epoch 1 wrap carriers
	wrapRecs [][]byte

	org      []*transaction.Transaction // org tokens seq 0, 1
	orgRecs  []*borgrec.Org
	group    []*transaction.Transaction // group tokens seq 0, 1, 2 (1 and 2 in one block)
	groupRec []*borgrec.Group
	sweep    *transaction.Transaction
	held     map[transaction.Outpoint]bool
	coinNext uint32
}

// nextFee hands out the next coin output for a fee input.
func (e *txEnv) nextFee() mint.Input {
	v := e.coinNext
	e.coinNext++
	return mint.Input{Tx: e.coin, Vout: v, Unlocker: e.o.feeUnlocker()}
}

func newTxEnv(s *Scenario) (*txEnv, error) {
	e := &txEnv{ctx: context.Background(), c: &chain{height: 800, headers: Headers{}},
		grants: map[string]*borgrec.Grant{}, carriers: map[string]*transaction.Transaction{}}
	var err error
	if e.o, err = newActor(Org); err != nil {
		return nil, err
	}
	if e.d, err = newActor(Dave); err != nil {
		return nil, err
	}
	return e, e.build(s)
}

// tree mints a funding tree of n outputs of 1000 under d, mined.
func (e *txEnv) tree(d pushdrop.Derivation, n int) (*transaction.Transaction, error) {
	lock, err := carrier.FundingLock(e.ctx, e.o.w, "", params(d))
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(*lock, borgrec.FundingScript(e.o.key(d))) {
		return nil, fmt.Errorf("bcommon's funding lock is not the funding script")
	}
	t, err := mint.FundingTree(lock, n, 1000, e.nextFee(), e.o.feeLock, mint.LegacyFees)
	if err != nil {
		return nil, err
	}
	return e.c.mine(t), nil
}

// token builds a chain token: the lock under d of [tag, record], signed,
// spending prev (the predecessor or a chain-tree output) and a fee coin.
func (e *txEnv) token(d pushdrop.Derivation, tag, record []byte, prev *transaction.Transaction, vout uint32) (*transaction.Transaction, error) {
	return mint.Transition(e.o.lock(d, true, tag, record), 1,
		&mint.Input{Tx: prev, Vout: vout, Unlocker: e.prevUnlocker(prev, vout)}, e.nextFee(), e.o.feeLock, mint.LegacyFees)
}

// prevUnlocker signs for an output of prev: a chain-tree output under chain,
// a token under its kind's derivation.
func (e *txEnv) prevUnlocker(prev *transaction.Transaction, vout uint32) transaction.UnlockingScriptTemplate {
	switch borgrec.Classify(*prev.Outputs[vout].LockingScript) {
	case borgrec.KindOrg:
		return e.o.unlocker(borgrec.OrgDerivation)
	case borgrec.KindGroup:
		return e.o.unlocker(borgrec.GroupDerivation)
	}
	for _, d := range []pushdrop.Derivation{borgrec.ChainDerivation, borgrec.RecordDerivation, borgrec.GrantDerivation} {
		if bytes.Equal(*prev.Outputs[vout].LockingScript, borgrec.FundingScript(e.o.key(d))) {
			return e.o.unlocker(d)
		}
	}
	return e.o.feeUnlocker()
}

func (e *txEnv) build(s *Scenario) error {
	// The coin: sixty-four outputs paying the organization's fund key, from
	// nowhere, mined.
	e.coin = transaction.NewTransaction()
	e.coin.AddInput(&transaction.TransactionInput{SourceTXID: fixedHash(0x11), UnlockingScript: &script.Script{},
		SequenceNumber: transaction.MaxTxInSequenceNum})
	for range 64 {
		e.coin.AddOutput(out(50000, e.o.feeLock))
	}
	e.c.mine(e.coin)
	var err error
	if e.grant, err = e.tree(borgrec.GrantDerivation, 6); err != nil {
		return err
	}
	if e.policy, err = e.tree(borgrec.PolicyDerivation, 4); err != nil {
		return err
	}
	if e.rec, err = e.tree(borgrec.RecordDerivation, 4); err != nil {
		return err
	}
	if e.chain, err = e.tree(borgrec.ChainDerivation, 4); err != nil {
		return err
	}

	// misc: outputs a grant carrier must not be funded from, each spent by
	// one signature: a bare pay-to-public-key under the grant key, the
	// funding tag under the grant key with a field signature, another tag
	// under the grant key, and the funding tag under the organization's
	// chain key.
	e.misc = transaction.NewTransaction()
	e.misc.AddInputFromTx(e.coin, e.nextFee().Vout, e.o.feeUnlocker())
	gk := e.o.key(borgrec.GrantDerivation)
	bare := script.Script(append(append([]byte{33}, gk.Compressed()...), script.OpCHECKSIG))
	e.misc.AddOutput(out(1000, &bare))
	e.misc.AddOutput(out(1000, e.o.lock(borgrec.GrantDerivation, true, borgrec.TagFunding)))
	e.misc.AddOutput(out(1000, e.o.lock(borgrec.GrantDerivation, false, []byte{'b', 'o', 0x04})))
	e.misc.AddOutput(out(1000, scr(borgrec.FundingScript(e.o.key(borgrec.ChainDerivation)))))
	e.misc.AddOutput(out(45500, e.o.feeLock))
	if err := e.misc.Sign(); err != nil {
		return err
	}
	e.c.mine(e.misc)

	// The grants, each on its own grant-tree output, its revocation outpoint
	// that output.
	gp := params(borgrec.GrantDerivation)
	for i, m := range members {
		spec := grantSpec(Org, m, uint32(i), GrantValues[m.Name], true)
		spec.RevTxid = [32]byte(*e.grant.TxID())
		g, err := borgrec.BuildGrant(e.ctx, Org.Priv, spec)
		if err != nil {
			return err
		}
		rec, err := g.Encode()
		if err != nil {
			return err
		}
		k, err := carrier.Mint(e.ctx, e.o.w, "", gp, rec, e.grant, uint32(i))
		if err != nil {
			return err
		}
		e.grants[m.Name], e.carriers[m.Name] = g, k
	}

	// The organization: seq 0 admits the three grants; seq 1 revokes bob's.
	cs := func(names ...string) [][32]byte {
		var out [][32]byte
		for _, n := range names {
			out = append(out, carrier.Commitment(e.carriers[n]))
		}
		return sorted32(out...)
	}
	o0 := &borgrec.Org{Hall: Hall, Org: Org.Pub(), Name: "Example Organization", Admit: cs("alice", "bob", "carol"),
		Count: 3, Roll: borgrec.RollRoot(cs("alice", "bob", "carol")), Created: Created, Policy: emptyPolicy}
	rec0, err := o0.Encode()
	if err != nil {
		return err
	}
	t0, err := e.token(borgrec.OrgDerivation, borgrec.TagOrg, rec0, e.chain, 0)
	if err != nil {
		return err
	}
	e.c.mine(t0)
	o1 := &borgrec.Org{Hall: Hall, Org: Org.Pub(), Seq: 1, Prev: [32]byte(*t0.TxID()), Name: "Example Organization",
		Revoke: cs("bob"), Count: 2, Roll: borgrec.RollRoot(cs("alice", "carol")), Created: Created + 7200,
		Policy: emptyPolicy}
	rec1, err := o1.Encode()
	if err != nil {
		return err
	}
	t1, err := e.token(borgrec.OrgDerivation, borgrec.TagOrg, rec1, t0, 0)
	if err != nil {
		return err
	}
	e.c.mine(t1)
	e.org, e.orgRecs = []*transaction.Transaction{t0, t1}, []*borgrec.Org{o0, o1}

	// The group: create (alice, bob, epoch 0); add carol; remove bob (epoch
	// 1). The last two are mined in one block.
	gid := GroupID()
	g0 := &borgrec.Group{Hall: Hall, Org: Org.Pub(), Group: gid, Name: "Engineering", Add: sortedKeys(Alice, Bob), Count: 2,
		Members: borgrec.MembersRoot(sortedKeys(Alice, Bob)), Commitment: borgrec.Commitment(s.EpochKeys[0]), Created: Created + 600}
	grec0, err := g0.Encode()
	if err != nil {
		return err
	}
	gt0, err := e.token(borgrec.GroupDerivation, borgrec.TagGroup, grec0, e.chain, 1)
	if err != nil {
		return err
	}
	e.c.mine(gt0)
	g1 := *g0
	g1.Seq, g1.Prev, g1.Add, g1.Count, g1.Created = 1, [32]byte(*gt0.TxID()), sortedKeys(Carol), 3, Created+1200
	g1.Members = borgrec.MembersRoot(sortedKeys(Alice, Bob, Carol))
	grec1, err := g1.Encode()
	if err != nil {
		return err
	}
	gt1, err := e.token(borgrec.GroupDerivation, borgrec.TagGroup, grec1, gt0, 0)
	if err != nil {
		return err
	}
	g2 := g1
	g2.Seq, g2.Prev, g2.Add, g2.Remove, g2.Count, g2.Created = 2, [32]byte(*gt1.TxID()), nil, sortedKeys(Bob), 2, Created+1800
	g2.Epoch, g2.EpochSeq, g2.Commitment = 1, 2, borgrec.Commitment(s.EpochKeys[1])
	g2.Members = borgrec.MembersRoot(sortedKeys(Alice, Carol))
	grec2, err := g2.Encode()
	if err != nil {
		return err
	}
	gt2, err := e.token(borgrec.GroupDerivation, borgrec.TagGroup, grec2, gt1, 0)
	if err != nil {
		return err
	}
	e.c.mineTogether(gt1, gt2)
	e.group, e.groupRec = []*transaction.Transaction{gt0, gt1, gt2}, []*borgrec.Group{g0, &g1, &g2}

	// The wraps: epoch 0 to alice and bob, epoch 1 to alice and carol, each
	// on its own record-tree output.
	rp := params(borgrec.RecordDerivation)
	for i, p := range []struct {
		g     *borgrec.Group
		rec   []byte
		epoch int
		to    []Party
	}{{g0, grec0, 0, []Party{Alice, Bob}}, {&g2, grec2, 1, []Party{Alice, Carol}}} {
		w := &borgrec.WrapRecord{Hall: Hall, Org: Org.Pub(), Group: gid, Epoch: uint64(p.epoch), EpochID: borgrec.EpochID(p.rec)}
		to := append([]Party(nil), p.to...)
		sort.Slice(to, func(i, j int) bool { return string(to[i].Pub()) < string(to[j].Pub()) })
		for _, m := range to {
			ent, err := borgrec.WrapWithIV(Org.Priv, m.Priv.PubKey(), w.EpochID, uint32(p.epoch), s.EpochKeys[p.epoch], label("iv/tx-wrap/%d/%s", p.epoch, m.Name))
			if err != nil {
				return err
			}
			w.Entries = append(w.Entries, ent)
		}
		wrec, err := w.Encode()
		if err != nil {
			return err
		}
		k, err := carrier.Mint(e.ctx, e.o.w, "", rp, wrec, e.rec, uint32(i))
		if err != nil {
			return err
		}
		e.wraps, e.wrapRecs = append(e.wraps, k), append(e.wrapRecs, wrec)
	}

	// The sweep: grant-tree outputs 1 (bob's revocation outpoint) and 5
	// (unused); the tree pays the fee and the tombstone takes the rest.
	if e.sweep, err = carrier.Sweep(e.ctx, e.o.w, "", gp, e.grant, []uint32{1, 5}, nil, 0, nil, e.o.feeLock, 1, 250); err != nil {
		return err
	}
	e.c.mine(e.sweep)

	// What the topic holds once the admitted cases are admitted: each
	// carrier's record output, each token output and the sweep's tombstone.
	e.held = map[transaction.Outpoint]bool{}
	for _, k := range append(append([]*transaction.Transaction{e.sweep}, e.wraps...), e.carriers["alice"], e.carriers["bob"], e.carriers["carol"]) {
		e.held[transaction.Outpoint{Txid: *k.TxID(), Index: 0}] = true
	}
	for _, t := range append(append([]*transaction.Transaction{}, e.org...), e.group...) {
		e.held[transaction.Outpoint{Txid: *t.TxID(), Index: 0}] = true
	}
	return nil
}

// ---- helpers that build what the cases change ----

// flagUnlocker signs a single-signature input with key under another
// sighash type.
type flagUnlocker struct {
	key  *ec.PrivateKey
	flag sighash.Flag
}

func (u flagUnlocker) Sign(tx *transaction.Transaction, i uint32) (*script.Script, error) {
	d, err := tx.CalcInputSignatureHash(i, u.flag)
	if err != nil {
		return nil, err
	}
	sig, err := u.key.Sign(d)
	if err != nil {
		return nil, err
	}
	return scr(push(append(sig.Serialize(), byte(u.flag)))), nil
}

func (u flagUnlocker) EstimateLength(*transaction.Transaction, uint32) uint32 { return 74 }

func signAll(tx *transaction.Transaction) *transaction.Transaction {
	if err := tx.Sign(); err != nil {
		panic(err)
	}
	return tx
}

// carrierFrom builds a carrier by hand: one input spending src:vout, the
// given outputs, the carrier's locktime and sequence, signed.
func carrierFrom(src *transaction.Transaction, vout uint32, u transaction.UnlockingScriptTemplate, outs ...*transaction.TransactionOutput) *transaction.Transaction {
	tx := transaction.NewTransaction()
	tx.LockTime = carrier.LockTime
	tx.AddInputFromTx(src, vout, u)
	tx.Inputs[0].SequenceNumber = carrier.Sequence
	for _, o := range outs {
		tx.AddOutput(o)
	}
	return signAll(tx)
}

func derInt(v *big.Int) []byte {
	b := v.Bytes()
	if len(b) == 0 || b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return b
}

// highSBytes rewrites a strict low-S DER signature to its high-S twin.
func highSBytes(der []byte) []byte {
	sig, err := ec.ParseDERSignature(der)
	if err != nil {
		panic(err)
	}
	r, s := derInt(sig.R), derInt(new(big.Int).Sub(ec.S256().N, sig.S))
	body := append(append([]byte{0x02, byte(len(r))}, r...), append([]byte{0x02, byte(len(s))}, s...)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// cloneTx copies tx, keeping each input's source transaction.
func cloneTx(tx *transaction.Transaction) *transaction.Transaction {
	cp := &transaction.Transaction{Version: tx.Version, LockTime: tx.LockTime, MerklePath: tx.MerklePath}
	for _, in := range tx.Inputs {
		c := *in
		cp.Inputs = append(cp.Inputs, &c)
	}
	for _, o := range tx.Outputs {
		c := *o
		cp.Outputs = append(cp.Outputs, &c)
	}
	return cp
}

// rewriteUnlock replaces input i's unlocking script with f of its one push.
func rewriteUnlock(tx *transaction.Transaction, i int, f func(sig []byte) []byte) *transaction.Transaction {
	cp := cloneTx(tx)
	ch, err := tx.Inputs[i].UnlockingScript.Chunks()
	if err != nil {
		panic(err)
	}
	cp.Inputs[i].UnlockingScript = scr(f(ch[0].Data))
	cp.MerklePath = nil
	return cp
}

// spendFrom returns tx with input 0's source replaced by src, the same
// transaction under another proof (or none).
func spendFrom(tx, src *transaction.Transaction) *transaction.Transaction {
	cp := cloneTx(tx)
	cp.Inputs[0].SourceTransaction = src
	return cp
}

// reproved is tx under another proof.
func reproved(tx *transaction.Transaction, mp *transaction.MerklePath) *transaction.Transaction {
	cp := cloneTx(tx)
	cp.MerklePath = mp
	return cp
}

func push(d []byte) []byte {
	s := &script.Script{}
	if err := s.AppendPushData(d); err != nil {
		panic(err)
	}
	return *s
}

// fieldSigOf rebuilds a signed PushDrop's fields under key with its field
// signature replaced by f(signature), canonically otherwise.
func fieldSigOf(s *script.Script, key *ec.PublicKey, nfields int, f func([]byte) []byte) *script.Script {
	ch, err := s.Chunks()
	if err != nil {
		panic(err)
	}
	var fields [][]byte
	for i := 0; i < nfields; i++ {
		fields = append(fields, ch[2+i].Data)
	}
	return scr(borgrec.PushDropScript(key, fields, f(ch[2+nfields].Data)))
}

func sigOf(s *script.Script, nfields int) []byte {
	ch, err := s.Chunks()
	if err != nil {
		panic(err)
	}
	return ch[2+nfields].Data
}

func signWith(k *ec.PrivateKey, msg []byte) func([]byte) []byte {
	return func([]byte) []byte {
		d := sha256.Sum256(msg)
		sg, err := k.Sign(d[:])
		if err != nil {
			panic(err)
		}
		return sg.Serialize()
	}
}

// beefV1 writes a BEEF V1 by hand: the BUMPs, then each transaction with the
// index of its BUMP (-1 for none), in the order given; the last is the
// subject.
func beefV1(bumps []*transaction.MerklePath, txs []*transaction.Transaction, bumpOf []int) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xEFBE0001))
	b.Write(util.VarInt(uint64(len(bumps))).Bytes())
	for _, m := range bumps {
		b.Write(m.Bytes())
	}
	b.Write(util.VarInt(uint64(len(txs))).Bytes())
	for i, tx := range txs {
		b.Write(tx.Bytes())
		if bumpOf[i] < 0 {
			b.WriteByte(0)
			continue
		}
		b.WriteByte(1)
		b.Write(util.VarInt(uint64(bumpOf[i])).Bytes())
	}
	return b.Bytes()
}

// beefV2 writes a BEEF V2 (BRC-96) by hand: format 0 raw, 1 raw with a BUMP
// index, 2 txid only (a nil transaction at that place, its txid in ids).
func beefV2(bumps []*transaction.MerklePath, txs []*transaction.Transaction, bumpOf []int, ids map[int][32]byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint32(0xEFBE0002))
	b.Write(util.VarInt(uint64(len(bumps))).Bytes())
	for _, m := range bumps {
		b.Write(m.Bytes())
	}
	b.Write(util.VarInt(uint64(len(txs))).Bytes())
	for i, tx := range txs {
		if id, ok := ids[i]; ok {
			b.WriteByte(2)
			b.Write(id[:])
			continue
		}
		if bumpOf[i] < 0 {
			b.WriteByte(0)
			b.Write(tx.Bytes())
			continue
		}
		b.WriteByte(1)
		b.Write(util.VarInt(uint64(bumpOf[i])).Bytes())
		b.Write(tx.Bytes())
	}
	return b.Bytes()
}

// atomic wraps a BEEF V1 or V2 as Atomic BEEF (BRC-95) of subject.
func atomic(subject *chainhash.Hash, inner []byte) []byte {
	out := []byte{0x01, 0x01, 0x01, 0x01}
	out = append(out, subject[:]...)
	return append(out, inner...)
}

func tokenBEEF(t, parent *transaction.Transaction) []byte {
	b, err := borgrec.TokenBEEF(t, parent)
	if err != nil {
		panic(err)
	}
	return b
}

// scriptsVerify runs the interpreter over tx's own inputs, each against the
// output it spends.
func scriptsVerify(ctx context.Context, tx *transaction.Transaction) bool {
	cp := tx.ShallowClone()
	cp.MerklePath = nil
	ok, err := spv.Verify(ctx, cp, &spv.GullibleHeadersClient{}, nil)
	return err == nil && ok
}
