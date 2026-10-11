package borgrec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/chaintoken"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/pushdrop"
)

// The transaction refusals of spec section 11.1, beside the record and
// content refusals. A transaction is refused for the first rule it breaks. A
// record that names another hall than the topic's is refused with ErrHall,
// the label of a hall that breaks the grammar.
var (
	ErrShape       = errors.New("borgrec: transaction has the wrong shape")
	ErrBEEF        = chaintoken.ErrBEEF
	ErrMineable    = errors.New("borgrec: carrier is mineable")
	ErrUnlock      = errors.New("borgrec: unlocking script is not one canonical signature push")
	ErrLock        = errors.New("borgrec: output script is not the canonical one for the owner")
	ErrFieldSig    = errors.New("borgrec: field signature is not strict or does not verify")
	ErrFunding     = errors.New("borgrec: carrier does not spend a funding output of the owner")
	ErrUnmined     = errors.New("borgrec: proof does not verify against the host's headers")
	ErrTokenInput  = errors.New("borgrec: token does not spend its predecessor, or its chain tree at seq 0")
	ErrNotBorg     = errors.New("borgrec: transaction is not for this topic")
	ErrHostConfig  = errors.New("borgrec: host configuration or previous coins out of range")
	errNoPredRead  = errors.New("borgrec: predecessor is not a well-formed token")
	errHostHallBad = fmt.Errorf("%w: hall breaks the grammar", ErrHostConfig)
)

// TxKind is what an admitted transaction is to the topic.
type TxKind int

// The kinds of admitted transaction.
const (
	TxGrant TxKind = iota + 1
	TxPolicy
	TxWrap
	TxOrgToken
	TxGroupToken
	TxSweep
)

func (k TxKind) String() string {
	switch k {
	case TxGrant:
		return "grant"
	case TxPolicy:
		return "policy"
	case TxWrap:
		return "wrap"
	case TxOrgToken:
		return "org-token"
	case TxGroupToken:
		return "group-token"
	case TxSweep:
		return "sweep"
	}
	return "none"
}

// DefaultMaxBEEF is the BEEF bound Admit applies when a host names none: the
// floor spec section 11.1 sets for a host's own bound.
const DefaultMaxBEEF = chaintoken.DefaultMaxBEEF

// DefaultMaxSweepInputs is the sweep input bound Admit applies when a host
// names none: the floor spec section 11.1 sets.
const DefaultMaxSweepInputs = 1024

// Host is what admission needs beyond the submission: the topic's hall, a
// chain tracker over the host's own block headers, the host's BEEF bound
// (DefaultMaxBEEF when zero) and its sweep input bound
// (DefaultMaxSweepInputs when zero). Nothing the topic holds is here: a
// verdict never reads another record (spec section 11.1).
type Host struct {
	Hall           string
	Headers        chaintracker.ChainTracker
	MaxBEEF        int
	MaxSweepInputs int
}

// Carrier is an admitted grant, policy or wrap carrier.
type Carrier struct {
	Grant  *Grant
	Policy *Policy
	Wrap   *WrapRecord
	// Owner is the record's org, the key the carrier is bound to.
	Owner []byte
	// Record is the record exactly as carried.
	Record []byte
	// Commitment is C = txid(K), hash byte order.
	Commitment [32]byte
	// Funding is the outpoint the carrier spends; a mined sweep of it
	// retracts the carrier, and for a grant it is the revocation outpoint.
	Funding transaction.Outpoint
}

// ChainToken is a chain token's output: its record, decoded.
type ChainToken struct {
	Kind  Kind // KindOrg or KindGroup
	Org   *Org
	Group *Group
	// Record is the record exactly as carried (the token's second field).
	Record []byte
	// Vout is the token output's index.
	Vout uint32
}

// Seq is the token's seq.
func (t *ChainToken) Seq() uint64 {
	if t.Org != nil {
		return t.Org.Seq
	}
	return t.Group.Seq
}

