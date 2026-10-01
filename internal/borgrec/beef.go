package borgrec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/util"

	"github.com/lightwebinc/bcommon/guard"
)

// DefaultMaxBEEF is the BEEF bound Admit applies when a host names none: the
// floor spec section 11.1 sets for a host's own bound.
const DefaultMaxBEEF = 256 << 10

// DefaultMaxSweepInputs is the sweep input bound Admit applies when a host
// names none: the floor spec section 11.1 sets.
const DefaultMaxSweepInputs = 1024

// The BEEF versions on the wire (BRC-62, BRC-96, BRC-95).
const (
	beefV1     = uint32(0xEFBE0001)
	beefV2     = uint32(0xEFBE0002)
	beefAtomic = uint32(0x01010101)
)

// beefEntry is one transaction as the BEEF declares it.
type beefEntry struct {
	tx       *transaction.Transaction // nil for a txid-only entry
	txid     chainhash.Hash
	bump     int // index of its BUMP, or -1
	txidOnly bool
}

// wireBEEF is a BEEF exactly as declared on the wire: its form, its BUMPs and
// its transactions in order, before any parser merges two proofs of one block
// or collapses a transaction listed twice. The rules of spec section 11.1
// count what was sent.
type wireBEEF struct {
	atomic  bool
	subject chainhash.Hash // the Atomic BEEF's subject, else the last entry's txid
	bumps   []*transaction.MerklePath
	entries []beefEntry
	byTxid  map[chainhash.Hash]*beefEntry
}

var errWire = errors.New("borgrec: malformed BEEF")

// readWire walks a BEEF the guard has already bounded, and links each
// transaction's inputs to the earlier entries that hold their source
// transactions and each proven transaction to its BUMP.
func readWire(b []byte, bound int) (*wireBEEF, error) {
	if err := guard.CheckBEEF(b, bound); err != nil {
		return nil, err
	}
	w := &wireBEEF{byTxid: map[chainhash.Hash]*beefEntry{}}
	r := bytes.NewReader(b)
	var v uint32
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return nil, errWire
	}
	if v == beefAtomic {
		w.atomic = true
		if _, err := io.ReadFull(r, w.subject[:]); err != nil {
			return nil, errWire
		}
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return nil, errWire
		}
	}
	if v != beefV1 && v != beefV2 {
		return nil, fmt.Errorf("%w: version %08x", errWire, v)
	}
	n, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		mp, err := transaction.NewMerklePathFromReader(r)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errWire, err)
		}
		w.bumps = append(w.bumps, mp)
	}
	if n, err = readVarInt(r); err != nil {
		return nil, err
	}
	for i := uint64(0); i < n; i++ {
		e := beefEntry{bump: -1}
		format := byte(0)
		if v == beefV2 {
			if format, err = r.ReadByte(); err != nil || format > 2 {
				return nil, errWire
			}
		}
		if format == 2 {
			e.txidOnly = true
			if _, err := io.ReadFull(r, e.txid[:]); err != nil {
				return nil, errWire
			}
		} else {
			hasBump := format == 1
			if hasBump { // V2: the BUMP index precedes the transaction
				if e.bump, err = readBumpIndex(r, len(w.bumps)); err != nil {
					return nil, err
				}
			}
			tx := &transaction.Transaction{}
			if _, err := tx.ReadFrom(r); err != nil {
				return nil, fmt.Errorf("%w: %v", errWire, err)
			}
			e.tx, e.txid = tx, *tx.TxID()
			if v == beefV1 { // V1: a flag and the index follow the transaction
				f, err := r.ReadByte()
				if err != nil || f > 1 {
					return nil, errWire
				}
				if f == 1 {
					if e.bump, err = readBumpIndex(r, len(w.bumps)); err != nil {
						return nil, err
					}
				}
			}
			if e.bump >= 0 {
				tx.MerklePath = w.bumps[e.bump]
			}
		}
		w.entries = append(w.entries, e)
	}
	if r.Len() != 0 || len(w.entries) == 0 {
		return nil, errWire
	}
	if !w.atomic {
		w.subject = w.entries[len(w.entries)-1].txid
	}
	for i := range w.entries {
		e := &w.entries[i]
		if e.tx != nil {
			for _, in := range e.tx.Inputs {
				if src := w.byTxid[*in.SourceTXID]; src != nil && src.tx != nil {
					in.SourceTransaction = src.tx
				}
			}
		}
		w.byTxid[e.txid] = e
	}
	if s := w.byTxid[w.subject]; s == nil || s.tx == nil {
		return nil, fmt.Errorf("%w: no subject transaction", errWire)
	}
	return w, nil
}

func (w *wireBEEF) subjectTx() *transaction.Transaction { return w.byTxid[w.subject].tx }

func readBumpIndex(r *bytes.Reader, n int) (int, error) {
	idx, err := readVarInt(r)
	if err != nil || idx >= uint64(n) {
		return -1, errWire
	}
	return int(idx), nil
}

