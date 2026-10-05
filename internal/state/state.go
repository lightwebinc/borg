// Package state is an identity's durable record in its home: the funding
// trees its carriers and chains spend, the organisation it runs (its own
// chain, its grants, its groups and every epoch key it sampled), the
// objects persisted before they are published, the sweeps it built, and,
// for a member, the epoch keys it opened, the grants it holds and the
// latest token of each chain it has verified.
//
// It is written before anything leaves the machine. A carrier is persisted
// with the funding output it spends marked used, and a transition with the
// epoch key it samples, before either is sent, so a run that stops part way
// sends the same bytes again and never makes a second carrier on one
// funding output or a second key for one epoch. The file is written
// atomically (a temporary file, fsync, rename, fsync of the directory) at
// mode 0600, and the home is locked while a command writes.
//
// The file holds secrets: every epoch key the organisation sampled and
// every epoch key a member opened, beside the identity key the wallet keeps
// in the same directory. The home is the wallet's encrypted storage's place
// in this command, so it is kept at mode 0700 and backed up as a secret.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/payee"
)

// Version is the state file's format version.
const Version = 1

// File is the state file's name in the home.
const File = "state.json"

// The kinds of object a home publishes.
const (
	KindGrant      = "grant"
	KindWrap       = "wrap"
	KindOrgToken   = "org-token"
	KindGroupToken = "group-token"
	KindSweep      = "sweep"
)

// The kinds of funding tree, each under its own key and basket (spec
// section 9.4).
const (
	TreeGrant  = "grant"
	TreeRecord = "record"
	TreeChain  = "chain"
)

// TreeKinds are the tree kinds in a fixed order.
var TreeKinds = []string{TreeGrant, TreeRecord, TreeChain}

// State is one identity's state.
type State struct {
	Version int `json:"version"`
	// Identity is the identity key, compressed hex.
	Identity string `json:"identity"`
	// Halls are the halls this home created (hall new).
	Halls []string `json:"halls,omitempty"`

	// Trees are the funding trees by kind.
	Trees map[string]*TreeSet `json:"trees,omitempty"`

	// PendingTrees are the funding trees signed for a pool coin that the
	// home has not adopted yet, each with its kind and the coin it spends:
	// the record made before a tree reaches the settlement leg, so a run
	// that stops before the tree is adopted leaves what the next command
	// needs to ask the node what became of it. Adopting a tree drops its
	// record in the same save.
	PendingTrees []PendingTree `json:"pendingTrees,omitempty"`

	// Org is the organisation this home runs, once org create began.
	Org *Org `json:"org,omitempty"`

	// Outbox holds every object persisted and not yet confirmed published,
	// oldest first. A command that finds one publishes the same bytes
	// before it builds anything new.
	Outbox []Pending `json:"outbox,omitempty"`
	// Sweeps are the sweeps this home built.
	Sweeps []Sweep `json:"sweeps,omitempty"`

	// Keys are the epoch keys this identity opened as a member.
	Keys []EpochKey `json:"keys,omitempty"`
	// Held are the grants to this identity it read and verified, kept
	// whole: the certificate, never a summary of it (spec section 5.5).
	Held []HeldGrant `json:"held,omitempty"`
	// Seen is the latest token of each chain this identity verified: a
	// host that answers an older one is answering an old state.
	Seen []Seen `json:"seen,omitempty"`
	// Evidence is the last positive evidence, per group, that its latest
	// token's output was unspent (spec section 6.5).
	Evidence []Evidence `json:"evidence,omitempty"`

	// Payments are the payments this identity made for priced questions
	// that have not mined yet, each as the BEEF the host was handed: the
	// next payment may spend the change of one, and then carries it.
	Payments []Payment `json:"payments,omitempty"`

	// Book is what payee settle did for this identity as a host's payee:
	// the payments it settled ("settled") and those the network refused
	// for good ("unsettleable").
	payee.Book

	// Taking is the coin pool as it stood when a command that spends from
	// it last saved this state: the record made before a coin is taken. A
	// coin leaves the pool's own file the moment it is taken, before the
	// transaction that spends it is recorded here, so a run that stops in
	// between leaves a coin in neither. The next command finds it in this
	// list and in no transaction the home records, and puts it back when
	// the node shows it unspent. It is empty whenever no command is
	// running.
	Taking []bwallet.Output `json:"taking,omitempty"`

	path       string
	beforeSave func()
}

