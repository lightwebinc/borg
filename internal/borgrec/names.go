package borgrec

import (
	"bytes"
	"crypto/rand"
	"io"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/guard"
)

// CheckIdentity refuses anything but a canonical compressed secp256k1 key
// (bcommon guard.ParsePubKey): 33 bytes, prefix 0x02 or 0x03, an x-coordinate
// below the field prime, and a point on the curve. Accepting an alias would
// let two byte strings name one organisation or member.
func CheckIdentity(k []byte) error {
	if _, err := guard.ParsePubKey(k); err != nil {
		return ErrIdentity
	}
	return nil
}

// CheckOrgKey is the key rule for an organisation: a canonical identity key
// that is not the anyone key, whose private key everyone knows.
func CheckOrgKey(k []byte) error {
	if CheckIdentity(k) != nil || IsAnyone(k) {
		return ErrIdentity
	}
	return nil
}

// AnyoneKey is the public key of BRC-43's "anyone" counterparty, the
// generator point (private key 1). Nothing is ever released to it (BRC-369
// section 5.2 rule 4), so it is never a member and never a wrap recipient.
func AnyoneKey() []byte {
	one := make([]byte, 32)
	one[31] = 1
	k, _ := ec.PrivateKeyFromBytes(one)
	return k.PubKey().Compressed()
}

// IsAnyone reports whether k is the anyone key.
func IsAnyone(k []byte) bool { return bytes.Equal(k, AnyoneKey()) }

// CheckHallName applies the grammar of a hall's readable part: 1 to
// MaxHallName bytes of lowercase ASCII letters and single underscores,
// starting and ending with a letter.
func CheckHallName(s string) error {
	if len(s) == 0 || len(s) > MaxHallName {
		return ErrHall
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c == '_':
			if i == 0 || i == len(s)-1 || s[i-1] == '_' {
				return ErrHall
			}
		default:
			return ErrHall
		}
	}
	return nil
}

// CheckHall applies the hall identifier grammar: <name>_<suffix>, where name
// meets CheckHallName and suffix is exactly SuffixLen lowercase ASCII
// letters. TopicPrefix + hall is then a BRC-87 topic manager name.
func CheckHall(s string) error {
	if len(s) < MinHall || len(s) > MaxHall {
		return ErrHall
	}
	cut := len(s) - SuffixLen - 1
	if s[cut] != '_' {
		return ErrHall
	}
	for i := cut + 1; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return ErrHall
		}
	}
	return CheckHallName(s[:cut])
}

// SplitHall returns a hall identifier's readable part and suffix. The
// readable part alone never identifies a hall.
func SplitHall(s string) (name, suffix string, err error) {
	if err := CheckHall(s); err != nil {
		return "", "", err
	}
	cut := len(s) - SuffixLen - 1
	return s[:cut], s[cut+1:], nil
}

// NewHall creates a hall identifier for name: name, an underscore, and
// SuffixLen letters drawn uniformly at random from a to z by r
// (crypto/rand.Reader when r is nil). Whoever creates the hall calls it
// once; the suffix never changes.
func NewHall(name string, r io.Reader) (string, error) {
	if err := CheckHallName(name); err != nil {
		return "", err
	}
	if r == nil {
		r = rand.Reader
	}
	suffix := make([]byte, SuffixLen)
	var b [1]byte
	for i := 0; i < SuffixLen; {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		// 234 = 9 * 26: rejecting 234 to 255 keeps every letter equally
		// likely.
		if b[0] >= 234 {
			continue
		}
		suffix[i] = 'a' + b[0]%26
		i++
	}
	return name + "_" + string(suffix), nil
}

// Topic is the topic manager name of a hall.
func Topic(hall string) string { return TopicPrefix + hall }

// NewGroupID draws a group identifier: GroupIDLen bytes uniformly at random
// from r (crypto/rand.Reader when r is nil).
func NewGroupID(r io.Reader) ([GroupIDLen]byte, error) {
	var id [GroupIDLen]byte
	if r == nil {
		r = rand.Reader
	}
	_, err := io.ReadFull(r, id[:])
	return id, err
}
