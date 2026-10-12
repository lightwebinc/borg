package vectors

import (
	"fmt"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/mint"

	"github.com/lightwebinc/borg/borgrec"
)

type admitJSON struct {
	Kind        string   `json:"kind"`
	Outputs     []int    `json:"outputsToAdmit"`
	Retain      []int    `json:"coinsToRetain"`
	Commitment  string   `json:"commitment,omitempty"`
	Record      string   `json:"record,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	FundingTxid string   `json:"fundingTxid,omitempty"`
	FundingVout *int     `json:"fundingVout,omitempty"`
	Seq         *uint64  `json:"seq,omitempty"`
	Parent      string   `json:"parentTxid,omitempty"`
	PredSeq     *uint64  `json:"predecessorSeq,omitempty"`
	Spent       []string `json:"spent,omitempty"`
}

type coinJSON struct {
	InputIndex int    `json:"inputIndex"`
	Txid       string `json:"txid"`
	Vout       uint32 `json:"vout"`
}

type txJSON struct {
	Name  string     `json:"name"`
	Note  string     `json:"note"`
	Rule  string     `json:"rule,omitempty"`
	Txid  string     `json:"txid"`
	RawTx string     `json:"rawTx"`
	BEEF  string     `json:"beef"`
	Coins []coinJSON `json:"previousCoins"`
	// MaxSweepInputs is set where a case admits under a host sweep bound
	// other than the default.
	MaxSweepInputs int        `json:"maxSweepInputs,omitempty"`
	Scripts        bool       `json:"scriptsVerify"`
	Verdict        string     `json:"verdict"`
	Reason         string     `json:"reason,omitempty"`
	Admits         *admitJSON `json:"admits,omitempty"`
}

type txCase struct {
	name, note, rule string
	tx               *transaction.Transaction
	beef             []byte // the BEEF as submitted; the Atomic BEEF of tx when nil
	reason           string // "" admits
	held             []int  // overrides the computed previous coins when not nil
	maxSweep         int
}

// heldInputs is the previous coins the engine hands the topic manager for tx.
func (e *txEnv) heldInputs(tx *transaction.Transaction) []int {
	held := []int{}
	for i, in := range tx.Inputs {
		if e.held[transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex}] {
			held = append(held, i)
		}
	}
	return held
}

func admission(a *borgrec.Admission) *admitJSON {
	j := &admitJSON{Kind: a.Kind.String(), Outputs: append([]int{}, a.Outputs...), Retain: append([]int{}, a.Retain...)}
	if a.Retain == nil {
		j.Retain = []int{}
	}
	if c := a.Carrier; c != nil {
		v := int(c.Funding.Index)
		j.Commitment, j.Record, j.Owner = hx(c.Commitment[:]), hx(c.Record), hx(c.Owner)
		j.FundingTxid, j.FundingVout = c.Funding.Txid.String(), &v
	}
	if t := a.Token; t != nil {
		seq := t.Seq()
		j.Seq, j.Record, j.Parent = &seq, hx(t.Record), displayHex(a.Parent)
		if a.Predecessor != nil {
			p := a.Predecessor.Seq()
			j.PredSeq = &p
		}
	}
	for _, o := range a.Spent {
		j.Spent = append(j.Spent, fmt.Sprintf("%s.%d", o.Txid.String(), o.Index))
	}
	return j
}

// recordWith re-encodes a record with key k set to v, signed by nobody: the
// record rules are what refuse it.
func recordWith(rec []byte, k uint64, v cbor.Value) []byte { return mutate(rec, k, v) }

// cases builds every transaction case.
func (e *txEnv) cases(s *Scenario) ([]txCase, error) {
	o := e.o
	gd, rd, pd := borgrec.GrantDerivation, borgrec.RecordDerivation, borgrec.PolicyDerivation
	ka := e.carriers["alice"]
	recA, _ := firstPushOf(ka.Outputs[0].LockingScript)
	lockA := ka.Outputs[0].LockingScript
	gUnlock := o.unlocker(gd)
	var cs []txCase
	add := func(c txCase) { cs = append(cs, c) }

	// ---- admitted ----
	for _, m := range members {
		add(txCase{name: "grant-carrier-" + m.Name, note: m.Name + "'s grant carrier, spending grant-tree output " +
			fmt.Sprint(map[string]int{"alice": 0, "bob": 1, "carol": 2}[m.Name]) + ", its revocation outpoint", tx: e.carriers[m.Name]})
	}
	v1, err := ka.BEEF()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "grant-carrier-beef-v1", note: "grant-carrier-alice submitted as BEEF V1 (BRC-62) rather than Atomic BEEF: the same two transactions and one proof", tx: ka, beef: v1})
	add(txCase{name: "wrap-carrier-epoch-0", note: "epoch 0's wrap to alice and bob, spending record-tree output 0", tx: e.wraps[0]})
	add(txCase{name: "wrap-carrier-epoch-1", note: "epoch 1's wrap to alice and carol, spending record-tree output 1", tx: e.wraps[1]})
	add(txCase{name: "org-token-genesis", note: "the organization's seq 0, admitting the three grants, spending chain-tree output 0; its BEEF is BEEF V1 of the chain tree then the token, each with its own path", tx: e.org[0], beef: tokenBEEF(e.org[0], e.chain)})
	add(txCase{name: "org-token-revoke", note: "seq 1, revoking bob's grant: it spends seq 0's token output, which the topic holds and retains; the predecessor is read from the parent in the BEEF", tx: e.org[1], beef: tokenBEEF(e.org[1], e.org[0])})
	add(txCase{name: "org-token-revoke-predecessor-not-held", note: "org-token-revoke offered with no previous coins, as to a host that does not hold seq 0: admitted all the same (the index reports it pending)", tx: e.org[1], beef: tokenBEEF(e.org[1], e.org[0]), held: []int{}})
	add(txCase{name: "group-token-create", note: "the group's seq 0, epoch 0 (alice and bob), spending chain-tree output 1", tx: e.group[0], beef: tokenBEEF(e.group[0], e.chain)})
	add(txCase{name: "group-token-add", note: "seq 1 adds carol in epoch 0", tx: e.group[1], beef: tokenBEEF(e.group[1], e.group[0])})
	merged := tokenBEEF(e.group[2], e.group[1])
	add(txCase{name: "group-token-remove-same-block", note: "seq 2 removes bob and begins epoch 1; it is mined in the same block as its parent (seq 1), so the BEEF holds one path merged as the SDK merges it, flagging exactly both txids", tx: e.group[2], beef: merged})
	add(txCase{name: "group-token-v2", note: "group-token-add as a BEEF V2 (BRC-96)", tx: e.group[1],
		beef: beefV2([]*transaction.MerklePath{e.group[0].MerklePath, e.group[1].MerklePath}, []*transaction.Transaction{e.group[0], e.group[1]}, []int{0, 1}, nil)})
	add(txCase{name: "sweep", note: "a mined sweep of grant-tree outputs 1 (bob's revocation outpoint) and 5: its tombstone, output 0, is admitted and both outpoints are recorded, which retracts bob's carrier and revokes his grant", tx: e.sweep})
	add(txCase{name: "funding-tree-alone", note: "the grant tree, mined, published on its own: it claims nothing and its output 0 is funding-shaped, so it is indistinguishable from a sweep and is admitted as one (output 0 alone, its input's outpoint recorded)", tx: e.grant})
	add(txCase{name: "sweep-at-the-bound", note: "the sweep of two inputs under a host whose sweep bound is 2", tx: e.sweep, maxSweep: 2})

	// ---- carriers: shape ----
	two := carrierFrom(e.grant, 3, gUnlock, out(1000, lockA))
	two.AddInputFromTx(e.grant, 4, gUnlock)
	add(txCase{name: "carrier-two-inputs", note: "alice's grant output with a second funding input", rule: "carrier shape", tx: signAll(two), reason: "shape"})
	add(txCase{name: "carrier-two-outputs", note: "the record output with 999 and a second output of 1", rule: "carrier shape",
		tx: carrierFrom(e.grant, 0, gUnlock, out(999, lockA), out(1, o.feeLock)), reason: "shape"})
	add(txCase{name: "carrier-value-short", note: "the one output carries 999 of the input's 1000", rule: "carrier shape",
		tx: carrierFrom(e.grant, 0, gUnlock, out(999, lockA)), reason: "shape"})
	add(txCase{name: "carrier-grant-and-wrap", note: "one output claims a grant and another a wrap", rule: "carrier shape",
		tx: carrierFrom(e.grant, 0, gUnlock, out(500, lockA), out(500, e.wraps[0].Outputs[0].LockingScript)), reason: "shape"})
	tokLock := e.org[0].Outputs[0].LockingScript
	add(txCase{name: "carrier-grant-and-token", note: "one output claims a grant and another an organization token", rule: "carrier shape",
		tx: carrierFrom(e.grant, 0, gUnlock, out(999, lockA), out(1, tokLock)), reason: "shape"})

	// ---- carriers: the BEEF ----
	bare := cloneTx(e.grant)
	bare.MerklePath = nil
	add(txCase{name: "carrier-beef-tree-unproven", note: "grant-carrier-alice (same txid) with its funding tree unproven, so its BEEF also carries the tree's parent: refused, not recorded, so this BEEF cannot suppress the genuine one", rule: "carrier shape", tx: spendFrom(ka, bare), reason: "beef"})
	wide := e.c.wide(cloneTx(e.grant))
	add(txCase{name: "carrier-beef-proof-extra-leaf", note: "grant-carrier-alice with its funding tree proven by a path holding a leaf the proof does not need", rule: "carrier shape", tx: spendFrom(ka, wide), reason: "beef"})
	add(txCase{name: "carrier-beef-extra-transaction", note: "grant-carrier-alice's BEEF with an unrelated mined transaction and its proof riding along", rule: "carrier shape",
		tx: ka, beef: beefV1([]*transaction.MerklePath{e.misc.MerklePath, e.grant.MerklePath}, []*transaction.Transaction{e.misc, e.grant, ka}, []int{0, 1, -1}), reason: "beef"})
	add(txCase{name: "carrier-beef-no-funding-tree", note: "grant-carrier-alice's BEEF without its funding tree", rule: "carrier shape",
		tx: ka, beef: beefV1(nil, []*transaction.Transaction{ka}, []int{-1}), reason: "beef"})
	add(txCase{name: "carrier-beef-txid-only", note: "grant-carrier-alice in a BEEF V2 whose funding tree is a txid-only entry", rule: "carrier shape",
		tx: ka, beef: beefV2([]*transaction.MerklePath{e.grant.MerklePath}, []*transaction.Transaction{nil, ka}, []int{0, -1}, map[int][32]byte{0: [32]byte(*e.grant.TxID())}), reason: "beef"})
	badGrant := recordWith(recA, borgrec.MKFields, cbor.Map{{Key: "expiry", Val: make([]byte, 67)}, {Key: "role", Val: make([]byte, 60)}, {Key: "scope", Val: make([]byte, 60)}})
	add(txCase{name: "carrier-beef-before-record", note: "breaks the beef rule (funding tree unproven) and the record rules (a field ciphertext of the wrong length): refused for the BEEF", rule: "carrier shape",
		tx: spendFrom(carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, badGrant))), bare), reason: "beef"})

	// ---- carriers: the record, the hall ----
	add(txCase{name: "carrier-grant-record-refused", note: "a grant record whose expiry ciphertext is 67 bytes", tx: carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, badGrant))), reason: "fields"})
	badWrap := recordWith(e.wrapRecs[0], borgrec.WKEpoch, uint64(1))
	add(txCase{name: "carrier-wrap-record-refused", note: "epoch 0's wrap record claiming epoch 1: its entries' key ids no longer match", tx: carrierFrom(e.rec, 2, o.unlocker(rd), out(1000, o.lock(rd, true, badWrap))), reason: "entry"})
	og := *e.grants["alice"]
	og.Hall = OtherHall
	ogRec, err := og.Encode()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-other-hall", note: "a well-formed grant naming another hall than the topic's", tx: carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, ogRec))), reason: "hall"})
	finalOther := cloneTx(carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, ogRec))))
	finalOther.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	add(txCase{name: "carrier-hall-before-mineable", note: "breaks the hall rule and the mineable rule (a final input): refused for the hall", tx: signAll(finalOther), reason: "hall"})

	// ---- carriers: mineable, unlock ----
	low := carrierFrom(e.grant, 0, gUnlock, out(1000, lockA))
	low.LockTime = carrier.LockTime - 1
	add(txCase{name: "carrier-locktime-below", note: "nLockTime one second before 2100", rule: "carrier shape", tx: signAll(low), reason: "mineable"})
	final := carrierFrom(e.grant, 0, gUnlock, out(1000, lockA))
	final.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	add(txCase{name: "carrier-final-input", note: "the input's nSequence is 0xFFFFFFFF", rule: "carrier shape", tx: signAll(final), reason: "mineable"})
	add(txCase{name: "carrier-locktime-max", note: "alice's grant on its outpoint under nLockTime 0xFFFFFFFF and nSequence 0xFFFFFFFE: any locktime from 2100 and any non-final sequence is admitted; a second carrier on one outpoint is the index's to supersede",
		tx: func() *transaction.Transaction {
			f := carrierFrom(e.grant, 0, gUnlock, out(1000, lockA))
			f.LockTime, f.Inputs[0].SequenceNumber = 0xffffffff, 0xfffffffe
			return signAll(f)
		}()})
	add(txCase{name: "carrier-unlock-high-s", note: "grant-carrier-alice with its input signature's high-S twin, which verifies: another txid, same record", rule: "signatures",
		tx: rewriteUnlock(ka, 0, func(sg []byte) []byte { return push(append(highSBytes(sg[:len(sg)-1]), sg[len(sg)-1])) }), reason: "unlock"})
	add(txCase{name: "carrier-unlock-pushdata1", note: "the signature pushed by OP_PUSHDATA1", rule: "carrier shape",
		tx: rewriteUnlock(ka, 0, func(sg []byte) []byte { return append([]byte{script.OpPUSHDATA1, byte(len(sg))}, sg...) }), reason: "unlock"})
	add(txCase{name: "carrier-unlock-extra-push", note: "OP_0 pushed before the signature", rule: "carrier shape",
		tx: rewriteUnlock(ka, 0, func(sg []byte) []byte { return append([]byte{script.Op0}, push(sg)...) }), reason: "unlock"})
	gpriv := o.derivedPriv(gd)
	add(txCase{name: "carrier-unlock-sighash-single", note: "the input signed SIGHASH_SINGLE|FORKID (0x43), which verifies", rule: "signatures",
		tx: carrierFrom(e.grant, 0, flagUnlocker{gpriv, sighash.SingleForkID}, out(1000, lockA)), reason: "unlock"})
	add(txCase{name: "carrier-unlock-sighash-anyonecanpay", note: "the input signed SIGHASH_ALL|ANYONECANPAY|FORKID (0xc1), which verifies", rule: "signatures",
		tx: carrierFrom(e.grant, 0, flagUnlocker{gpriv, sighash.AllForkID | sighash.AnyOneCanPay}, out(1000, lockA)), reason: "unlock"})

	// ---- carriers: lock (script canonicality) ----
	add(txCase{name: "carrier-grant-under-record-key", note: "alice's grant record locked and signed under the record key (which wraps use), not grant", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(rd, true, recA))), reason: "lock"})
	add(txCase{name: "carrier-wrap-under-grant-key", note: "epoch 0's wrap record locked and signed under the grant key", rule: "script canonicality",
		tx: carrierFrom(e.rec, 2, o.unlocker(rd), out(1000, o.lock(gd, true, e.wrapRecs[0]))), reason: "lock"})
	alice, err := newActor(Alice)
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-lock-member-key", note: "the organization's grant record locked and signed under alice's grant key: the lock is bound to the record's org", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, alice.lock(gd, true, recA))), reason: "lock"})
	sigA := sigOf(lockA, 1)
	if len(recA) <= 255 || len(recA) > 65535 {
		return nil, fmt.Errorf("alice's grant is %d bytes; the non-minimal push case assumes OP_PUSHDATA2", len(recA))
	}
	gk := o.key(gd)
	wideLock := append(append(append([]byte{33}, gk.Compressed()...), script.OpCHECKSIG, script.OpPUSHDATA4, byte(len(recA)), byte(len(recA)>>8), 0, 0),
		append(append(append([]byte{}, recA...), push(sigA)...), script.Op2DROP)...)
	add(txCase{name: "carrier-lock-non-minimal-push", note: "the record pushed with OP_PUSHDATA4 where OP_PUSHDATA2 is minimal", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, scr(wideLock))), reason: "lock"})
	dd := append(append([]byte{}, (*lockA)[:len(*lockA)-1]...), script.OpDROP, script.OpDROP)
	add(txCase{name: "carrier-lock-drop-drop", note: "OP_DROP OP_DROP where OP_2DROP is the fewest", rule: "script canonicality", tx: carrierFrom(e.grant, 0, gUnlock, out(1000, scr(dd))), reason: "lock"})
	add(txCase{name: "carrier-lock-unsigned", note: "the record output carries no field signature", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, scr(borgrec.PushDropScript(gk, [][]byte{recA}, nil)))), reason: "lock"})
	add(txCase{name: "carrier-lock-extra-field", note: "a third push after the record and its signature, dropped canonically", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, scr(borgrec.PushDropScript(gk, [][]byte{recA, sigA}, []byte{0x62, 0x6f})))), reason: "lock"})
	add(txCase{name: "carrier-lock-trailing-opcode", note: "the canonical script followed by OP_NOP", rule: "script canonicality",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, scr(append(append([]byte{}, *lockA...), script.OpNOP)))), reason: "lock"})

	// ---- carriers: field signature ----
	add(txCase{name: "carrier-field-signature-high-s", note: "the field signature's high-S twin, which verifies", rule: "signatures",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, fieldSigOf(lockA, gk, 1, highSBytes))), reason: "signature"})
	add(txCase{name: "carrier-field-signature-wrong-key", note: "a strict signature over the record by a stranger's key", rule: "signatures",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, fieldSigOf(lockA, gk, 1, signWith(Dave.Priv, recA)))), reason: "signature"})
	add(txCase{name: "carrier-field-signature-identity-key", note: "a strict signature over the record by the organization's identity key rather than its grant key", rule: "signatures",
		tx: carrierFrom(e.grant, 0, gUnlock, out(1000, fieldSigOf(lockA, gk, 1, signWith(Org.Priv, recA)))), reason: "signature"})

	// ---- policy carriers ----
	// One definition and one binding, on the policy tree, and the ways a
	// host refuses one: the wrong funding key, the wrong locking key, and
	// a record its own rules refuse.
	polRec, err := (&borgrec.Policy{Hall: Hall, Org: Org.Pub(), Kind: borgrec.PolicyRole, Role: "releaser",
		Permissions: []string{"bsecret/release/payroll", "bstore/get/*"}, Created: Created}).Encode()
	if err != nil {
		return nil, err
	}
	bindRec, err := (&borgrec.Policy{Hall: Hall, Org: Org.Pub(), Kind: borgrec.PolicyBinding, Role: "releaser",
		To: Alice.Pub(), Created: Created}).Encode()
	if err != nil {
		return nil, err
	}
	pUnlock := o.unlocker(pd)
	add(txCase{name: "policy-carrier-definition", note: "a role definition on policy-tree output 0, the outpoint a sweep retires it by",
		tx: carrierFrom(e.policy, 0, pUnlock, out(1000, o.lock(pd, true, polRec)))})
	add(txCase{name: "policy-carrier-binding", note: "a binding of that role to alice's key, on policy-tree output 1",
		tx: carrierFrom(e.policy, 1, pUnlock, out(1000, o.lock(pd, true, bindRec)))})
	add(txCase{name: "policy-on-grant-tree", note: "a policy record funded from a grant-tree output: a grant's outpoint revokes a grant, so policy records are kept apart by their key", rule: "carrier shape",
		tx: carrierFrom(e.grant, 4, gUnlock, out(1000, o.lock(pd, true, polRec))), reason: "funding"})
	add(txCase{name: "policy-under-grant-key", note: "a policy record locked and signed under the grant key", rule: "script canonicality",
		tx: carrierFrom(e.policy, 2, pUnlock, out(1000, o.lock(gd, true, polRec))), reason: "lock"})
	add(txCase{name: "policy-record-upper-case-role", note: "a policy record whose role name is not the grammar's", rule: "the policy record",
		tx: carrierFrom(e.policy, 3, pUnlock, out(1000, o.lock(pd, true, recordWith(polRec, 4, "Releaser")))), reason: "role"})

	// ---- carriers: funding ----
	add(txCase{name: "carrier-grant-on-record-tree", note: "alice's grant funded from a record-tree output (funding-shaped under the record key): grant and wrap trees are kept apart by their keys", rule: "carrier shape",
		tx: carrierFrom(e.rec, 2, o.unlocker(rd), out(1000, lockA)), reason: "funding"})
	add(txCase{name: "carrier-wrap-on-grant-tree", note: "epoch 0's wrap funded from a grant-tree output", rule: "carrier shape",
		tx: carrierFrom(e.grant, 3, gUnlock, out(1000, e.wraps[0].Outputs[0].LockingScript)), reason: "funding"})
	add(txCase{name: "carrier-grant-on-chain-tree", note: "alice's grant funded from a funding output under the chain key", rule: "carrier shape",
		tx: carrierFrom(e.misc, 3, o.unlocker(borgrec.ChainDerivation), out(1000, lockA)), reason: "funding"})
	add(txCase{name: "carrier-funding-bare-p2pk", note: "spends a bare pay-to-public-key under the grant key", rule: "carrier shape", tx: carrierFrom(e.misc, 0, gUnlock, out(1000, lockA)), reason: "funding"})
	add(txCase{name: "carrier-funding-signed-tag", note: "spends an output under the grant key whose one field is the funding tag with a field signature", rule: "script canonicality", tx: carrierFrom(e.misc, 1, gUnlock, out(1000, lockA)), reason: "funding"})
	add(txCase{name: "carrier-funding-other-tag", note: "spends an output under the grant key whose one field is 62 6f 04", rule: "carrier shape", tx: carrierFrom(e.misc, 2, gUnlock, out(1000, lockA)), reason: "funding"})

	// ---- carriers: grant content, last ----
	add(txCase{name: "carrier-grant-revocation", note: "alice's grant (revocation outpoint grant-tree output 0) carried on grant-tree output 3", tx: carrierFrom(e.grant, 3, gUnlock, out(1000, lockA)), reason: "grant-revocation"})
	forged := *e.grants["alice"]
	forged.Signature = signWith(Dave.Priv, []byte("not the certificate"))(nil)
	fRec, err := forged.Encode()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-grant-signature", note: "alice's grant whose certificate signature is a stranger's strict signature over other bytes", tx: carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, fRec))), reason: "grant-signature"})
	wr := *e.grants["alice"]
	wr.Reveal = map[string][]byte{}
	for k, v := range e.grants["alice"].Reveal {
		wr.Reveal[k] = v
	}
	wr.Reveal["role"] = e.grants["alice"].Reveal["scope"]
	wrRec, err := wr.Encode()
	if err != nil {
		return nil, err
	}
	add(txCase{name: "carrier-grant-reveal", note: "alice's grant whose revealed role key is the scope's: it does not open the role", tx: carrierFrom(e.grant, 0, gUnlock, out(1000, o.lock(gd, true, wrRec))), reason: "grant-reveal"})

	if err := e.tokenCases(s, add); err != nil {
		return nil, err
	}

	// ---- transactions that claim nothing ----
	unminedSweep := cloneTx(e.sweep)
	unminedSweep.MerklePath = nil
	add(txCase{name: "sweep-unmined", note: "the sweep with no proof: nothing unmined retracts, so it is refused and decided again when it arrives mined", rule: "sweep", tx: unminedSweep, reason: "unmined"})
	add(txCase{name: "sweep-proof-unknown-height", note: "the sweep with a proof at a height the headers do not hold", rule: "sweep", tx: reproved(e.sweep, transaction.NewMerklePath(unproven, e.sweep.MerklePath.Path)), reason: "unmined"})
	add(txCase{name: "sweep-beef-extra-transaction", note: "the sweep in a BEEF V1 that also carries an unrelated mined transaction", rule: "sweep",
		tx: e.sweep, beef: beefV1([]*transaction.MerklePath{e.misc.MerklePath, e.sweep.MerklePath}, []*transaction.Transaction{e.misc, e.sweep}, []int{0, 1}), reason: "beef"})
	add(txCase{name: "sweep-over-the-bound", note: "the sweep of two inputs under a host whose sweep bound is 1", rule: "sweep", tx: e.sweep, maxSweep: 1, reason: "too-large"})
	late := transaction.NewTransaction()
	late.AddInputFromTx(e.grant, 4, gUnlock)
	late.AddOutput(out(700, o.feeLock))
	late.AddOutput(out(100, scr(borgrec.FundingScript(gk))))
	add(txCase{name: "sweep-tombstone-second", note: "a mined spend of grant-tree output 4 whose funding-shaped output is output 1, not 0", rule: "sweep", tx: e.c.mine(signAll(late)), reason: "not-borg"})
	tomb := borgrec.FundingScript(gk)
	loose := append(append(append([]byte{}, tomb[:35]...), script.OpPUSHDATA1, 3), tomb[36:]...)
	odd := transaction.NewTransaction()
	odd.AddInputFromTx(e.grant, 4, gUnlock)
	odd.AddOutput(out(800, scr(loose)))
	add(txCase{name: "sweep-tombstone-pushdata1", note: "a mined spend whose output 0 pushes the funding tag with OP_PUSHDATA1: not funding-shaped", rule: "script canonicality", tx: e.c.mine(signAll(odd)), reason: "not-borg"})
	spend := transaction.NewTransaction()
	spend.AddInputFromTx(e.sweep, 0, gUnlock)
	spend.AddOutput(out(e.sweep.Outputs[0].Satoshis-200, o.feeLock))
	add(txCase{name: "spend-held-tombstone", note: "an unmined transaction spending the sweep's tombstone, which the topic holds: a spend of held outputs is never accepted", rule: "sweep", tx: signAll(spend), reason: "not-borg"})
	return cs, nil
}

func firstPushOf(s *script.Script) ([]byte, bool) {
	ch, err := s.Chunks()
	if err != nil || len(ch) < 3 {
		return nil, false
	}
	return ch[2].Data, true
}

// tokenCases are the chain tokens' refusals.
func (e *txEnv) tokenCases(s *Scenario, add func(txCase)) error {
	o := e.o
	od, grd := borgrec.OrgDerivation, borgrec.GroupDerivation
	o0, o1 := e.orgRecs[0], e.orgRecs[1]
	rec1, _ := o1.Encode()
	t0, t1 := e.org[0], e.org[1]
	ok := o.key(od)

	// A token at seq 1 built from a changed record, spending t0.
	org1 := func(f func(*borgrec.Org)) (*transaction.Transaction, error) {
		r := *o1
		f(&r)
		b, err := r.Encode()
		if err != nil {
			return nil, err
		}
		t, err := e.token(od, borgrec.TagOrg, b, t0, 0)
		if err != nil {
			return nil, err
		}
		return e.c.mine(t), nil
	}
	// A token at seq 1 with an arbitrary lock, spending t0.
	org1Lock := func(lock *script.Script, sats uint64) *transaction.Transaction {
		t, err := mintRaw(e, lock, sats, t0, 0)
		if err != nil {
			panic(err)
		}
		return e.c.mine(t)
	}

	// shape.
	add(txCase{name: "token-two-satoshis", note: "seq 1's token output holding 2 satoshis", rule: "chain-token shape",
		tx: org1Lock(t1.Outputs[0].LockingScript, 2), beef: nil, reason: "shape"})
	threeF := o.lock(od, true, borgrec.TagOrg, rec1, []byte{0x01})
	tf := org1Lock(threeF, 1)
	add(txCase{name: "token-three-fields", note: "seq 1's token with a third field before the signature", rule: "chain-token shape", tx: tf, beef: tokenBEEF(tf, t0), reason: "shape"})

	// beef.
	add(txCase{name: "token-atomic-beef", note: "org-token-revoke's token and parent in an Atomic BEEF: a token is never an Atomic BEEF", rule: "chain-token shape",
		tx: t1, beef: atomic(t1.TxID(), tokenBEEF(t1, t0)), reason: "beef"})
	add(txCase{name: "token-alone", note: "seq 1 alone with its proof, as a host stores it: without its parent it is not admissible", rule: "chain-token shape", tx: t1, reason: "beef"})
	add(txCase{name: "token-extra-transaction", note: "seq 1 and its parent with an unrelated mined transaction", rule: "chain-token shape",
		tx: t1, beef: beefV1([]*transaction.MerklePath{e.misc.MerklePath, t0.MerklePath, t1.MerklePath}, []*transaction.Transaction{e.misc, t0, t1}, []int{0, 1, 2}), reason: "beef"})
	add(txCase{name: "token-parent-not-spent", note: "seq 1 with an unrelated mined transaction in its parent's place", rule: "chain-token shape",
		tx: t1, beef: beefV1([]*transaction.MerklePath{e.misc.MerklePath, t1.MerklePath}, []*transaction.Transaction{e.misc, t1}, []int{0, 1}), reason: "beef"})
	t0bare := cloneTx(t0)
	t0bare.MerklePath = nil
	add(txCase{name: "token-parent-unproven", note: "seq 1 with its parent carried without a proof", rule: "chain-token shape",
		tx: t1, beef: beefV1([]*transaction.MerklePath{t1.MerklePath}, []*transaction.Transaction{t0bare, t1}, []int{-1, 0}), reason: "beef"})
	add(txCase{name: "token-parent-txid-only", note: "seq 1 in a BEEF V2 whose parent is a txid-only entry", rule: "chain-token shape",
		tx: t1, beef: beefV2([]*transaction.MerklePath{t1.MerklePath}, []*transaction.Transaction{nil, t1}, []int{-1, 0}, map[int][32]byte{0: [32]byte(*t0.TxID())}), reason: "beef"})
	wideT0 := e.c.wide(cloneTx(t0))
	add(txCase{name: "token-parent-proof-extra-leaf", note: "seq 1 with its parent proven by a path holding a leaf it does not need", rule: "chain-token shape",
		tx: t1, beef: beefV1([]*transaction.MerklePath{wideT0.MerklePath, t1.MerklePath}, []*transaction.Transaction{wideT0, t1}, []int{0, 1}), reason: "beef"})
	g1, g2 := e.group[1], e.group[2]
	add(txCase{name: "token-same-block-two-paths", note: "group-token-remove-same-block with its parent's and its own paths kept apart: two paths of one block are refused, BEEF writers merge them", rule: "chain-token shape",
		tx: g2, beef: beefV1([]*transaction.MerklePath{g1.MerklePath, g2.MerklePath}, []*transaction.Transaction{g1, g2}, []int{0, 1}), reason: "beef"})
	add(txCase{name: "token-beef-before-record", note: "breaks the beef rule (Atomic BEEF) and the record rules: refused for the BEEF", rule: "chain-token shape",
		tx: org1Lock(o.lock(od, true, borgrec.TagOrg, recordWith(rec1, borgrec.OKCreated, uint64(0))), 1), reason: "beef"})

	// the record, the hall.
	badRec := org1Lock(o.lock(od, true, borgrec.TagOrg, recordWith(rec1, borgrec.OKCreated, uint64(0))), 1)
	add(txCase{name: "token-record-refused", note: "seq 1 whose record has created 0, signed", tx: badRec, beef: tokenBEEF(badRec, t0), reason: "range"})
	oh, err := org1(func(r *borgrec.Org) { r.Hall = OtherHall })
	if err != nil {
		return err
	}
	add(txCase{name: "token-other-hall", note: "seq 1 naming another hall", tx: oh, beef: tokenBEEF(oh, t0), reason: "hall"})

	// lock.
	ug := org1Lock(o.lock(grd, true, borgrec.TagOrg, rec1), 1)
	add(txCase{name: "token-org-under-group-key", note: "seq 1's organization record locked and signed under the group key", rule: "script canonicality", tx: ug, beef: tokenBEEF(ug, t0), reason: "lock"})
	alice, err := newActor(Alice)
	if err != nil {
		return err
	}
	ua := org1Lock(alice.lock(od, true, borgrec.TagOrg, rec1), 1)
	add(txCase{name: "token-lock-member-key", note: "the organization's seq 1 record locked and signed under alice's org key", rule: "script canonicality", tx: ua, beef: tokenBEEF(ua, t0), reason: "lock"})
	sig1 := sigOf(t1.Outputs[0].LockingScript, 2)
	if len(rec1) <= 75 || len(rec1) > 255 {
		return fmt.Errorf("org seq 1 is %d bytes; the non-minimal push case assumes OP_PUSHDATA1", len(rec1))
	}
	nm := append(append(append([]byte{33}, ok.Compressed()...), script.OpCHECKSIG, 3, 'b', 'o', 0x01, script.OpPUSHDATA2, byte(len(rec1)), 0),
		append(append(append([]byte{}, rec1...), push(sig1)...), script.Op2DROP, script.OpDROP)...)
	tnm := org1Lock(scr(nm), 1)
	add(txCase{name: "token-lock-non-minimal-push", note: "seq 1's record pushed with OP_PUSHDATA2 where OP_PUSHDATA1 is minimal", rule: "script canonicality", tx: tnm, beef: tokenBEEF(tnm, t0), reason: "lock"})

	// signature.
	hs := org1Lock(fieldSigOf(t1.Outputs[0].LockingScript, ok, 2, highSBytes), 1)
	add(txCase{name: "token-field-signature-high-s", note: "seq 1's field signature's high-S twin", rule: "signatures", tx: hs, beef: tokenBEEF(hs, t0), reason: "signature"})
	recOnly := org1Lock(fieldSigOf(t1.Outputs[0].LockingScript, ok, 2, signWith(o.derivedPriv(od), rec1)), 1)
	add(txCase{name: "token-field-signature-record-only", note: "a strict signature by the org key over the record alone, not the tag and the record", rule: "signatures", tx: recOnly, beef: tokenBEEF(recOnly, t0), reason: "signature"})

	// unmined.
	t1x := reproved(t1, transaction.NewMerklePath(unproven, t1.MerklePath.Path))
	add(txCase{name: "token-proof-unknown-height", note: "seq 1 proven at a height the headers do not hold", tx: t1x, beef: tokenBEEF(t1x, t0), reason: "unmined"})
	t0x := reproved(t0, transaction.NewMerklePath(unproven, t0.MerklePath.Path))
	add(txCase{name: "token-parent-proof-unknown-height", note: "seq 1 whose parent's proof names a height the headers do not hold", tx: t1, beef: tokenBEEF(t1, t0x), reason: "unmined"})

	// token-input.
	pc, err := org1(func(r *borgrec.Org) { r.Prev = label("txid/elsewhere") })
	if err != nil {
		return err
	}
	add(txCase{name: "token-prev-not-parent", note: "seq 1 whose prev names another transaction than the parent it spends", rule: "chain-token shape", tx: pc, beef: tokenBEEF(pc, t0), reason: "token-input"})
	sk, err := org1(func(r *borgrec.Org) { r.Seq = 2 })
	if err != nil {
		return err
	}
	add(txCase{name: "token-seq-skips", note: "a token at seq 2 spending seq 0", rule: "chain-token shape", tx: sk, beef: tokenBEEF(sk, t0), reason: "token-input"})
	ch, err := mint.Transition(t1.Outputs[0].LockingScript, 1, &mint.Input{Tx: t0, Vout: 1, Unlocker: o.feeUnlocker()}, e.nextFee(), o.feeLock, mint.LegacyFees)
	if err != nil {
		return err
	}
	e.c.mine(ch)
	add(txCase{name: "token-spends-change", note: "seq 1's record spending seq 0's change output, not its token", rule: "chain-token shape", tx: ch, beef: tokenBEEF(ch, t0), reason: "token-input"})
	gOnOrg, err := e.groupOn(t0, 0, 1)
	if err != nil {
		return err
	}
	add(txCase{name: "token-group-spends-org-token", note: "a group token at seq 1 spending the organization's token: another kind", rule: "chain-token shape", tx: gOnOrg, beef: tokenBEEF(gOnOrg, t0), reason: "token-input"})
	g0rec, _ := e.groupRec[0].Encode()
	recTree, err := e.token(grd, borgrec.TagGroup, g0rec, e.rec, 3)
	if err != nil {
		return err
	}
	e.c.mine(recTree)
	add(txCase{name: "token-genesis-on-record-tree", note: "the group's seq 0 spending a record-tree output, not a chain-tree output", rule: "chain-token shape", tx: recTree, beef: tokenBEEF(recTree, e.rec), reason: "token-input"})
	gOnTok, err := e.groupOn(t0, 0, 0)
	if err != nil {
		return err
	}
	add(txCase{name: "token-genesis-spends-token", note: "a group seq 0 spending a token output", rule: "chain-token shape", tx: gOnTok, beef: tokenBEEF(gOnTok, t0), reason: "token-input"})

	// transition rules.
	op, err := org1(func(r *borgrec.Org) { r.Opaque = true })
	if err != nil {
		return err
	}
	add(txCase{name: "token-opaque-changes", note: "seq 1 setting opaque, fixed at the first token", tx: op, beef: tokenBEEF(op, t0), reason: "transition"})
	cnt, err := org1(func(r *borgrec.Org) { r.Count = 3 })
	if err != nil {
		return err
	}
	add(txCase{name: "token-count", note: "seq 1 whose count is not the predecessor's less one", tx: cnt, beef: tokenBEEF(cnt, t0), reason: "count"})
	g0 := e.group[0]
	nr := *e.groupRec[2]
	nr.Seq, nr.Prev, nr.Epoch, nr.EpochSeq, nr.Commitment = 1, [32]byte(*g0.TxID()), 0, 0, e.groupRec[0].Commitment
	nr.Remove, nr.Count, nr.Members = sortedKeys(Bob), 1, borgrec.MembersRoot(sortedKeys(Alice))
	nrRec, err := nr.Encode()
	if err != nil {
		return err
	}
	nrt, err := e.token(grd, borgrec.TagGroup, nrRec, g0, 0)
	if err != nil {
		return err
	}
	e.c.mine(nrt)
	add(txCase{name: "token-removal-without-rotation", note: "a group seq 1 removing bob in epoch 0: no removal is admitted without a new epoch", tx: nrt, beef: tokenBEEF(nrt, g0), reason: "rotation"})
	_ = o0
	_ = s
	return nil
}

// mintRaw builds a token transaction with an arbitrary lock and value,
// spending prev:vout (signed for its kind) and a fee coin.
func mintRaw(e *txEnv, lock *script.Script, sats uint64, prev *transaction.Transaction, vout uint32) (*transaction.Transaction, error) {
	return mint.Transition(lock, sats, &mint.Input{Tx: prev, Vout: vout, Unlocker: e.prevUnlocker(prev, vout)}, e.nextFee(), e.o.feeLock, mint.LegacyFees)
}

// groupOn builds a group token at seq spending prev:vout, mined.
func (e *txEnv) groupOn(prev *transaction.Transaction, vout uint32, seq uint64) (*transaction.Transaction, error) {
	g := *e.groupRec[0]
	if seq > 0 {
		g.Seq, g.Prev, g.Add, g.Remove, g.Count = seq, [32]byte(*prev.TxID()), nil, nil, 2
	}
	b, err := g.Encode()
	if err != nil {
		return nil, err
	}
	t, err := e.token(borgrec.GroupDerivation, borgrec.TagGroup, b, prev, vout)
	if err != nil {
		return nil, err
	}
	return e.c.mine(t), nil
}

var _ = ec.S256
