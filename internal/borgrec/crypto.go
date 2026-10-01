package borgrec

import (
	"crypto/sha256"
	"errors"
	"sort"

	aesgcm "github.com/bsv-blockchain/go-sdk/primitives/aesgcm"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/commit"
)

// gcmSeal is BRC-2's symmetric form as go-sdk writes it: AES-256-GCM with a
// 32-byte IV, written IV || ciphertext || tag. Only vectors pass a fixed IV.
func gcmSeal(key []byte, iv [32]byte, plaintext []byte) ([]byte, error) {
	ct, tag, err := aesgcm.AESGCMEncrypt(plaintext, key, iv[:], []byte{})
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 32+len(ct)+16)
	out = append(out, iv[:]...)
	out = append(out, ct...)
	return append(out, tag...), nil
}

// gcmOpen reverses gcmSeal through go-sdk's own symmetric key.
func gcmOpen(key, sealed []byte) ([]byte, error) {
	if len(sealed) < 48 {
		return nil, errors.New("borgrec: ciphertext too short")
	}
	return ec.NewSymmetricKey(key).Decrypt(sealed)
}

// MembersRoot is the RFC 6962 root (bcommon commit) a group record commits
// its members to: the leaves are SHA-256 of each 33-byte identity key, in
// ascending order of the keys' bytes. The empty set's root is SHA-256 of
// nothing.
func MembersRoot(keys [][]byte) [32]byte {
	sorted := make([][]byte, len(keys))
	copy(sorted, keys)
	sort.Slice(sorted, func(i, j int) bool { return string(sorted[i]) < string(sorted[j]) })
	leaves := make([][32]byte, len(sorted))
	for i, k := range sorted {
		leaves[i] = sha256.Sum256(k)
	}
	return commit.Root(leaves)
}

// RollRoot is the RFC 6962 root an organisation record commits its roll to:
// the leaves are the grant commitments (carrier txids, hash byte order) in
// ascending byte order.
func RollRoot(grants [][32]byte) [32]byte {
	sorted := make([][32]byte, len(grants))
	copy(sorted, grants)
	sortCommitments(sorted)
	return commit.Root(sorted)
}

func sortCommitments(a [][32]byte) {
	sort.Slice(a, func(i, j int) bool { return string(a[i][:]) < string(a[j][:]) })
}
