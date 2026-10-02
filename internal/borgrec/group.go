package borgrec

import (
	"bytes"
	"fmt"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/record"
)

// Group record keys.
const (
	GKMagic        = 0
	GKHall         = 1
	GKOrg          = 2
	GKGroup        = 3
	GKSeq          = 4
	GKPrev         = 5
	GKName         = 6
	GKEpoch        = 7
	GKEpochSeq     = 8
	GKCommitment   = 9
	GKAdd          = 10
	GKRemove       = 11
	GKCount        = 12
	GKMembers      = 13
	GKRotateOnJoin = 14
	GKRotateEvery  = 15
	GKCreated      = 16
	gkLast         = GKCreated
)

// Group is one group record: the second field of a group chain token. Each
// token is one transition of the group's membership; a token that begins an
// epoch is that epoch's descriptor.
type Group struct {
	Hall         string           // key 1
	Org          []byte           // key 2
	Group        [GroupIDLen]byte // key 3, random, fixed at creation
	Seq          uint64           // key 4
	Prev         [32]byte         // key 5, txid of the token at seq-1; zero at seq 0
	Name         string           // key 6
	Epoch        uint64           // key 7
	EpochSeq     uint64           // key 8, seq of the token that began this epoch
	Commitment   [32]byte         // key 9, BRC-369 digest of this epoch's key
	Add          [][]byte         // key 10, members added, ascending
	Remove       [][]byte         // key 11, members removed, ascending
	Count        uint64           // key 12, members after this transition
	Members      [32]byte         // key 13, MembersRoot after this transition
	RotateOnJoin bool             // key 14, adding a member also begins an epoch
	RotateEvery  uint64           // key 15, seconds; 0 means no schedule
	Created      uint64           // key 16, Unix seconds, a claim
	Extra        cbor.Map         // integer keys above 16, preserved and ignored
}

// BeginsEpoch reports whether this token began its epoch.
func (g *Group) BeginsEpoch() bool { return g.EpochSeq == g.Seq }

// Validate applies every rule that needs only the record itself.
func (g *Group) Validate() error {
	if err := record.CheckExtra(g.Extra, gkLast, gkLast+1); err != nil {
		return err
	}
	if err := CheckHall(g.Hall); err != nil {
		return err
	}
	if err := CheckOrgKey(g.Org); err != nil {
		return err
	}
	if err := record.InRange(g.Seq, 0, MaxSafe, "seq"); err != nil {
		return err
	}
	if err := checkName(g.Name); err != nil {
		return err
	}
	if err := record.InRange(g.Epoch, 0, MaxEpoch, "epoch"); err != nil {
		return err
	}
	if err := record.InRange(g.EpochSeq, 0, MaxSafe, "epochSeq"); err != nil {
		return err
	}
	for _, l := range [][][]byte{g.Add, g.Remove} {
		if len(l) > MaxDelta {
			return ErrList
		}
		for _, k := range l {
			if err := CheckIdentity(k); err != nil {
				return err
			}
		}
		if err := sortedDistinctKeys(l); err != nil {
			return err
		}
	}
	if err := record.InRange(g.Count, 0, MaxMembers, "count"); err != nil {
		return err
	}
	if err := record.InRange(g.RotateEvery, 0, MaxEpoch, "rotateEvery"); err != nil {
		return err
	}
	if err := record.InRange(g.Created, 1, MaxTime, "created"); err != nil {
		return err
	}
	return g.cross()
}