// OnSave sets what runs before every Save: a publisher records the pool
// with it. Nil clears it.
func (s *State) OnSave(f func()) { s.beforeSave = f }

// PendingTree is a funding tree signed for a pool coin and not yet
// adopted, keyed by the tree's txid.
type PendingTree struct {
	// Kind is the tree's kind.
	Kind string `json:"kind"`
	// Tree is the record the tree is adopted as, without a proof.
	Tree funding.Tree `json:"tree"`
	// Coin is the fee coin the tree spends, as the pool held it.
	Coin bwallet.Output `json:"coin"`
	// ReturnedOnce is set when a command found the tree absent from the
	// node and the coin unspent, and put the coin back: the record is kept
	// for one more command, in case the tree reaches the node after all.
	ReturnedOnce bool `json:"returned_once,omitempty"`
}

// KeepPendingTree records p, replacing a record of the same tree.
func (s *State) KeepPendingTree(p PendingTree) {
	for i := range s.PendingTrees {
		if s.PendingTrees[i].Tree.Txid == p.Tree.Txid {
			s.PendingTrees[i] = p
			return
		}
	}
	s.PendingTrees = append(s.PendingTrees, p)
}

// PendingTreeOf finds the record of the tree txid.
func (s *State) PendingTreeOf(txid string) *PendingTree {
	for i := range s.PendingTrees {
		if s.PendingTrees[i].Tree.Txid == txid {
			return &s.PendingTrees[i]
		}
	}
	return nil
}

// DropPendingTree drops the record of the tree txid, and reports whether
// there was one.
func (s *State) DropPendingTree(txid string) bool {
	n := len(s.PendingTrees)
	s.PendingTrees = slices.DeleteFunc(s.PendingTrees, func(p PendingTree) bool { return p.Tree.Txid == txid })
	return len(s.PendingTrees) != n
}

// MintedAhead finds txid among the trees minted ahead and not adopted, of
// every kind.
func (s *State) MintedAhead(txid string) *funding.Tree {
	for _, ts := range s.Trees {
		if ts == nil {
			continue
		}
		for i := range ts.Ahead {
			if ts.Ahead[i].Txid == txid {
				return &ts.Ahead[i]
			}
		}
	}
	return nil
}

// UnsettledTrees are the records of trees the home records nowhere else:
// those a command has yet to ask the node about. A tree minted ahead and
// recorded in its kind's Ahead keeps its record until it is adopted, and
// is settled.
func (s *State) UnsettledTrees() []PendingTree {
	var out []PendingTree
	for _, p := range s.PendingTrees {
		if s.MintedAhead(p.Tree.Txid) == nil {
			out = append(out, p)
		}
	}
	return out
}

// TreeSet is one kind's funding trees: the one carriers spend now, every
// tree minted, and the trees minted ahead and never adopted when a run
// ended (on the chain, in no other list, kept so a sweep finds them).
type TreeSet struct {
	Tree  *funding.Tree  `json:"tree,omitempty"`
	Trees []funding.Tree `json:"trees,omitempty"`
	Ahead []funding.Tree `json:"ahead,omitempty"`
}

// Set is the tree set of kind, made when absent.
func (s *State) Set(kind string) *TreeSet {
	if s.Trees == nil {
		s.Trees = map[string]*TreeSet{}
	}
	ts := s.Trees[kind]
	if ts == nil {
		ts = &TreeSet{}
		s.Trees[kind] = ts
	}
	return ts
}

