package borgrec

import (
	"bytes"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// Wrap record keys.
const (
	WKMagic   = 0
	WKHall    = 1
	WKOrg     = 2
	WKGroup   = 3
	WKEpoch   = 4
	WKEpochID = 5
	WKEntries = 6
	wkLast    = WKEntries
)

// WrapRecord is one wrap record: the payload of one wrap carrier. It
// releases one epoch's key to up to MaxEntries members, one BRC-369 release
// (BRC-78 serialization) each.
type WrapRecord struct {
	Hall    string           // key 1
	Org     []byte           // key 2, the releaser and the carrier's owner
	Group   [GroupIDLen]byte // key 3
	Epoch   uint64           // key 4
	EpochID [32]byte         // key 5, SHA-256 of the record of the token that began the epoch
	Entries [][]byte         // key 6, WrapLen bytes each, ascending by recipient
	Extra   cbor.Map         // integer keys above 6, preserved and ignored
}

// Recipients returns each entry's recipient key, in record order.
func (w *WrapRecord) Recipients() [][]byte {
	out := make([][]byte, len(w.Entries))
	for i, e := range w.Entries {
		out[i] = e[37:70]
	}
	return out
}

// checkEntries is the rule of key 6: 1 to MaxEntries wraps, each meeting
// CheckEntry for this record, recipients strictly ascending.
func (w *WrapRecord) checkEntries() error {
	if len(w.Entries) < 1 || len(w.Entries) > MaxEntries {
		return fmt.Errorf("%w: entries", ErrList)
	}
	var last []byte
	for _, e := range w.Entries {
		ent, err := CheckEntry(e, w.Org, w.EpochID, uint32(w.Epoch))
		if err != nil {
			return err
		}
		if last != nil && bytes.Compare(last, ent.Recipient) >= 0 {
			return fmt.Errorf("%w: recipients not ascending", ErrEntry)
		}
		last = ent.Recipient
	}
	return nil
}

// Validate applies every rule that needs only the record itself.
func (w *WrapRecord) Validate() error {
	if err := record.CheckExtra(w.Extra, wkLast, wkLast+1); err != nil {
		return err
	}
	if err := CheckHall(w.Hall); err != nil {
		return err
	}
	if err := CheckOrgKey(w.Org); err != nil {
		return err
	}
	if err := record.InRange(w.Epoch, 0, MaxEpoch, "epoch"); err != nil {
		return err
	}
	return w.checkEntries()
}

// Encode writes the wrap record in canonical CBOR.
func (w *WrapRecord) Encode() ([]byte, error) {
	if err := w.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(WKMagic), Val: MagicWrap},
		{Key: uint64(WKHall), Val: w.Hall},
		{Key: uint64(WKOrg), Val: w.Org},
		{Key: uint64(WKGroup), Val: append([]byte(nil), w.Group[:]...)},
		{Key: uint64(WKEpoch), Val: w.Epoch},
		{Key: uint64(WKEpochID), Val: append([]byte(nil), w.EpochID[:]...)},
		{Key: uint64(WKEntries), Val: valuesBytes(w.Entries)},
	}
	return record.Encode(m, w.Extra, MaxWrapRecord)
}

// DecodeWrap parses a wrap record in the refusal order.
func DecodeWrap(p []byte) (*WrapRecord, error) {
	f, err := decodeRecord(p, MaxWrapRecord, wkLast, MagicWrap)
	if err != nil {
		return nil, err
	}
	w := &WrapRecord{Extra: f.Extra}
	if w.Hall, err = f.Text(WKHall); err != nil {
		return nil, err
	}
	if err := CheckHall(w.Hall); err != nil {
		return nil, err
	}
	if w.Org, err = f.BytesN(WKOrg, -1); err != nil {
		return nil, err
	}
	if err := CheckOrgKey(w.Org); err != nil {
		return nil, err
	}
	b, err := f.BytesN(WKGroup, GroupIDLen)
	if err != nil {
		return nil, err
	}
	copy(w.Group[:], b)
	if w.Epoch, err = f.Uint(WKEpoch, 0, MaxEpoch); err != nil {
		return nil, err
	}
	if b, err = f.BytesN(WKEpochID, 32); err != nil {
		return nil, err
	}
	copy(w.EpochID[:], b)
	a, err := f.Array(WKEntries, MaxEntries)
	if err != nil {
		return nil, err
	}
	w.Entries = make([][]byte, len(a))
	for i, e := range a {
		eb, ok := e.([]byte)
		if !ok {
			return nil, fmt.Errorf("%w: entry", ErrEntry)
		}
		w.Entries[i] = eb
	}
	if err := w.checkEntries(); err != nil {
		return nil, err
	}
	return w, nil
}