// cross is refusal step 7: the rules across fields.
func (g *Group) cross() error {
	for _, a := range g.Add {
		for _, r := range g.Remove {
			if bytes.Equal(a, r) {
				return fmt.Errorf("%w: a member both added and removed", ErrList)
			}
		}
	}
	for _, a := range g.Add {
		if bytes.Equal(a, g.Org) || IsAnyone(a) {
			return ErrMembers
		}
	}
	if (g.Seq == 0) != zero32(g.Prev[:]) {
		return fmt.Errorf("%w: prev is zero exactly at seq 0", ErrChain)
	}
	if g.EpochSeq > g.Seq {
		return fmt.Errorf("%w: epochSeq after seq", ErrChain)
	}
	if g.Seq == 0 && (g.Epoch != 0 || g.EpochSeq != 0) {
		return fmt.Errorf("%w: the first token begins epoch 0", ErrChain)
	}
	if g.Seq == 0 && (len(g.Remove) != 0 || g.Count != uint64(len(g.Add))) {
		return fmt.Errorf("%w: the first token adds its whole membership", ErrCount)
	}
	return nil
}

// Encode writes the group record in canonical CBOR.
func (g *Group) Encode() ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	m := cbor.Map{
		{Key: uint64(GKMagic), Val: MagicGroup},
		{Key: uint64(GKHall), Val: g.Hall},
		{Key: uint64(GKOrg), Val: g.Org},
		{Key: uint64(GKGroup), Val: append([]byte(nil), g.Group[:]...)},
		{Key: uint64(GKSeq), Val: g.Seq},
		{Key: uint64(GKPrev), Val: append([]byte(nil), g.Prev[:]...)},
		{Key: uint64(GKName), Val: g.Name},
		{Key: uint64(GKEpoch), Val: g.Epoch},
		{Key: uint64(GKEpochSeq), Val: g.EpochSeq},
		{Key: uint64(GKCommitment), Val: append([]byte(nil), g.Commitment[:]...)},
		{Key: uint64(GKAdd), Val: valuesBytes(g.Add)},
		{Key: uint64(GKRemove), Val: valuesBytes(g.Remove)},
		{Key: uint64(GKCount), Val: g.Count},
		{Key: uint64(GKMembers), Val: append([]byte(nil), g.Members[:]...)},
		{Key: uint64(GKRotateOnJoin), Val: g.RotateOnJoin},
		{Key: uint64(GKRotateEvery), Val: g.RotateEvery},
		{Key: uint64(GKCreated), Val: g.Created},
	}
	return record.Encode(m, g.Extra, MaxGroupRecord)
}

// DecodeGroup parses a group record in the refusal order.
func DecodeGroup(p []byte) (*Group, error) {
	f, err := decodeRecord(p, MaxGroupRecord, gkLast, MagicGroup)
	if err != nil {
		return nil, err
	}
	g := &Group{Extra: f.Extra}
	if g.Hall, err = f.Text(GKHall); err != nil {
		return nil, err
	}
	if err := CheckHall(g.Hall); err != nil {
		return nil, err
	}
	if g.Org, err = f.BytesN(GKOrg, -1); err != nil {
		return nil, err
	}
	if err := CheckOrgKey(g.Org); err != nil {
		return nil, err
	}
	b, err := f.BytesN(GKGroup, GroupIDLen)
	if err != nil {
		return nil, err
	}
	copy(g.Group[:], b)
	if g.Seq, err = f.Uint(GKSeq, 0, MaxSafe); err != nil {
		return nil, err
	}
	if b, err = f.BytesN(GKPrev, 32); err != nil {
		return nil, err
	}
	copy(g.Prev[:], b)
	if g.Name, err = f.Text(GKName); err != nil {
		return nil, err
	}
	if err := checkName(g.Name); err != nil {
		return nil, err
	}
	if g.Epoch, err = f.Uint(GKEpoch, 0, MaxEpoch); err != nil {
		return nil, err
	}
	if g.EpochSeq, err = f.Uint(GKEpochSeq, 0, MaxSafe); err != nil {
		return nil, err
	}
	if b, err = f.BytesN(GKCommitment, 32); err != nil {
		return nil, err
	}
	copy(g.Commitment[:], b)
	if g.Add, err = f.listKeys(GKAdd, MaxDelta); err != nil {
		return nil, err
	}
	if g.Remove, err = f.listKeys(GKRemove, MaxDelta); err != nil {
		return nil, err
	}
	if g.Count, err = f.Uint(GKCount, 0, MaxMembers); err != nil {
		return nil, err
	}
	if b, err = f.BytesN(GKMembers, 32); err != nil {
		return nil, err
	}
	copy(g.Members[:], b)
	if g.RotateOnJoin, err = f.Bool(GKRotateOnJoin); err != nil {
		return nil, err
	}
	if g.RotateEvery, err = f.Uint(GKRotateEvery, 0, MaxEpoch); err != nil {
		return nil, err
	}
	if g.Created, err = f.Uint(GKCreated, 1, MaxTime); err != nil {
		return nil, err
	}
	if err := g.cross(); err != nil {
		return nil, err
	}
	return g, nil
}