// TreeOf finds a funding tree of any kind the home recorded, and its kind.
func (s *State) TreeOf(txid string) (*funding.Tree, string, bool) {
	for _, kind := range TreeKinds {
		ts := s.Trees[kind]
		if ts == nil {
			continue
		}
		if ts.Tree != nil && ts.Tree.Txid == txid {
			return ts.Tree, kind, true
		}
		for i := range ts.Trees {
			if ts.Trees[i].Txid == txid {
				return &ts.Trees[i], kind, true
			}
		}
		for i := range ts.Ahead {
			if ts.Ahead[i].Txid == txid {
				return &ts.Ahead[i], kind, true
			}
		}
	}
	return nil, "", false
}

// Org is the organisation this home runs.
type Org struct {
	Hall   string `json:"hall"`
	Name   string `json:"name"`
	Opaque bool   `json:"opaque,omitempty"`
	// Tokens are its chain, mined, in seq order.
	Tokens []Token `json:"tokens,omitempty"`
	// Grants are every grant it issued.
	Grants []Grant `json:"grants,omitempty"`
	// Groups are its groups.
	Groups []Group `json:"groups,omitempty"`
	// Pending is a transition built and persisted, not yet known mined.
	Pending *Transition `json:"pending,omitempty"`
}

// Token is one mined chain token.
type Token struct {
	Seq    uint64 `json:"seq"`
	Txid   string `json:"txid"`
	RawHex string `json:"rawHex"`
	// BumpHex is its proof and Height its block's height.
	BumpHex string `json:"bumpHex"`
	Height  uint32 `json:"height"`
	// Record is the token's record, hex; Vout its token output.
	Record string `json:"record"`
	Vout   uint32 `json:"vout"`
	// Parent is the transaction its predecessor input spends: the token at
	// seq - 1, or at seq 0 the chain tree.
	Parent string `json:"parent"`
	// Published is set once every host named took it.
	Published bool `json:"published,omitempty"`
}

// Grant is one grant the organisation issued.
type Grant struct {
	Subject string `json:"subject"`
	// Txid is the carrier's txid, display order; its commitment C.
	Txid string `json:"txid"`
	// Commitment is C in hash byte order, hex: what a roll holds.
	Commitment string `json:"commitment"`
	// Tree and Vout are the grant-tree output the carrier spends: its
	// revocation outpoint.
	Tree   string `json:"tree"`
	Vout   uint32 `json:"vout"`
	Role   string `json:"role"`
	Scope  string `json:"scope"`
	Expiry string `json:"expiry"`
	// Record is the grant record, hex; Beef its carrier as published.
	Record string `json:"record"`
	Beef   string `json:"beef"`
	// Admitted is set once an organisation token admits it; Revoked once
	// one revokes it, with that token's seq.
	Admitted bool   `json:"admitted,omitempty"`
	Revoked  bool   `json:"revoked,omitempty"`
	RevSeq   uint64 `json:"revSeq,omitempty"`
	// Swept is the sweep that spent its revocation outpoint.
	Swept string `json:"swept,omitempty"`
}

// Group is one of the organisation's groups.
type Group struct {
	// ID is the group identifier, 32 lowercase hex digits.
	ID           string `json:"id"`
	Name         string `json:"name"`
	RotateOnJoin bool   `json:"rotateOnJoin,omitempty"`
	RotateEvery  uint64 `json:"rotateEvery,omitempty"`
	// Tokens are its chain, mined, in seq order.
	Tokens []Token `json:"tokens,omitempty"`
	// Epochs are every epoch it began, with the key the organisation
	// sampled.
	Epochs []Epoch `json:"epochs,omitempty"`
}

// Epoch is one epoch of a group, as the organisation keeps it.
type Epoch struct {
	Epoch uint32 `json:"epoch"`
	// Seq is the seq of the token that began it.
	Seq uint64 `json:"seq"`
	// Key is the epoch key k, hex: a secret.
	Key        string `json:"key"`
	Commitment string `json:"commitment"`
	EpochID    string `json:"epochId"`
	// Wrapped are the members a wrap of this epoch's key was persisted
	// for.
	Wrapped []string `json:"wrapped,omitempty"`
}

