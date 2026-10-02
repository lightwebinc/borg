package borgrec

import (
	"bytes"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// Organisation record keys.
const (
	OKMagic   = 0
	OKHall    = 1
	OKOrg     = 2
	OKSeq     = 3
	OKPrev    = 4
	OKName    = 5
	OKAdmit   = 6
	OKRevoke  = 7
	OKCount   = 8
	OKRoll    = 9
	OKOpaque  = 10
	OKCreated = 11
	okLast    = OKCreated
)

// Org is one organisation record: the second field of an organisation chain
// token. Each token is one transition of the organisation's roll.
type Org struct {
	Hall    string     // key 1
	Org     []byte     // key 2, the organisation's identity key
	Seq     uint64     // key 3, 0 for the first token
	Prev    [32]byte   // key 4, txid (hash byte order) of the token at seq-1; zero at seq 0
	Name    string     // key 5, display name
	Admit   [][32]byte // key 6, grant commitments entering the roll, ascending
	Revoke  [][32]byte // key 7, grant commitments leaving the roll, ascending
	Count   uint64     // key 8, grants in the roll after this transition
	Roll    [32]byte   // key 9, RollRoot of the roll after this transition
	Opaque  bool       // key 10, grants carry no public reveal
	Created uint64     // key 11, Unix seconds, a claim
	// Extra holds integer keys above 11, preserved verbatim and ignored.
	Extra cbor.Map
}

// Validate applies every rule that needs only the record itself, in the
// refusal order of spec section 3.1.
func (o *Org) Validate() error {
	if err := record.CheckExtra(o.Extra, okLast, okLast+1); err != nil {
		return err
	}
	if err := CheckHall(o.Hall); err != nil {
		return err
	}
	if err := checkOrgKey(o.Org); err != nil {
		return err
	}
	if err := record.InRange(o.Seq, 0, MaxSafe, "seq"); err != nil {
		return err
	}
	if err := checkName(o.Name); err != nil {
		return err
	}
	if len(o.Admit) > MaxDelta || record.Ascending32(o.Admit) != nil {
		return fmt.Errorf("%w: admit", ErrList)
	}
	if len(o.Revoke) > MaxDelta || record.Ascending32(o.Revoke) != nil {
		return fmt.Errorf("%w: revoke", ErrList)
	}
	if err := record.InRange(o.Count, 0, MaxRoll, "count"); err != nil {
		return err
	}
	if err := record.InRange(o.Created, 1, MaxTime, "created"); err != nil {
		return err
	}
	return o.cross()
}

// cross is refusal step 7: the rules across fields.
func (o *Org) cross() error {
	for _, a := range o.Admit {
		for _, r := range o.Revoke {
			if a == r {
				return fmt.Errorf("%w: a grant both admitted and revoked", ErrList)
			}
		}
	}
	if (o.Seq == 0) != zero32(o.Prev[:]) {
		return fmt.Errorf("%w: prev is zero exactly at seq 0", ErrChain)
	}
	if o.Seq == 0 && (len(o.Revoke) != 0 || o.Count != uint64(len(o.Admit))) {
		return fmt.Errorf("%w: the first token admits its whole roll", ErrCount)
	}
	return nil
}

// Encode writes the organisation record in canonical CBOR.
func (o *Org) Encode() ([]byte, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(OKMagic), Val: MagicOrg},
		{Key: uint64(OKHall), Val: o.Hall},
		{Key: uint64(OKOrg), Val: o.Org},
		{Key: uint64(OKSeq), Val: o.Seq},
		{Key: uint64(OKPrev), Val: append([]byte(nil), o.Prev[:]...)},
		{Key: uint64(OKName), Val: o.Name},
		{Key: uint64(OKAdmit), Val: record.Values32(o.Admit)},
		{Key: uint64(OKRevoke), Val: record.Values32(o.Revoke)},
		{Key: uint64(OKCount), Val: o.Count},
		{Key: uint64(OKRoll), Val: append([]byte(nil), o.Roll[:]...)},
		{Key: uint64(OKOpaque), Val: o.Opaque},
		{Key: uint64(OKCreated), Val: o.Created},
	}
	return record.Encode(m, o.Extra, MaxOrgRecord)
}

// DecodeOrg parses an organisation record in the refusal order.
func DecodeOrg(p []byte) (*Org, error) {
	f, err := decodeRecord(p, MaxOrgRecord, okLast, MagicOrg)
	if err != nil {
		return nil, err
	}
	o := &Org{Extra: f.Extra}
	if o.Hall, err = f.Text(OKHall); err != nil {
		return nil, err
	}
	if err := CheckHall(o.Hall); err != nil {
		return nil, err
	}
	if o.Org, err = f.BytesN(OKOrg, -1); err != nil {
		return nil, err
	}
	if err := checkOrgKey(o.Org); err != nil {
		return nil, err
	}
	if o.Seq, err = f.Uint(OKSeq, 0, MaxSafe); err != nil {
		return nil, err
	}
	b, err := f.BytesN(OKPrev, 32)
	if err != nil {
		return nil, err
	}
	copy(o.Prev[:], b)
	if o.Name, err = f.Text(OKName); err != nil {
		return nil, err
	}
	if err := checkName(o.Name); err != nil {
		return nil, err
	}
	if o.Admit, err = f.List32(OKAdmit, MaxDelta); err != nil {
		return nil, err
	}
	if o.Revoke, err = f.List32(OKRevoke, MaxDelta); err != nil {
		return nil, err
	}
	if o.Count, err = f.Uint(OKCount, 0, MaxRoll); err != nil {
		return nil, err
	}
	if b, err = f.BytesN(OKRoll, 32); err != nil {
		return nil, err
	}
	copy(o.Roll[:], b)
	if o.Opaque, err = f.Bool(OKOpaque); err != nil {
		return nil, err
	}
	if o.Created, err = f.Uint(OKCreated, 1, MaxTime); err != nil {
		return nil, err
	}
	if err := o.cross(); err != nil {
		return nil, err
	}
	return o, nil
}

// CheckOrgTransition is the admission rule an organisation token meets
// against the token it spends (read from the topic's previous coins): same
// hall and organisation, the next seq, prev naming the predecessor's
// transaction, and a count that follows from the predecessor's and the
// change lists.
func CheckOrgTransition(prev *Org, prevTxid [32]byte, next *Org) error {
	if next.Seq == 0 {
		return fmt.Errorf("%w: seq 0 spends no token", ErrTransition)
	}
	if next.Hall != prev.Hall || !bytes.Equal(next.Org, prev.Org) || next.Seq != prev.Seq+1 || next.Prev != prevTxid {
		return fmt.Errorf("%w: not the successor of the token spent", ErrTransition)
	}
	if next.Opaque != prev.Opaque {
		return fmt.Errorf("%w: opaque is fixed at the first token", ErrTransition)
	}
	if prev.Count+uint64(len(next.Admit)) < uint64(len(next.Revoke)) ||
		prev.Count+uint64(len(next.Admit))-uint64(len(next.Revoke)) != next.Count {
		return ErrCount
	}
	return nil
}