// CheckGroupTransition is the admission rule a group token meets against the
// token it spends (read from the parent the token's BEEF carries). It is what makes
// removal a rekey at every host: a transition that removes anyone, or adds
// anyone while the predecessor asks for rotation on join, begins the next
// epoch with a different commitment, and no other transition may change the
// epoch's key.
func CheckGroupTransition(prev *Group, prevTxid [32]byte, next *Group) error {
	if next.Seq == 0 {
		return fmt.Errorf("%w: seq 0 spends no token", ErrTransition)
	}
	if next.Hall != prev.Hall || !bytes.Equal(next.Org, prev.Org) || next.Group != prev.Group ||
		next.Seq != prev.Seq+1 || next.Prev != prevTxid {
		return fmt.Errorf("%w: not the successor of the token spent", ErrTransition)
	}
	mustRotate := len(next.Remove) > 0 || (prev.RotateOnJoin && len(next.Add) > 0)
	switch next.Epoch {
	case prev.Epoch:
		if mustRotate {
			return ErrRotation
		}
		if next.Commitment != prev.Commitment || next.EpochSeq != prev.EpochSeq {
			return fmt.Errorf("%w: the epoch's key changed without a new epoch", ErrRotation)
		}
	case prev.Epoch + 1:
		if next.Commitment == prev.Commitment {
			return fmt.Errorf("%w: a new epoch reuses the key", ErrRotation)
		}
		if next.EpochSeq != next.Seq {
			return fmt.Errorf("%w: a new epoch begins at this seq", ErrRotation)
		}
	default:
		return fmt.Errorf("%w: epoch skips", ErrTransition)
	}
	if prev.Count+uint64(len(next.Add)) < uint64(len(next.Remove)) ||
		prev.Count+uint64(len(next.Add))-uint64(len(next.Remove)) != next.Count {
		return ErrCount
	}
	return nil
}

// Fold applies a transition to a member set, refusing a removal of a
// non-member or an addition of a member. The result's MembersRoot must equal
// the token's Members; that is a reader's and an index's check, never an
// admission rule, because a host cannot hold the set while it admits.
func Fold(set map[string]bool, next *Group) error {
	for _, k := range next.Remove {
		if !set[string(k)] {
			return fmt.Errorf("%w: removes a non-member", ErrCount)
		}
		delete(set, string(k))
	}
	for _, k := range next.Add {
		if set[string(k)] {
			return fmt.Errorf("%w: adds a member", ErrCount)
		}
		set[string(k)] = true
	}
	if uint64(len(set)) != next.Count {
		return ErrCount
	}
	keys := make([][]byte, 0, len(set))
	for k := range set {
		keys = append(keys, []byte(k))
	}
	if MembersRoot(keys) != next.Members {
		return fmt.Errorf("%w: members root", ErrCount)
	}
	return nil
}

// RejoinUnrotated reports a token that adds a key an earlier token of the
// group removed without beginning an epoch: the index's rejoin-unrotated
// status (spec section 4.2). removed holds every key any earlier token of the
// group removed. It is never an admission rule, because the predecessor's
// record names no removed keys.
func RejoinUnrotated(removed map[string]bool, next *Group) bool {
	if next.BeginsEpoch() {
		return false
	}
	for _, k := range next.Add {
		if removed[string(k)] {
			return true
		}
	}
	return false
}