// Transition is a chain token built and persisted before it is sent: the
// bytes, and the epoch key it commits to when it begins an epoch.
type Transition struct {
	// Kind is KindOrgToken or KindGroupToken; Group the group's id.
	Kind  string `json:"kind"`
	Group string `json:"group,omitempty"`
	Seq   uint64 `json:"seq"`
	Txid  string `json:"txid"`
	// RawHex is the signed transaction, Record its token record.
	RawHex string `json:"rawHex"`
	Record string `json:"record"`
	Vout   uint32 `json:"vout"`
	// Parent is the transaction its predecessor input spends, and
	// ParentVout the output.
	Parent     string `json:"parent"`
	ParentVout uint32 `json:"parentVout"`
	// Key is the epoch key a new epoch commits to, hex, and Epoch that
	// epoch; empty when the transition keeps the epoch.
	Key   string `json:"key,omitempty"`
	Epoch uint32 `json:"epoch,omitempty"`
	// Fee is the pool coin that pays its fee, as the pool held it.
	Fee *bwallet.Output `json:"fee,omitempty"`
	// Submitted is set once the settlement leg took it.
	Submitted bool `json:"submitted,omitempty"`
	// Grants are the grant commitments it admits (org tokens), so they are
	// marked admitted once it mines; Revokes those it revokes.
	Grants  []string `json:"grants,omitempty"`
	Revokes []string `json:"revokes,omitempty"`
	// What describes it in notes.
	What string `json:"what,omitempty"`
}

// Pending is one object persisted before it is published.
type Pending struct {
	Kind string `json:"kind"`
	Hall string `json:"hall"`
	Txid string `json:"txid"`
	// Beef is the object's BEEF, hex.
	Beef string `json:"beef"`
	// The lookup that confirms a host holds it, when the host's answer to
	// the submission admitted nothing: for a grant the organisation and
	// subject (member class); for a wrap the organisation, group, epoch
	// and one recipient (wrap class); for a token the organisation, chain
	// and seq (org-seq, group-seq); for a sweep one outpoint it spends.
	Org      string `json:"org,omitempty"`
	Group    string `json:"group,omitempty"`
	Seq      uint64 `json:"seq,omitempty"`
	Epoch    uint32 `json:"epoch,omitempty"`
	Member   string `json:"member,omitempty"`
	Outpoint string `json:"outpoint,omitempty"`
	// NotBefore is the Unix time before which it is not published: a mined
	// object waits for the propagation delay (spec section 7.2).
	NotBefore int64 `json:"notBefore,omitempty"`
}

// Mined reports whether a pending object is a mined one, which every host
// named must take.
func (p Pending) Mined() bool {
	return p.Kind == KindOrgToken || p.Kind == KindGroupToken || p.Kind == KindSweep
}

// Sweep is one mined transaction spending funding-tree outputs: output 0 a
// funding-shaped tombstone, so a host admits it, and every carrier that
// spent one of Vouts retracted (a grant among them revoked) once it mines.
type Sweep struct {
	Tree  string   `json:"tree"`
	Vouts []uint32 `json:"vouts"`
	Hall  string   `json:"hall"`
	Txid  string   `json:"txid"`
	// Adopted is set for a spend this home did not build, or built and
	// gave up on: the node named it as the spender of an output, and the
	// home took it as the sweep it is.
	Adopted bool `json:"adopted,omitempty"`
	// RawHex is the signed sweep; BumpHex and Height are set once it mined.
	RawHex    string `json:"rawHex"`
	BumpHex   string `json:"bumpHex,omitempty"`
	Height    uint32 `json:"height,omitempty"`
	Submitted bool   `json:"submitted,omitempty"`
	// Done is set once it is mined and handed to the outbox.
	Done bool `json:"done,omitempty"`
	// Fee is the pool coin that pays its fee, as the pool held it.
	Fee *bwallet.Output `json:"fee,omitempty"`
	// Failed is why the network refused it, once it did.
	Failed string `json:"failed,omitempty"`
}