// Admission is what a host admits from one transaction: the outputs to admit
// and the held inputs to retain (BRC-22's admittance instructions), and what
// the lookup service records.
type Admission struct {
	Kind TxKind
	// Txid is the transaction's txid, hash byte order.
	Txid [32]byte
	// Outputs are the output indices admitted.
	Outputs []int
	// Retain are the input indices that spend an output the topic holds.
	Retain []int
	// Carrier is set for TxGrant, TxPolicy and TxWrap.
	Carrier *Carrier
	// Token, Predecessor and Parent are set for a token. Predecessor is the
	// token at seq - 1 read from the parent the BEEF carries; nil at seq 0,
	// where the parent is the chain tree.
	Token       *ChainToken
	Predecessor *ChainToken
	Parent      [32]byte
	// Spent is, for TxSweep, every outpoint the sweep's inputs spend.
	Spent []transaction.Outpoint
}

// Admit applies spec section 11.1 to one submission: the BEEF as it arrived,
// and held, the indices of its subject's inputs that spend an output the
// topic holds (BRC-22's previous coins, as the engine hands them to the
// topic manager). held decides only what is retained, never a verdict: the
// predecessor of a token is read from the parent its BEEF carries, and no
// rule reads another topic or anything this topic holds. SPV of the BEEF is
// the engine's and is assumed to have run, including the scripts of an
// unproven carrier's input.
//
// Every refusal is an error: a topic manager raises it rather than answer
// with empty instructions, so the engine records nothing for the txid.
func Admit(ctx context.Context, beef []byte, held []int, h Host) (*Admission, error) {
	if CheckHall(h.Hall) != nil {
		return nil, errHostHallBad
	}
	bound := h.MaxBEEF
	if bound == 0 {
		bound = DefaultMaxBEEF
	}
	w, err := chaintoken.ReadWire(beef, bound)
	if err != nil {
		return nil, err
	}
	tx := w.SubjectTx()
	retain, err := retained(tx, held)
	if err != nil {
		return nil, err
	}
	a := &Admission{Txid: [32]byte(*tx.TxID()), Retain: retain}

	// Classify: the one output that claims a record or a token.
	claim, claims, at := KindNone, 0, 0
	for i, o := range tx.Outputs {
		if o == nil || o.LockingScript == nil {
			continue
		}
		if k := Classify(*o.LockingScript); k != KindNone && k != KindFunding {
			claim, at = k, i
			claims++
		}
	}
	if claims > 1 {
		return nil, fmt.Errorf("%w: %d outputs claim a record or a token", ErrShape, claims)
	}
	switch claim {
	case KindGrant, KindPolicy, KindWrap:
		c, err := checkCarrier(w, tx, claim, h)
		if err != nil {
			return nil, err
		}
		a.Kind, a.Carrier, a.Outputs = TxGrant, c, []int{0}
		switch claim {
		case KindPolicy:
			a.Kind = TxPolicy
		case KindWrap:
			a.Kind = TxWrap
		}
		return a, nil
	case KindOrg, KindGroup:
		t, pred, parent, err := checkToken(ctx, w, tx, at, claim, h)
		if err != nil {
			return nil, err
		}
		a.Kind, a.Token, a.Predecessor, a.Parent, a.Outputs = TxOrgToken, t, pred, parent, []int{at}
		if claim == KindGroup {
			a.Kind = TxGroupToken
		}
		return a, nil
	}

	// Sweep: output 0 funding-shaped under any key, and mined.
	if len(tx.Outputs) > 0 && tx.Outputs[0] != nil && tx.Outputs[0].LockingScript != nil && isFundingShape(*tx.Outputs[0].LockingScript) {
		if !Mined(ctx, tx, h.Headers) {
			return nil, ErrUnmined
		}
		if err := w.Alone(tx); err != nil {
			return nil, err
		}
		max := h.MaxSweepInputs
		if max == 0 {
			max = DefaultMaxSweepInputs
		}
		if len(tx.Inputs) > max {
			return nil, fmt.Errorf("%w: a sweep of %d inputs, host bound %d", ErrTooLarge, len(tx.Inputs), max)
		}
		a.Kind, a.Outputs = TxSweep, []int{0}
		for _, in := range tx.Inputs {
			a.Spent = append(a.Spent, transaction.Outpoint{Txid: *in.SourceTXID, Index: in.SourceTxOutIndex})
		}
		return a, nil
	}
	return nil, ErrNotBorg
}

