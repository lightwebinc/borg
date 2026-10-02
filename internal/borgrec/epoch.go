package borgrec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/keyed"
)

// WrapVersion is the BRC-78 version prefix borg writes and requires: the
// bytes go-sdk and ts-sdk write. BRC-78's hex example prints 10334242 and
// BRC-369's release vector carries 33104242; neither is accepted.
var WrapVersion = []byte{0x42, 0x42, 0x10, 0x33}

// Epoch-key errors, a reader's (BRC-369 section 5.3), never a host's.
var (
	ErrScalar     = keyed.ErrScalar
	ErrCommitment = keyed.ErrCommitment
	ErrUnwrap     = keyed.ErrUnwrap
	ErrToken      = errors.New("borgrec: the wrap's epoch id is not the record of the token that began its epoch")
)

// CheckEpochToken is the member's check 1 (spec section 6.4): the group
// record of the token that began the wrap's epoch hashes to the wrap's
// epoch id and names the same hall, organisation, group and epoch. The
// token's proof against the member's headers is checked by the caller.
func CheckEpochToken(groupRecord []byte, w *WrapRecord) (*Group, error) {
	g, err := DecodeGroup(groupRecord)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToken, err)
	}
	if EpochID(groupRecord) != w.EpochID || !g.BeginsEpoch() || g.Epoch != w.Epoch ||
		g.Hall != w.Hall || !bytes.Equal(g.Org, w.Org) || g.Group != w.Group {
		return nil, ErrToken
	}
	return g, nil
}

// SampleKey draws an epoch key (BRC-369 section 2.1 rule 1).
func SampleKey(r io.Reader) ([32]byte, error) { return keyed.SampleKey(r) }

// CheckScalar refuses a 32-byte big-endian value outside [1, n-1].
func CheckScalar(k [32]byte) error { return keyed.CheckScalar(k) }

// SymmetricKey is BRC-369 section 2.1 rule 3.
func SymmetricKey(k [32]byte) [32]byte { return keyed.SymmetricKey(k) }

// Commitment is BRC-369 section 2.2's digest form.
func Commitment(k [32]byte) [32]byte { return keyed.Commitment(k) }

// EpochID is the identifier of an epoch: SHA-256 of the encoded group record
// of the token that began it, BRC-369 section 4.2 rule 3's content id with
// the record in the descriptor's place.
func EpochID(groupRecord []byte) [32]byte { return sha256.Sum256(groupRecord) }

// ReleaseKeyID is BRC-369 section 5.2's key id bytes:
// SHA-256(epoch id || epoch as four bytes big-endian).
func ReleaseKeyID(epochID [32]byte, epoch uint32) [32]byte {
	var e [4]byte
	binary.BigEndian.PutUint32(e[:], epoch)
	return sha256.Sum256(append(epochID[:], e[:]...))
}

// ReleaseKeyIDString is the key id as the BRC-43 invoice carries it:
// standard padded base64.
func ReleaseKeyIDString(epochID [32]byte, epoch uint32) string {
	id := ReleaseKeyID(epochID, epoch)
	return base64.StdEncoding.EncodeToString(id[:])
}

// ReleaseInvoice is the BRC-43 invoice number the wrap is derived under.
func ReleaseInvoice(epochID [32]byte, epoch uint32) string {
	return "2-" + ReleaseProtocolName + "-" + ReleaseKeyIDString(epochID, epoch)
}

// releaseArgs are the BRC-100 encryption arguments for a wrap.
func releaseArgs(epochID [32]byte, epoch uint32, counterparty *ec.PublicKey) wallet.EncryptionArgs {
	return wallet.EncryptionArgs{
		ProtocolID:   ReleaseProtocol,
		KeyID:        ReleaseKeyIDString(epochID, epoch),
		Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: counterparty},
	}
}

// frame writes the BRC-78 serialization around a BRC-2 ciphertext.
func frame(org, member []byte, keyID [32]byte, ciphertext []byte) []byte {
	out := make([]byte, 0, WrapLen)
	out = append(out, WrapVersion...)
	out = append(out, org...)
	out = append(out, member...)
	out = append(out, keyID[:]...)
	return append(out, ciphertext...)
}

