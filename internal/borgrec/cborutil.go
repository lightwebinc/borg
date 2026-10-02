package borgrec

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// fields is a decoded record with this package's own readers beside the
// shared ones.
type fields struct{ *record.Fields }

func decodeRecord(b []byte, max int, last uint64, magic []byte) (*fields, error) {
	f, err := record.Decode(b, max, last, magic)
	if err != nil {
		return nil, err
	}
	return &fields{f}, nil
}

// textMap returns key k as a map with text keys whose values are byte
// strings. The names are checked by the caller.
func (f *fields) textMap(k uint64) (map[string][]byte, error) {
	v, err := f.Need(k)
	if err != nil {
		return nil, err
	}
	m, ok := v.(cbor.Map)
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrType, k)
	}
	if len(m) > len(GrantFieldNames) {
		return nil, fmt.Errorf("%w: key %d", ErrFields, k)
	}
	out := make(map[string][]byte, len(m))
	for _, p := range m {
		name, ok := p.Key.(string)
		if !ok {
			return nil, fmt.Errorf("%w: key %d", ErrFields, k)
		}
		b, ok := p.Val.([]byte)
		if !ok {
			return nil, fmt.Errorf("%w: key %d", ErrFields, k)
		}
		out[name] = b
	}
	return out, nil
}

// checkName is the display-name rule: 1 to MaxName bytes of UTF-8 text.
// Rendering filters it; the record only bounds it.
func checkName(s string) error {
	if len(s) < 1 || len(s) > MaxName || !utf8.ValidString(s) {
		return ErrName
	}
	return nil
}

// sortedDistinctKeys holds a key list to strictly ascending byte order.
func sortedDistinctKeys(a [][]byte) error {
	for i := 1; i < len(a); i++ {
		if bytes.Compare(a[i-1], a[i]) >= 0 {
			return ErrList
		}
	}
	return nil
}

// listKeys decodes key k as an array of at most max canonical identity keys
// in strictly ascending byte order.
func (f *fields) listKeys(k uint64, max int) ([][]byte, error) {
	a, err := f.Array(k, max)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(a))
	for i, e := range a {
		b, ok := e.([]byte)
		if !ok {
			return nil, fmt.Errorf("%w: key %d", ErrList, k)
		}
		if err := CheckIdentity(b); err != nil {
			return nil, err
		}
		out[i] = b
	}
	if err := sortedDistinctKeys(out); err != nil {
		return nil, fmt.Errorf("%w: key %d", err, k)
	}
	return out, nil
}

func valuesBytes(a [][]byte) []cbor.Value {
	out := make([]cbor.Value, len(a))
	for i := range a {
		out[i] = append([]byte(nil), a[i]...)
	}
	return out
}

func zero32(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