// retained checks held against the inputs and returns it sorted, once each.
func retained(tx *transaction.Transaction, held []int) ([]int, error) {
	seen := map[int]bool{}
	var out []int
	for _, i := range held {
		if i < 0 || i >= len(tx.Inputs) {
			return nil, fmt.Errorf("%w: previous coin names input %d of %d", ErrHostConfig, i, len(tx.Inputs))
		}
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// derivation is the derivation a kind's output is locked and signed under.
func derivation(k Kind) pushdrop.Derivation {
	switch k {
	case KindGrant:
		return GrantDerivation
	case KindPolicy:
		return PolicyDerivation
	case KindWrap:
		return RecordDerivation
	case KindOrg:
		return OrgDerivation
	case KindGroup:
		return GroupDerivation
	}
	panic("borgrec: no derivation for " + string(k))
}

// lockingKey derives the key an owner's output of kind k is locked to.
func lockingKey(k Kind, owner []byte) *ec.PublicKey {
	pub, err := guard.ParsePubKey(owner)
	if err != nil {
		panic(err) // the record's key rule already held it to a canonical key
	}
	key, err := derivation(k).ExpectedLockingKey(pub)
	if err != nil {
		panic(err)
	}
	return key
}

// Mined reports whether tx carries a proof that verifies against the
// headers (bcommon chaintoken.Mined).
func Mined(ctx context.Context, tx *transaction.Transaction, headers chaintracker.ChainTracker) bool {
	return chaintoken.Mined(ctx, tx, headers)
}

// checkCarrier is the carrier's rules 1 to 10; 5 to 8 in bcommon
// carrier.Validate's order.
func checkCarrier(w *chaintoken.Wire, tx *transaction.Transaction, claim Kind, h Host) (*Carrier, error) {
	// 1. shape.
	if len(tx.Inputs) != 1 || len(tx.Outputs) != 1 {
		return nil, fmt.Errorf("%w: %d inputs, %d outputs", ErrShape, len(tx.Inputs), len(tx.Outputs))
	}
	in, out := tx.Inputs[0], tx.Outputs[0]
	src := in.SourceTxOutput()
	if src == nil || src.LockingScript == nil {
		// The value cannot be compared without the funding tree, whose
		// absence is the beef rule's.
		return nil, fmt.Errorf("%w: the spent output is not in the BEEF", ErrBEEF)
	}
	if src.Satoshis != out.Satoshis {
		return nil, fmt.Errorf("%w: output %d of input %d", ErrShape, out.Satoshis, src.Satoshis)
	}
	// 2. beef: the carrier (unproven) and its funding tree (proven, minimally).
	parent := *in.SourceTXID
	if _, err := w.Carrier(tx); err != nil {
		return nil, err
	}
	// 3. the record's own rules.
	record, _ := pushdrop.FirstPush(*out.LockingScript)
	c := &Carrier{Record: record, Commitment: carrier.Commitment(tx),
		Funding: transaction.Outpoint{Txid: parent, Index: in.SourceTxOutIndex}}
	var hall string
	switch claim {
	case KindGrant:
		g, err := DecodeGrant(record)
		if err != nil {
			return nil, err
		}
		c.Grant, c.Owner, hall = g, g.Org, g.Hall
	case KindPolicy:
		p, err := DecodePolicy(record)
		if err != nil {
			return nil, err
		}
		c.Policy, c.Owner, hall = p, p.Org, p.Hall
	default:
		r, err := DecodeWrap(record)
		if err != nil {
			return nil, err
		}
		c.Wrap, c.Owner, hall = r, r.Org, r.Hall
	}
	// 4. hall.
	if hall != h.Hall {
		return nil, fmt.Errorf("%w: not this topic's hall", ErrHall)
	}
	// 5. mineable.
	if tx.LockTime < carrier.LockTime || in.SequenceNumber == transaction.MaxTxInSequenceNum {
		return nil, ErrMineable
	}
	// 6. unlock, bcommon's check.
	if err := carrier.CheckUnlocking(tx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnlock, err)
	}
	// 7. lock.
	key := lockingKey(claim, c.Owner)
	fields, _ := pushdrop.Fields(*out.LockingScript)
	if len(fields) != 2 {
		return nil, fmt.Errorf("%w: %d pushes, want the record and a signature", ErrLock, len(fields))
	}
	sig := fields[1]
	if !bytes.Equal(*out.LockingScript, PushDropScript(key, [][]byte{record}, sig)) {
		return nil, ErrLock
	}
	// 8. signature.
	if !chaintoken.VerifyField(key, record, sig) {
		return nil, ErrFieldSig
	}
	// 9. funding: the same key as the lock.
	if !bytes.Equal(*src.LockingScript, FundingScript(key)) {
		return nil, ErrFunding
	}
	// 10. the content rules, a grant's alone.
	if g := c.Grant; g != nil {
		if err := g.CheckRevocation([32]byte(parent), in.SourceTxOutIndex); err != nil {
			return nil, err
		}
		if err := g.CheckCertificate(context.Background()); err != nil {
			return nil, err
		}
		if _, err := g.CheckReveal(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// checkToken is the token's rules 1 to 9.
func checkToken(ctx context.Context, w *chaintoken.Wire, tx *transaction.Transaction, at int, claim Kind, h Host) (*ChainToken, *ChainToken, [32]byte, error) {
	var none [32]byte
	out := tx.Outputs[at]
	// 1. shape: a PushDrop of two fields and a signature, of 1 satoshi.
	to, ok := chaintoken.ReadOutput(out, uint32(at), 2)
	if !ok {
		return nil, nil, none, fmt.Errorf("%w: a token is two fields, a signature and 1 satoshi", ErrShape)
	}
	fields := to.Fields
	// 2. beef: the token and its parent, each proven minimally, or one
	// merged path when both are in one block. Never an Atomic BEEF.
	parentEntry, err := w.Token(tx)
	if err != nil {
		return nil, nil, none, err
	}
	parent := parentEntry.Tx
	// 3. the record's own rules.
	t := &ChainToken{Kind: claim, Record: fields[1], Vout: uint32(at)}
	var hall string
	var org []byte
	if claim == KindOrg {
		if t.Org, err = DecodeOrg(fields[1]); err != nil {
			return nil, nil, none, err
		}
		hall, org = t.Org.Hall, t.Org.Org
	} else {
		if t.Group, err = DecodeGroup(fields[1]); err != nil {
			return nil, nil, none, err
		}
		hall, org = t.Group.Hall, t.Group.Org
	}
	// 4. hall.
	if hall != h.Hall {
		return nil, nil, none, fmt.Errorf("%w: not this topic's hall", ErrHall)
	}
	// 5. lock.
	key := lockingKey(claim, org)
	if !to.LockedTo(*out.LockingScript, key) {
		return nil, nil, none, ErrLock
	}
	// 6. signature.
	if !to.SignedBy(key) {
		return nil, nil, none, ErrFieldSig
	}
	// 7. unmined: the token's and the parent's proofs.
	if !Mined(ctx, tx, h.Headers) || !Mined(ctx, parent, h.Headers) {
		return nil, nil, none, ErrUnmined
	}
	// 8. token-input.
	ptxid := [32]byte(parentEntry.Txid)
	var pred *ChainToken
	spendsToken, spendsChain := 0, false
	for _, sp := range chaintoken.Spends(tx, parentEntry) {
		po := sp.Output
		switch Classify(*po.LockingScript) {
		case KindOrg, KindGroup:
			spendsToken++
			if p, err := wellFormedToken(po, sp.Vout, claim); err == nil {
				pred = p
			}
		case KindFunding:
			if bytes.Equal(*po.LockingScript, FundingScript(lockingKeyChain(org))) {
				spendsChain = true
			}
		}
	}
	if t.Seq() == 0 {
		if !spendsChain || spendsToken > 0 {
			return nil, nil, none, fmt.Errorf("%w: seq 0 spends a chain-tree output and no token", ErrTokenInput)
		}
		return t, nil, ptxid, nil
	}
	if ptxid != t.prev() || spendsToken != 1 || pred == nil || !samePredecessor(t, pred) {
		return nil, nil, none, fmt.Errorf("%w: the parent's token at seq - 1 is not spent exactly once", ErrTokenInput)
	}
	// 9. the transition rules against the predecessor.
	if claim == KindOrg {
		err = CheckOrgTransition(pred.Org, ptxid, t.Org)
	} else {
		err = CheckGroupTransition(pred.Group, ptxid, t.Group)
	}
	if err != nil {
		return nil, nil, none, err
	}
	return t, pred, ptxid, nil
}

func (t *ChainToken) prev() [32]byte {
	if t.Org != nil {
		return t.Org.Prev
	}
	return t.Group.Prev
}

// samePredecessor is rule 8's "a well-formed token of the same kind, hall
// and organisation (and group) at seq - 1".
func samePredecessor(t, p *ChainToken) bool {
	if t.Kind != p.Kind || p.Seq()+1 != t.Seq() {
		return false
	}
	if t.Org != nil {
		return p.Org.Hall == t.Org.Hall && bytes.Equal(p.Org.Org, t.Org.Org)
	}
	return p.Group.Hall == t.Group.Hall && bytes.Equal(p.Group.Org, t.Group.Org) && p.Group.Group == t.Group.Group
}

func lockingKeyChain(org []byte) *ec.PublicKey {
	pub, err := guard.ParsePubKey(org)
	if err != nil {
		panic(err)
	}
	key, err := ChainDerivation.ExpectedLockingKey(pub)
	if err != nil {
		panic(err)
	}
	return key
}

// wellFormedToken reads a parent's output as a token of kind k: two fields
// and a signature, 1 satoshi, the tag, a record that decodes, the canonical
// lock under the record's organisation and a field signature that verifies.
func wellFormedToken(o *transaction.TransactionOutput, vout uint32, k Kind) (*ChainToken, error) {
	if Classify(*o.LockingScript) != k {
		return nil, errNoPredRead
	}
	return readTokenOutput(o, vout)
}

// readTokenOutput is the well-formed reading of one token output.
func readTokenOutput(o *transaction.TransactionOutput, vout uint32) (*ChainToken, error) {
	k := Classify(*o.LockingScript)
	to, ok := chaintoken.ReadOutput(o, vout, 2)
	if (k != KindOrg && k != KindGroup) || !ok {
		return nil, errNoPredRead
	}
	fields := to.Fields
	t := &ChainToken{Kind: k, Record: fields[1], Vout: vout}
	var org []byte
	var err error
	if k == KindOrg {
		if t.Org, err = DecodeOrg(fields[1]); err != nil {
			return nil, err
		}
		org = t.Org.Org
	} else {
		if t.Group, err = DecodeGroup(fields[1]); err != nil {
			return nil, err
		}
		org = t.Group.Org
	}
	key := lockingKey(k, org)
	if !to.LockedTo(*o.LockingScript, key) || !to.SignedBy(key) {
		return nil, errNoPredRead
	}
	return t, nil
}

// TokenBEEF assembles the BEEF a chain token is admitted in (spec sections
// 11.1 and 11.6): bcommon chaintoken.TokenBEEF.
func TokenBEEF(token, parent *transaction.Transaction) ([]byte, error) {
	return chaintoken.TokenBEEF(token, parent)
}

// StoredToken reads a chain token as a host stores it: the token's
// transaction alone, with its proof. It returns the transaction and the
// token output's record, decoded, without the parent.
func StoredToken(b []byte) (*transaction.Transaction, *ChainToken, error) {
	tx, err := chaintoken.Stored(b, DefaultMaxBEEF)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrBEEF, err)
	}
	t, err := ReadToken(tx)
	return tx, t, err
}

// ReadToken reads the one token a transaction holds, well formed, without
// its parent: how the index and a reader read a stored token (spec section
// 11.6), which the engine keeps alone with its proof.
func ReadToken(tx *transaction.Transaction) (*ChainToken, error) {
	var found *ChainToken
	for i, o := range tx.Outputs {
		if o == nil || o.LockingScript == nil {
			continue
		}
		if k := Classify(*o.LockingScript); k == KindOrg || k == KindGroup {
			if found != nil {
				return nil, fmt.Errorf("%w: two token outputs", ErrShape)
			}
			t, err := readTokenOutput(o, uint32(i))
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrShape, err)
			}
			found = t
		}
	}
	if found == nil {
		return nil, ErrNotBorg
	}
	return found, nil
}