// EpochKey is an epoch key this identity opened, checked against the
// epoch's commitment.
type EpochKey struct {
	Hall       string `json:"hall"`
	Org        string `json:"org"`
	Group      string `json:"group"`
	Epoch      uint32 `json:"epoch"`
	EpochID    string `json:"epochId"`
	Commitment string `json:"commitment"`
	// Key is k, hex: a secret.
	Key string `json:"key"`
}

// HeldGrant is a grant to this identity, kept whole.
type HeldGrant struct {
	Hall   string `json:"hall"`
	Org    string `json:"org"`
	Txid   string `json:"txid"`
	Record string `json:"record"`
}

// Seen is the latest token of one chain this identity verified.
type Seen struct {
	Hall  string `json:"hall"`
	Org   string `json:"org"`
	Chain string `json:"chain"`
	Seq   uint64 `json:"seq"`
	Txid  string `json:"txid"`
}

// Evidence is positive evidence that a group's token was the latest: its
// proof verified and its output was unspent, at a time.
type Evidence struct {
	Hall  string `json:"hall"`
	Org   string `json:"org"`
	Group string `json:"group"`
	Seq   uint64 `json:"seq"`
	Txid  string `json:"txid"`
	Epoch uint32 `json:"epoch"`
	// At is when the node answered, Unix seconds.
	At int64 `json:"at"`
	// Source names what said unspent.
	Source string `json:"source"`
}

// Payment is a payment this identity made that has not mined yet.
type Payment struct {
	Txid string `json:"txid"`
	// Beef is the payment as Atomic BEEF, hex, with its ancestry back to
	// each proof.
	Beef string `json:"beef"`
}

// PaymentOf finds an unmined payment of this identity's.
func (s *State) PaymentOf(txid string) *Payment {
	for i := range s.Payments {
		if s.Payments[i].Txid == txid {
			return &s.Payments[i]
		}
	}
	return nil
}

// KeepPayments drops every payment whose txid is not in waiting: it mined,
// and its change needs no ancestry any more. It reports whether any went.
func (s *State) KeepPayments(waiting []string) bool {
	n := len(s.Payments)
	s.Payments = slices.DeleteFunc(s.Payments, func(p Payment) bool { return !slices.Contains(waiting, p.Txid) })
	return len(s.Payments) != n
}

// ErrNoState is a home with no state file yet.
var ErrNoState = errors.New("state: no state in this home")

// Load reads the home's state, or a new one for identity when the home has
// none yet.
func Load(home, identity string) (*State, error) {
	p := filepath.Join(home, File)
	raw, err := os.ReadFile(p) //nolint:gosec // the home's own file
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: Version, Identity: identity, path: p}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state %s: %w", p, err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("state %s: version %d, this borg reads %d", p, s.Version, Version)
	}
	if identity != "" && s.Identity != identity {
		return nil, fmt.Errorf("state %s: belongs to identity %s, not this home's %s", p, s.Identity, identity)
	}
	s.path = p
	return &s, nil
}

// Save writes the state atomically at mode 0600.
func (s *State) Save() error {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(s.path, append(raw, '\n'), 0o600)
}

// AddHall records a hall this home created, once.
func (s *State) AddHall(hall string) {
	if !slices.Contains(s.Halls, hall) {
		s.Halls = append(s.Halls, hall)
	}
}

// Done removes a published object from the outbox.
func (s *State) Done(txid string) {
	s.Outbox = slices.DeleteFunc(s.Outbox, func(p Pending) bool { return p.Txid == txid })
}

// Queued reports whether txid is in the outbox.
func (s *State) Queued(txid string) bool {
	return slices.ContainsFunc(s.Outbox, func(p Pending) bool { return p.Txid == txid })
}

