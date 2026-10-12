package borgrec

import (
	"crypto/sha256"
	"sort"

	"github.com/lightwebinc/bcommon/commit"
)

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

// RollRoot is the RFC 6962 root an organization record commits its roll to:
// the leaves are the grant commitments (carrier txids, hash byte order) in
// ascending byte order.
func RollRoot(grants [][32]byte) [32]byte {
	sorted := make([][32]byte, len(grants))
	copy(sorted, grants)
	sortCommitments(sorted)
	return commit.Root(sorted)
}

// PolicyRoot is the RFC 6962 root an organization record commits its policy
// set to, computed exactly as RollRoot is over the grant commitments: the
// leaves are the policy carriers' commitments in ascending byte order. The
// two roots are separate so that a change of membership and a change of
// policy are told apart, and one chain orders both.
func PolicyRoot(policies [][32]byte) [32]byte { return RollRoot(policies) }

func sortCommitments(a [][32]byte) {
	sort.Slice(a, func(i, j int) bool { return string(a[i][:]) < string(a[j][:]) })
}