// Wrap releases the epoch key k to member under BRC-369 section 5.2, through
// the organisation's wallet: the wallet's BRC-2 ciphertext under
// [2, "keyed content release"], the derived key id and counterparty member,
// framed as BRC-78 (version, sender, recipient, key id, ciphertext).
func Wrap(ctx context.Context, w wallet.Interface, org []byte, member *ec.PublicKey, epochID [32]byte, epoch uint32, k [32]byte) ([]byte, error) {
	if err := CheckScalar(k); err != nil {
		return nil, err
	}
	if IsAnyone(member.Compressed()) {
		return nil, fmt.Errorf("%w: never to the anyone key", ErrEntry)
	}
	res, err := w.Encrypt(ctx, wallet.EncryptArgs{EncryptionArgs: releaseArgs(epochID, epoch, member), Plaintext: k[:]}, "")
	if err != nil {
		return nil, err
	}
	out := frame(org, member.Compressed(), ReleaseKeyID(epochID, epoch), res.Ciphertext)
	if len(out) != WrapLen {
		return nil, fmt.Errorf("%w: length %d", ErrEntry, len(out))
	}
	return out, nil
}

// WrapWithIV is Wrap with a caller's 32-byte IV in place of a random one,
// for golden vectors only: the symmetric key is the one the wallet derives
// (BRC-42 child keys, the raw x-coordinate of their shared point), and the
// result decrypts through any wallet's Decrypt.
func WrapWithIV(orgKey *ec.PrivateKey, member *ec.PublicKey, epochID [32]byte, epoch uint32, k [32]byte, iv [32]byte) ([]byte, error) {
	sk, err := wallet.NewKeyDeriver(orgKey).DeriveSymmetricKey(ReleaseProtocol, ReleaseKeyIDString(epochID, epoch),
		wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: member})
	if err != nil {
		return nil, err
	}
	ct, err := keyed.SymmetricSeal(sk.ToBytes(), iv, k[:])
	if err != nil {
		return nil, err
	}
	return frame(orgKey.PubKey().Compressed(), member.Compressed(), ReleaseKeyID(epochID, epoch), ct), nil
}

// Entry is a parsed wrap: the BRC-78 header and the ciphertext.
type Entry struct {
	Sender     []byte
	Recipient  []byte
	KeyID      [32]byte
	Ciphertext []byte
}

// ParseEntry splits one wrap without checking it against a record.
func ParseEntry(b []byte) (*Entry, error) {
	if len(b) != WrapLen || !bytes.Equal(b[:4], WrapVersion) {
		return nil, ErrEntry
	}
	e := &Entry{Sender: b[4:37], Recipient: b[37:70], Ciphertext: b[102:]}
	copy(e.KeyID[:], b[70:102])
	return e, nil
}

// CheckEntry is the host's rule for one wrap of a wrap record: exactly
// WrapLen bytes, the version, the organisation as sender, a canonical
// recipient that is neither the organisation nor the anyone key, and the key
// id BRC-369 derives for the record's epoch.
func CheckEntry(b, org []byte, epochID [32]byte, epoch uint32) (*Entry, error) {
	e, err := ParseEntry(b)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(e.Sender, org) {
		return nil, fmt.Errorf("%w: sender is not the organisation", ErrEntry)
	}
	if CheckIdentity(e.Recipient) != nil || bytes.Equal(e.Recipient, org) || IsAnyone(e.Recipient) {
		return nil, fmt.Errorf("%w: recipient", ErrEntry)
	}
	if e.KeyID != ReleaseKeyID(epochID, epoch) {
		return nil, fmt.Errorf("%w: key id", ErrEntry)
	}
	return e, nil
}

// Unwrap is the member's side of BRC-369 sections 5.2 and 5.3: it decrypts
// its wrap through its own wallet (counterparty the organisation), refuses a
// value that is not a scalar, and checks it against the epoch's commitment
// before anything else uses it.
func Unwrap(ctx context.Context, w wallet.Interface, wrap []byte, org *ec.PublicKey, epochID [32]byte, epoch uint32, commitment [32]byte) ([32]byte, error) {
	var k [32]byte
	e, err := CheckEntry(wrap, org.Compressed(), epochID, epoch)
	if err != nil {
		return k, err
	}
	res, err := w.Decrypt(ctx, wallet.DecryptArgs{EncryptionArgs: releaseArgs(epochID, epoch, org), Ciphertext: e.Ciphertext}, "")
	if err != nil {
		return k, fmt.Errorf("%w: %v", ErrUnwrap, err)
	}
	return keyed.CheckOpened(res.Plaintext, commitment)
}

// HexKey is a key's lowercase hex, the spelling questions and vectors use.
func HexKey(k []byte) string { return hex.EncodeToString(k) }