// InFlight is the sweep built and not yet finished, if there is one.
func (s *State) InFlight() *Sweep {
	for i := range s.Sweeps {
		if !s.Sweeps[i].Done && s.Sweeps[i].Failed == "" {
			return &s.Sweeps[i]
		}
	}
	return nil
}

// Group finds one of the organisation's groups by id.
func (o *Org) Group(id string) *Group {
	for i := range o.Groups {
		if o.Groups[i].ID == id {
			return &o.Groups[i]
		}
	}
	return nil
}

// GrantByTxid finds a grant by its carrier's txid.
func (o *Org) GrantByTxid(txid string) *Grant {
	for i := range o.Grants {
		if o.Grants[i].Txid == txid {
			return &o.Grants[i]
		}
	}
	return nil
}

// GrantByCommitment finds a grant by its commitment, hash byte order hex.
func (o *Org) GrantByCommitment(c string) *Grant {
	for i := range o.Grants {
		if o.Grants[i].Commitment == c {
			return &o.Grants[i]
		}
	}
	return nil
}

// Latest is the chain's last token, or nil.
func Latest(ts []Token) *Token {
	if len(ts) == 0 {
		return nil
	}
	return &ts[len(ts)-1]
}

// EpochOf finds a group's epoch.
func (g *Group) EpochOf(e uint32) *Epoch {
	for i := range g.Epochs {
		if g.Epochs[i].Epoch == e {
			return &g.Epochs[i]
		}
	}
	return nil
}

// KeyFor finds an opened epoch key.
func (s *State) KeyFor(hall, org, group string, epoch uint32) *EpochKey {
	for i := range s.Keys {
		k := &s.Keys[i]
		if k.Hall == hall && k.Org == org && k.Group == group && k.Epoch == epoch {
			return k
		}
	}
	return nil
}

// AddKey records an opened epoch key, once.
func (s *State) AddKey(k EpochKey) {
	if s.KeyFor(k.Hall, k.Org, k.Group, k.Epoch) == nil {
		s.Keys = append(s.Keys, k)
	}
}

// SeenOf is the latest token of a chain this identity verified, or nil.
func (s *State) SeenOf(hall, org, chain string) *Seen {
	for i := range s.Seen {
		x := &s.Seen[i]
		if x.Hall == hall && x.Org == org && x.Chain == chain {
			return x
		}
	}
	return nil
}

// See records a verified latest token, never moving a chain backwards.
func (s *State) See(v Seen) {
	if old := s.SeenOf(v.Hall, v.Org, v.Chain); old != nil {
		if v.Seq >= old.Seq {
			*old = v
		}
		return
	}
	s.Seen = append(s.Seen, v)
}

// EvidenceOf is the last evidence for a group, or nil.
func (s *State) EvidenceOf(hall, org, group string) *Evidence {
	for i := range s.Evidence {
		x := &s.Evidence[i]
		if x.Hall == hall && x.Org == org && x.Group == group {
			return x
		}
	}
	return nil
}

// Record keeps evidence for a group, replacing the older.
func (s *State) Record(v Evidence) {
	if old := s.EvidenceOf(v.Hall, v.Org, v.Group); old != nil {
		*old = v
		return
	}
	s.Evidence = append(s.Evidence, v)
}

// Hold keeps a grant to this identity, once.
func (s *State) Hold(g HeldGrant) {
	if !slices.ContainsFunc(s.Held, func(h HeldGrant) bool { return h.Txid == g.Txid }) {
		s.Held = append(s.Held, g)
	}
}

// WriteAtomic writes data to path through a temporary file in the same
// directory, synced and renamed, then syncs the directory, so a crash
// leaves either the old file or the new one.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // the home's own directory
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ErrLocked is a lock another process holds.
var ErrLocked = errors.New("state: locked by another borg process")

// Lock takes an exclusive lock on path (created at 0600), without waiting.
// The lock is the kernel's and ends with the process, so a crash never
// leaves it held. Release it with the returned function.
func Lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the home's own file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a file descriptor
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor
		f.Close()
	}, nil
}