func readVarInt(r *bytes.Reader) (uint64, error) {
	h, err := r.ReadByte()
	if err != nil {
		return 0, errWire
	}
	var n int
	switch h {
	case 0xfd:
		n = 2
	case 0xfe:
		n = 4
	case 0xff:
		n = 8
	default:
		return uint64(h), nil
	}
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, errWire
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// anyTxidOnly reports a BRC-96 txid-only entry: every transaction must be
// carried whole.
func (w *wireBEEF) anyTxidOnly() bool {
	for _, e := range w.entries {
		if e.txidOnly {
			return true
		}
	}
	return false
}

// pathSet is the leaves a path holds at each level: offset to whether the
// leaf is flagged as a txid.
func pathSet(mp *transaction.MerklePath) ([]map[uint64]bool, bool) {
	out := make([]map[uint64]bool, len(mp.Path))
	for h, level := range mp.Path {
		out[h] = map[uint64]bool{}
		for _, e := range level {
			if _, dup := out[h][e.Offset]; dup {
				return nil, false
			}
			out[h][e.Offset] = e.Txid != nil && *e.Txid
		}
	}
	return out, true
}

// txidOffset finds txid at the lowest level, flagged as a txid.
func txidOffset(mp *transaction.MerklePath, txid chainhash.Hash) (uint64, bool) {
	if len(mp.Path) == 0 {
		return 0, false
	}
	for _, e := range mp.Path[0] {
		if e.Hash != nil && *e.Hash == txid && e.Txid != nil && *e.Txid {
			return e.Offset, true
		}
	}
	return 0, false
}

// minimalPath reports whether mp proves txid with only the leaves that needs:
// at the lowest level the txid, flagged as one, and its sibling (a hash or a
// duplicate), and above that exactly the one sibling each level needs.
func minimalPath(mp *transaction.MerklePath, txid chainhash.Hash) bool {
	return mergedPath(mp, txid, txid)
}

// mergedPath reports whether mp is the union of the minimal paths of a and b
// (a == b for one transaction): at the lowest level a and b, each flagged as
// a txid and no other leaf flagged, and their siblings; above that only the
// siblings of their ancestors. A sibling that is itself an ancestor of the
// other transaction can be computed and MAY be left out, as BEEF writers do
// when they merge two paths of one block; every other sibling MUST be there,
// and nothing else may be.
func mergedPath(mp *transaction.MerklePath, a, b chainhash.Hash) bool {
	if mp == nil {
		return false
	}
	set, ok := pathSet(mp)
	if !ok || len(set) == 0 {
		return false
	}
	oa, okA := txidOffset(mp, a)
	ob, okB := txidOffset(mp, b)
	if !okA || !okB {
		return false
	}
	flagged := 0
	for _, isTxid := range set[0] {
		if isTxid {
			flagged++
		}
	}
	want := 2
	if a == b {
		want = 1
	}
	if flagged != want {
		return false
	}
	for h := range set {
		ancestors := map[uint64]bool{oa >> uint(h): true, ob >> uint(h): true}
		allowed := map[uint64]bool{}
		for x := range ancestors {
			allowed[x^1] = true
			if h == 0 {
				allowed[x] = true
			}
		}
		for off := range set[h] {
			if !allowed[off] {
				return false
			}
		}
		for x := range ancestors {
			s := x ^ 1
			if h == 0 && !set[0][x] {
				return false // the txid itself
			}
			if _, have := set[h][s]; !have && !ancestors[s] {
				return false // a sibling that cannot be computed
			}
		}
	}
	return true
}

// TokenBEEF assembles the BEEF a chain token is admitted in (spec sections
// 11.1 and 11.6): a BEEF V1 of exactly the parent then the token, each with
// its own proof, or one merged proof when both are in one block. A replayer
// builds it from the token as a host stores it (the token alone with its
// proof) and the parent: the token at seq - 1, or at seq 0 the chain tree.
func TokenBEEF(token, parent *transaction.Transaction) ([]byte, error) {
	if token == nil || parent == nil || token.MerklePath == nil || parent.MerklePath == nil {
		return nil, errors.New("borgrec: a token's BEEF needs the token and its parent, both proven")
	}
	tp, pp := clonePath(token.MerklePath), clonePath(parent.MerklePath)
	bumps := []*transaction.MerklePath{pp, tp}
	idx := []int{0, 1}
	if tp.BlockHeight == pp.BlockHeight {
		rt, err1 := tp.ComputeRoot(token.TxID())
		rp, err2 := pp.ComputeRoot(parent.TxID())
		if err1 == nil && err2 == nil && rt.IsEqual(rp) {
			if err := pp.Combine(tp); err != nil {
				return nil, err
			}
			bumps, idx = []*transaction.MerklePath{pp}, []int{0, 0}
		}
	}
	return writeV1(bumps, []*transaction.Transaction{parent, token}, idx), nil
}

func clonePath(mp *transaction.MerklePath) *transaction.MerklePath {
	cp, err := transaction.NewMerklePathFromBinary(mp.Bytes())
	if err != nil {
		panic(err) // a path that serialized parses
	}
	return cp
}

// writeV1 writes a BEEF V1 (BRC-62): the BUMPs, then each transaction with
// its BUMP index (-1 for none), in the order given; the last is the subject.
func writeV1(bumps []*transaction.MerklePath, txs []*transaction.Transaction, bumpOf []int) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, beefV1)
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

// StoredToken reads a chain token as a host stores it: the token's
// transaction alone, with its proof, as the engine keeps a proven
// transaction (an Atomic BEEF or any BEEF whose subject is the token). It
// returns the transaction, the token output's index and its record, decoded,
// without the parent: the index and a reader read a stored token this way,
// never by admitting it again.
func StoredToken(b []byte) (*transaction.Transaction, *ChainToken, error) {
	w, err := readWire(b, DefaultMaxBEEF)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrBEEF, err)
	}
	tx := w.subjectTx()
	t, err := ReadToken(tx)
	return tx, t, err
}
