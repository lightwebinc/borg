package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/funding"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/borg/borgrec"
	"github.com/lightwebinc/borg/internal/limits"
	"github.com/lightwebinc/borg/internal/state"
	"github.com/lightwebinc/borg/internal/unicast"
	"github.com/lightwebinc/borg/reader"
)

// ErrNoOrg is a home that runs no organisation yet.
var ErrNoOrg = errors.New("this home runs no organisation (borg org create)")

// LostError is a transition another transaction spending the same
// predecessor beat to the chain (spec section 7.3): the epoch key it sampled
// is discarded and nothing it built was released.
type LostError struct {
	What string
	Txid string
	By   string
	// Fee is set when it was the fee coin, not the predecessor, that
	// another transaction spent: no transition beat this one.
	Fee bool
}

func (e *LostError) Error() string {
	if e.Fee {
		return fmt.Sprintf("%s %s lost: its fee coin is spent by %s, a transaction of another instance of the tooling that held the same coin; the epoch key it sampled is discarded and nothing was released; run the command again", e.What, Short(e.Txid), Short(e.By))
	}
	return fmt.Sprintf("%s %s lost: its predecessor is spent by %s, another transition; the epoch key it sampled is discarded and nothing was released; the command reads the new tip when run again", e.What, Short(e.Txid), Short(e.By))
}

// GrantReq is a grant to issue.
type GrantReq struct {
	Subject *ec.PublicKey
	Role    string
	Scope   string
	Expiry  time.Time
}

func (e *Engine) org() (*state.Org, error) {
	if e.St.Org == nil {
		return nil, ErrNoOrg
	}
	return e.St.Org, nil
}

// OrgKey is the organisation's identity key, compressed.
func (e *Engine) OrgKey() []byte { return e.Signer.Identity.Compressed() }

func (e *Engine) orgHex() string { return e.Signer.IdentityHex() }

// rebuild rebuilds a mined token of the home's own.
func rebuild(t *state.Token) (*transaction.Transaction, error) {
	return funding.Rebuild(t.RawHex, t.BumpHex, "")
}

// ownToken finds a token of the home's own chains by txid, rebuilt.
func (e *Engine) ownToken(txid string) (*transaction.Transaction, error) {
	if o := e.St.Org; o != nil {
		for i := range o.Tokens {
			if o.Tokens[i].Txid == txid {
				return rebuild(&o.Tokens[i])
			}
		}
		for gi := range o.Groups {
			for i := range o.Groups[gi].Tokens {
				if o.Groups[gi].Tokens[i].Txid == txid {
					return rebuild(&o.Groups[gi].Tokens[i])
				}
			}
		}
	}
	return nil, fmt.Errorf("%s is not a token of this home's", txid)
}

// currentGrant is the subject's current grant by the organisation's own
// record: admitted, not revoked, not swept, not expired.
func (e *Engine) currentGrant(subject string) *state.Grant {
	o := e.St.Org
	if o == nil {
		return nil
	}
	now := e.Now()
	var best *state.Grant
	for i := range o.Grants {
		g := &o.Grants[i]
		if g.Subject != subject || !g.Admitted || g.Revoked || g.Swept != "" || !borgrec.IsMembership(g.Scope) {
			continue
		}
		exp, err := borgrec.ParseExpiry(g.Expiry)
		if err != nil || !now.Before(exp) {
			continue
		}
		if best == nil || g.Expiry > best.Expiry {
			best = g
		}
	}
	return best
}

// CurrentGrant is the subject's current grant, or nil.
func (e *Engine) CurrentGrant(subject string) *state.Grant { return e.currentGrant(subject) }

// orgRecords are the organisation's own chain, decoded.
func orgRecords(o *state.Org) ([]*borgrec.Org, error) {
	var out []*borgrec.Org
	for _, t := range o.Tokens {
		b, err := hex.DecodeString(t.Record)
		if err != nil {
			return nil, err
		}
		r, err := borgrec.DecodeOrg(b)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// GroupRecords are a group's own chain, decoded.
func GroupRecords(g *state.Group) ([]*borgrec.Group, error) {
	var out []*borgrec.Group
	for _, t := range g.Tokens {
		b, err := hex.DecodeString(t.Record)
		if err != nil {
			return nil, err
		}
		r, err := borgrec.DecodeGroup(b)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Members folds a group's own chain: the members after its latest token
// and every key any token removed. Only a fold that breaks
// (members-mismatch) is an error: a token the index reports
// commitment-reuse or rejoin-unrotated leaves the members well defined,
// and the group must stay operable so that it can be rotated (Flagged).
func Members(g *state.Group) ([][]byte, map[string]bool, error) {
	recs, err := GroupRecords(g)
	if err != nil {
		return nil, nil, err
	}
	f := borgrec.NewGroupFold()
	removed := map[string]bool{}
	for _, r := range recs {
		if st := f.Apply(r); st == borgrec.StatusMembersMismatch {
			return nil, nil, fmt.Errorf("the group's own chain folds to %s at seq %d", st, r.Seq)
		}
		for _, k := range r.Remove {
			removed[string(k)] = true
		}
	}
	return f.Members(), removed, nil
}

// Flagged reports whether the group's current epoch holds a token the
// index reports commitment-reuse or rejoin-unrotated (spec section 11.2):
// its status and seq, or "" when the epoch is clean. Such an epoch's key is
// not one to release: a key re-added without a rotation would read what
// was written while it was away, and a repeated commitment is an old key.
// A rotation begins a clean epoch.
func Flagged(g *state.Group) (string, uint64, error) {
	recs, err := GroupRecords(g)
	if err != nil || len(recs) == 0 {
		return "", 0, err
	}
	cur := recs[len(recs)-1].EpochSeq
	f := borgrec.NewGroupFold()
	for _, r := range recs {
		st := f.Apply(r)
		if r.Seq >= cur && (st == borgrec.StatusCommitmentReuse || st == borgrec.StatusRejoinUnrotated) {
			return st, r.Seq, nil
		}
	}
	return "", 0, nil
}

// Debt is a group's rotation debt by the organisation's own record: the
// members with no current grant now (spec section 7.1).
func (e *Engine) Debt(g *state.Group) ([][]byte, error) {
	members, _, err := Members(g)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, m := range members {
		if e.currentGrant(hex.EncodeToString(m)) == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// IssueGrant issues a grant to req.Subject (spec sections 5 and 7.1): a
// BRC-52 certificate signed by orgKey, its revocation outpoint the grant-
// tree output its carrier spends. The carrier is persisted and published
// before any organisation token admits it.
func (e *Engine) IssueGrant(ctx context.Context, orgKey *ec.PrivateKey, req GrantReq) (*state.Grant, error) {
	o, err := e.org()
	if err != nil {
		return nil, err
	}
	subject := req.Subject.Compressed()
	if err := borgrec.CheckIdentity(subject); err != nil || bytes.Equal(subject, e.OrgKey()) || borgrec.IsAnyone(subject) {
		return nil, fmt.Errorf("%s cannot be granted: neither the organisation nor the anyone key is ever a member", hex.EncodeToString(subject))
	}
	held := 0
	for _, g := range o.Grants {
		if g.Subject == hex.EncodeToString(subject) && !g.Revoked && g.Swept == "" {
			held++
		}
	}
	if held >= limits.MaxGrantsPerSubject {
		return nil, fmt.Errorf("%s holds %d grants already, the most one subject holds in a roll (spec section 5.6); revoke one first", hex.EncodeToString(subject), held)
	}
	vals := map[string]string{"expiry": req.Expiry.UTC().Format(borgrec.ExpiryLayout), "role": req.Role, "scope": req.Scope}
	for n, v := range vals {
		if err := borgrec.CheckFieldValue(n, v); err != nil {
			return nil, fmt.Errorf("grant %s %q: %w", n, v, err)
		}
	}
	tree, vout, err := e.spend(ctx, state.TreeGrant)
	if err != nil {
		return nil, err
	}
	spec := borgrec.GrantSpec{Hall: o.Hall, Subject: req.Subject, Serial: random32(), RevTxid: [32]byte(*tree.TxID()), RevVout: vout,
		Values: vals, FieldKeys: map[string][32]byte{}, FieldIVs: map[string][32]byte{}, KeyringIVs: map[string][32]byte{}, Reveal: !o.Opaque}
	for _, n := range borgrec.GrantFieldNames {
		spec.FieldKeys[n], spec.FieldIVs[n], spec.KeyringIVs[n] = random32(), random32(), random32()
	}
	g, err := borgrec.BuildGrant(ctx, orgKey, spec)
	if err != nil {
		return nil, err
	}
	rec, err := g.Encode()
	if err != nil {
		return nil, err
	}
	a, beef, err := e.carrierFor(ctx, o.Hall, state.TreeGrant, rec, tree, vout)
	if err != nil {
		return nil, err
	}
	txid := Txid(a.Txid)
	sg := state.Grant{Subject: hex.EncodeToString(subject), Txid: txid, Commitment: hex.EncodeToString(a.Carrier.Commitment[:]),
		Tree: tree.TxID().String(), Vout: vout, Role: req.Role, Scope: req.Scope, Expiry: vals["expiry"], Record: hex.EncodeToString(rec), Beef: hex.EncodeToString(beef)}
	e.markSpent(state.TreeGrant, tree, vout)
	o.Grants = append(o.Grants, sg)
	p := state.Pending{Kind: state.KindGrant, Hall: o.Hall, Txid: txid, Beef: hex.EncodeToString(beef), Org: e.orgHex(), Member: sg.Subject}
	if err := e.queue(ctx, p); err != nil {
		return o.GrantByTxid(txid), err
	}
	return o.GrantByTxid(txid), nil
}

func random32() [32]byte {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return b
}

// CreateOrg begins the organisation (spec section 7.1): a chain tree and a
// grant tree, mined; the first grants, published; organisation token seq 0
// admitting them, mined and then published with the chain tree. A run that
// stopped part way is finished: grants issued and not admitted are reused.
func (e *Engine) CreateOrg(ctx context.Context, orgKey *ec.PrivateKey, hall, name string, opaque bool, members []GrantReq) error {
	if e.St.Org != nil && len(e.St.Org.Tokens) > 0 {
		return fmt.Errorf("this home runs the organisation %q in hall %s already", e.St.Org.Name, e.St.Org.Hall)
	}
	if e.St.Org == nil {
		e.St.Org = &state.Org{Hall: hall, Name: name, Opaque: opaque}
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	o := e.St.Org
	if o.Hall != hall || o.Name != name || o.Opaque != opaque {
		return fmt.Errorf("an organisation %q in hall %s (opaque %v) was begun in this home; finish it with the same name, hall and setting", o.Name, o.Hall, o.Opaque)
	}
	if e.Opts.Signers > 1 && len(members) < borgrec.MaxDelta {
		// Several instances sign: the signer key's grant is among the
		// first, so no later token is needed to admit it.
		if req, err := e.signerReq(); err == nil {
			members = append(slices.Clone(members), req)
		}
	}
	if len(members) > borgrec.MaxDelta {
		return fmt.Errorf("%d members: one token admits at most %d grants; create with %d and grant the rest", len(members), borgrec.MaxDelta, borgrec.MaxDelta)
	}
	chainTree, cv, err := e.spend(ctx, state.TreeChain)
	if err != nil {
		return err
	}
	var admit []*state.Grant
	for _, m := range members {
		var g *state.Grant
		for i := range o.Grants {
			if og := &o.Grants[i]; og.Subject == hex.EncodeToString(m.Subject.Compressed()) && !og.Admitted && !og.Revoked && og.Swept == "" {
				g = og
			}
		}
		if g == nil {
			if g, err = e.IssueGrant(ctx, orgKey, m); err != nil {
				return err
			}
			e.note("grant %s issued to %s", g.Txid, g.Subject)
		}
		admit = append(admit, g)
	}
	rec := &borgrec.Org{Hall: hall, Org: e.OrgKey(), Seq: 0, Name: name, Opaque: opaque, Created: e.created()}
	for _, g := range admit {
		c, _ := hex.DecodeString(g.Commitment)
		rec.Admit = append(rec.Admit, [32]byte(c))
	}
	sortHashes(rec.Admit)
	rec.Count = uint64(len(rec.Admit))
	rec.Roll = borgrec.RollRoot(rec.Admit)
	if st := borgrec.NewOrgFold().Apply(rec); st != borgrec.StatusConsistent {
		return fmt.Errorf("the organisation's first token would fold to %s; it is not built", st)
	}
	b, err := rec.Encode()
	if err != nil {
		return err
	}
	tr, tx, err := e.buildToken(ctx, state.KindOrgToken, "", 0, b, chainTree, cv, borgrec.ChainDerivation)
	if err != nil {
		return err
	}
	tr.What = "organisation token seq 0"
	for _, g := range admit {
		tr.Grants = append(tr.Grants, g.Commitment)
	}
	e.markSpent(state.TreeChain, chainTree, cv)
	return e.run(ctx, tr, tx)
}

func (e *Engine) created() uint64 {
	t := e.Now().Unix()
	if t < 1 {
		t = 1
	}
	return uint64(t) //nolint:gosec // a clock after 1970
}

func sortHashes(a [][32]byte) {
	sort.Slice(a, func(i, j int) bool { return bytes.Compare(a[i][:], a[j][:]) < 0 })
}

func sortKeys(a [][]byte) {
	sort.Slice(a, func(i, j int) bool { return bytes.Compare(a[i], a[j]) < 0 })
}

// OrgTransition mints the organisation's next token: admit and revoke
// grants (spec sections 3 and 5.6). The tooling refuses to build a
// transition the index would report: its fold over the organisation's own
// chain must stay consistent.
func (e *Engine) OrgTransition(ctx context.Context, admit, revoke []*state.Grant, what string) error {
	o, err := e.org()
	if err != nil {
		return err
	}
	last := state.Latest(o.Tokens)
	if last == nil {
		return errors.New("the organisation has no token yet (borg org create)")
	}
	if len(admit) > borgrec.MaxDelta || len(revoke) > borgrec.MaxDelta {
		return fmt.Errorf("one token admits and revokes at most %d grants each; a larger change is several transitions", borgrec.MaxDelta)
	}
	recs, err := orgRecords(o)
	if err != nil {
		return err
	}
	pred := recs[len(recs)-1]
	f := borgrec.NewOrgFold()
	for _, r := range recs {
		if st := f.Apply(r); st != borgrec.StatusConsistent {
			return fmt.Errorf("the organisation's own chain folds to %s at seq %d", st, r.Seq)
		}
	}
	roll := map[[32]byte]bool{}
	for _, c := range f.Roll() {
		roll[c] = true
	}
	rec := &borgrec.Org{Hall: o.Hall, Org: e.OrgKey(), Seq: pred.Seq + 1, Prev: hashOf(last.Txid), Name: o.Name, Opaque: o.Opaque, Created: e.created()}
	for _, g := range revoke {
		c := hashOfHex(g.Commitment)
		if !roll[c] {
			return fmt.Errorf("grant %s is not in the roll", g.Txid)
		}
		delete(roll, c)
		rec.Revoke = append(rec.Revoke, c)
	}
	for _, g := range admit {
		c := hashOfHex(g.Commitment)
		if roll[c] {
			return fmt.Errorf("grant %s is in the roll already", g.Txid)
		}
		roll[c] = true
		rec.Admit = append(rec.Admit, c)
	}
	sortHashes(rec.Admit)
	sortHashes(rec.Revoke)
	all := make([][32]byte, 0, len(roll))
	for c := range roll {
		all = append(all, c)
	}
	rec.Count, rec.Roll = uint64(len(all)), borgrec.RollRoot(all)
	if rec.Count > borgrec.MaxRoll {
		return fmt.Errorf("a roll holds at most %d grants", borgrec.MaxRoll)
	}
	if e.Legs.Facade != nil && len(rec.Admit)+len(rec.Revoke) > limits.PlaneChangesWarn {
		e.note("warning: %d changes in one token on the plane; a token and its parent then pass 4 packets (docs/limits.md)", len(rec.Admit)+len(rec.Revoke))
	}
	if err := borgrec.CheckOrgTransition(pred, hashOf(last.Txid), rec); err != nil {
		return fmt.Errorf("the transition breaks the host's rules (%s); it is not built: %w", borgrec.Reason(err), err)
	}
	if st := f.Apply(rec); st != borgrec.StatusConsistent {
		return fmt.Errorf("the transition would fold to %s; it is not built", st)
	}
	b, err := rec.Encode()
	if err != nil {
		return err
	}
	parent, err := e.tokenTx(ctx, last)
	if err != nil {
		return err
	}
	tr, tx, err := e.buildToken(ctx, state.KindOrgToken, "", rec.Seq, b, parent, last.Vout, borgrec.OrgDerivation)
	if err != nil {
		return err
	}
	tr.What = what
	for _, g := range admit {
		tr.Grants = append(tr.Grants, g.Commitment)
	}
	for _, g := range revoke {
		tr.Revokes = append(tr.Revokes, g.Commitment)
	}
	return e.run(ctx, tr, tx)
}

func hashOf(txid string) [32]byte {
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		panic(err)
	}
	return [32]byte(*h)
}

func hashOfHex(s string) [32]byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		panic("engine: a commitment that is not 32 bytes")
	}
	return [32]byte(b)
}

// CreateGroup creates a group (spec section 7.1): a fresh group id and
// epoch key, token seq 0 adding members, spending a chain-tree output; once
// mined, the token and the wraps of epoch 0 to every member. Every member
// must hold a current grant.
func (e *Engine) CreateGroup(ctx context.Context, name string, members [][]byte, rotateOnJoin bool, rotateEvery uint64) (*state.Group, error) {
	o, err := e.org()
	if err != nil {
		return nil, err
	}
	if len(o.Tokens) == 0 {
		return nil, errors.New("the organisation has no token yet (borg org create)")
	}
	members = slices.Clone(members)
	if e.signerActive() {
		sk, err := e.EnsureSigner(ctx)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(members, func(m []byte) bool { return bytes.Equal(m, sk) }) {
			members = append(members, sk)
		}
	}
	if len(members) > borgrec.MaxDelta {
		return nil, fmt.Errorf("one token adds at most %d members; create with %d and add the rest", borgrec.MaxDelta, borgrec.MaxDelta)
	}
	sortKeys(members)
	for i, m := range members {
		if i > 0 && bytes.Equal(m, members[i-1]) {
			return nil, fmt.Errorf("%s is named twice", hex.EncodeToString(m))
		}
		if err := e.checkJoiner(m); err != nil {
			return nil, err
		}
	}
	gid, err := borgrec.NewGroupID(rand.Reader)
	if err != nil {
		return nil, err
	}
	k, err := borgrec.SampleKey(nil)
	if err != nil {
		return nil, err
	}
	rec := &borgrec.Group{Hall: o.Hall, Org: e.OrgKey(), Group: gid, Name: name, Commitment: borgrec.Commitment(k),
		Add: members, Count: uint64(len(members)), Members: borgrec.MembersRoot(members), RotateOnJoin: rotateOnJoin,
		RotateEvery: rotateEvery, Created: e.created()}
	if st := borgrec.NewGroupFold().Apply(rec); st != borgrec.StatusConsistent {
		return nil, fmt.Errorf("the group's first token would fold to %s; it is not built", st)
	}
	b, err := rec.Encode()
	if err != nil {
		return nil, err
	}
	chainTree, cv, err := e.spend(ctx, state.TreeChain)
	if err != nil {
		return nil, err
	}
	id := reader.GroupHex(gid)
	o.Groups = append(o.Groups, state.Group{ID: id, Name: name, RotateOnJoin: rotateOnJoin, RotateEvery: rotateEvery})
	tr, tx, err := e.buildToken(ctx, state.KindGroupToken, id, 0, b, chainTree, cv, borgrec.ChainDerivation)
	if err != nil {
		o.Groups = o.Groups[:len(o.Groups)-1]
		return nil, err
	}
	tr.What, tr.Key, tr.Epoch = "group "+id+" token seq 0", hex.EncodeToString(k[:]), 0
	e.markSpent(state.TreeChain, chainTree, cv)
	if err := e.run(ctx, tr, tx); err != nil {
		return o.Group(id), err
	}
	return o.Group(id), nil
}

// checkJoiner refuses a key that may not join a group: the organisation,
// the anyone key, or a key with no current grant (spec section 7.1: the
// organisation wraps a key to nobody without one).
func (e *Engine) checkJoiner(m []byte) error {
	if err := borgrec.CheckIdentity(m); err != nil {
		return fmt.Errorf("%s is not an identity key", hex.EncodeToString(m))
	}
	if bytes.Equal(m, e.OrgKey()) || borgrec.IsAnyone(m) {
		return fmt.Errorf("%s cannot be a member: neither the organisation nor the anyone key is ever a member", hex.EncodeToString(m))
	}
	if e.currentGrant(hex.EncodeToString(m)) == nil {
		return fmt.Errorf("%s holds no current grant (admitted, not revoked, not expired, scope member): grant it first (borg grant issue)", hex.EncodeToString(m))
	}
	return nil
}

// Change is a group transition asked for.
type Change struct {
	Add    [][]byte
	Remove [][]byte
	// Rotate begins a new epoch whatever else the change does.
	Rotate bool
	What   string
}

// ChangeResult is what a group transition did.
type ChangeResult struct {
	Seq     uint64
	Epoch   uint32
	Rotated bool
	// Debt are the keys removed because they held no current grant.
	Debt [][]byte
	// Rejoin are the keys added back that an earlier token removed.
	Rejoin [][]byte
}

// GroupChange mints a group's next token (spec sections 4.2 and 7.1). Every
// transition first removes the group's rotation debt. A removal, a
// predecessor's rotateOnJoin with an addition, a key an earlier token
// removed coming back, or Rotate begins a new epoch with a freshly
// sampled key; the transition is checked against the host's rules and the
// index's fold before it is built, and refused if it would trip either.
func (e *Engine) GroupChange(ctx context.Context, gid string, c Change) (*ChangeResult, error) {
	o, err := e.org()
	if err != nil {
		return nil, err
	}
	g := o.Group(gid)
	if g == nil {
		return nil, fmt.Errorf("group %s is not one of this organisation's", gid)
	}
	last := state.Latest(g.Tokens)
	if last == nil {
		return nil, fmt.Errorf("group %s has no token yet", gid)
	}
	recs, err := GroupRecords(g)
	if err != nil {
		return nil, err
	}
	pred := recs[len(recs)-1]
	members, removed, err := Members(g)
	if err != nil {
		return nil, err
	}
	isMember := map[string]bool{}
	for _, m := range members {
		isMember[string(m)] = true
	}
	if e.signerActive() {
		// The signer key joins every group this home changes, so another
		// instance opens the epoch keys this one samples (signer.go). Its
		// grant is renewed first, so it is never rotation debt.
		sk, err := e.EnsureSigner(ctx)
		if err != nil {
			return nil, err
		}
		named := func(set [][]byte) bool {
			return slices.ContainsFunc(set, func(k []byte) bool { return bytes.Equal(k, sk) })
		}
		if !isMember[string(sk)] && !named(c.Add) && !named(c.Remove) {
			c.Add = append(slices.Clone(c.Add), sk)
		}
	}
	res := &ChangeResult{}
	remove := map[string]bool{}
	for _, r := range c.Remove {
		if !isMember[string(r)] {
			return nil, fmt.Errorf("%s is not a member of group %s", hex.EncodeToString(r), gid)
		}
		remove[string(r)] = true
	}
	debt, err := e.Debt(g)
	if err != nil {
		return nil, err
	}
	for _, d := range debt {
		if !remove[string(d)] {
			remove[string(d)] = true
			res.Debt = append(res.Debt, d)
		}
	}
	add := map[string]bool{}
	for _, a := range c.Add {
		if isMember[string(a)] && !remove[string(a)] {
			return nil, fmt.Errorf("%s is a member of group %s already", hex.EncodeToString(a), gid)
		}
		if err := e.checkJoiner(a); err != nil {
			return nil, err
		}
		if remove[string(a)] {
			return nil, fmt.Errorf("%s is both added and removed; a key change adds the new key and removes the old one", hex.EncodeToString(a))
		}
		if add[string(a)] {
			return nil, fmt.Errorf("%s is named twice", hex.EncodeToString(a))
		}
		add[string(a)] = true
		if removed[string(a)] {
			res.Rejoin = append(res.Rejoin, a)
		}
	}
	rec := *pred
	rec.Extra = nil
	rec.Seq, rec.Prev, rec.Created = pred.Seq+1, hashOf(last.Txid), e.created()
	rec.Add, rec.Remove = keysOf(add), keysOf(remove)
	if len(rec.Add) > borgrec.MaxDelta || len(rec.Remove) > borgrec.MaxDelta {
		return nil, fmt.Errorf("one token adds and removes at most %d members each; a larger change is several transitions", borgrec.MaxDelta)
	}
	after := map[string]bool{}
	for _, m := range members {
		if !remove[string(m)] {
			after[string(m)] = true
		}
	}
	for a := range add {
		after[a] = true
	}
	all := keysOf(after)
	rec.Count, rec.Members = uint64(len(all)), borgrec.MembersRoot(all)
	if rec.Count > borgrec.MaxMembers {
		return nil, fmt.Errorf("a group holds at most %d members", borgrec.MaxMembers)
	}
	if e.Legs.Facade != nil && len(rec.Add)+len(rec.Remove) > limits.PlaneChangesWarn {
		e.note("warning: %d changes in one token on the plane; a token and its parent then pass 4 packets (docs/limits.md)", len(rec.Add)+len(rec.Remove))
	}
	if rec.Count > limits.MembersWarn {
		e.note("warning: group %s has %d members; every removal wraps the new key to each (docs/limits.md)", gid, rec.Count)
	}
	rotate := c.Rotate || len(rec.Remove) > 0 || (pred.RotateOnJoin && len(rec.Add) > 0) || len(res.Rejoin) > 0
	if st, at, err := Flagged(g); err != nil {
		return nil, err
	} else if st != "" && !rotate {
		// The epoch is one the index flags: every transition leaves it.
		e.note("group %s: seq %d is %s, so this transition begins a new epoch", gid, at, st)
		rotate = true
	}
	if !rotate && len(rec.Add) > 0 && g.EpochOf(uint32(pred.Epoch)) == nil { //nolint:gosec // at most 2^32 - 1
		// Another instance began the epoch and this home has not opened
		// its key: it could not wrap it to the keys it adds. A new epoch
		// is one it holds.
		e.note("group %s: this home does not hold the key of epoch %d, so this transition begins a new epoch", gid, pred.Epoch)
		rotate = true
	}
	if !rotate && len(rec.Add) == 0 {
		return nil, errors.New("the transition changes nothing")
	}
	var k [32]byte
	if rotate {
		used := map[[32]byte]bool{}
		for _, r := range recs {
			used[r.Commitment] = true
		}
		for {
			if k, err = borgrec.SampleKey(nil); err != nil {
				return nil, err
			}
			if !used[borgrec.Commitment(k)] {
				break
			}
		}
		rec.Epoch, rec.EpochSeq, rec.Commitment = pred.Epoch+1, rec.Seq, borgrec.Commitment(k)
		if rec.Epoch > borgrec.MaxEpoch {
			return nil, errors.New("the group has used every epoch number")
		}
	}
	if err := borgrec.CheckGroupTransition(pred, hashOf(last.Txid), &rec); err != nil {
		return nil, fmt.Errorf("the transition breaks the host's rules (%s); it is not built: %w", borgrec.Reason(err), err)
	}
	f := borgrec.NewGroupFold()
	for _, r := range recs {
		f.Apply(r)
	}
	if st := f.Apply(&rec); st != borgrec.StatusConsistent {
		return nil, fmt.Errorf("the transition would be reported %s by every host's index; it is not built", st)
	}
	b, err := rec.Encode()
	if err != nil {
		return nil, err
	}
	parent, err := e.tokenTx(ctx, last)
	if err != nil {
		return nil, err
	}
	tr, tx, err := e.buildToken(ctx, state.KindGroupToken, gid, rec.Seq, b, parent, last.Vout, borgrec.GroupDerivation)
	if err != nil {
		return nil, err
	}
	tr.What = c.What
	if tr.What == "" {
		tr.What = fmt.Sprintf("group %s token seq %d", gid, rec.Seq)
	}
	if rotate {
		tr.Key, tr.Epoch = hex.EncodeToString(k[:]), uint32(rec.Epoch) //nolint:gosec // bounded above
	}
	res.Seq, res.Epoch, res.Rotated = rec.Seq, uint32(rec.Epoch), rotate //nolint:gosec // bounded above
	if err := e.run(ctx, tr, tx); err != nil {
		return res, err
	}
	return res, nil
}

func keysOf(set map[string]bool) [][]byte {
	out := make([][]byte, 0, len(set))
	for k := range set {
		out = append(out, []byte(k))
	}
	sortKeys(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildToken builds and signs a chain token spending output parentVout of
// parent (unlocked under parentDeriv) and a fee coin from the pool.
func (e *Engine) buildToken(ctx context.Context, kind, group string, seq uint64, record []byte, parent *transaction.Transaction, parentVout uint32, parentDeriv pushdrop.Derivation) (*state.Transition, *transaction.Transaction, error) {
	d, tag := borgrec.OrgDerivation, borgrec.TagOrg
	if kind == state.KindGroupToken {
		d, tag = borgrec.GroupDerivation, borgrec.TagGroup
	}
	lock, err := d.Lock(ctx, e.Signer, e.Signer.Originator, [][]byte{tag, record}, true)
	if err != nil {
		return nil, nil, err
	}
	payer := e.NewPayer()
	fee, coin, err := e.sweepFee(ctx, payer)
	if err != nil {
		return nil, nil, FeeError(err)
	}
	change, err := e.Signer.FundScript()
	if err != nil {
		payer.GiveBack()
		return nil, nil, err
	}
	tx, err := mint.Transition(lock, 1, &mint.Input{Tx: parent, Vout: parentVout, Unlocker: parentDeriv.Unlocker(ctx, e.Signer, e.Signer.Originator)},
		fee, change, e.Opts.Fees)
	if err != nil {
		payer.GiveBack()
		return nil, nil, err
	}
	tr := &state.Transition{Kind: kind, Group: group, Seq: seq, Txid: tx.TxID().String(), RawHex: tx.Hex(), Record: hex.EncodeToString(record),
		Vout: 0, Parent: parent.TxID().String(), ParentVout: parentVout, Fee: coin}
	return tr, tx, nil
}

// pendingTx is the transition built in this run, so its inputs keep their
// source transactions for the settlement leg.
type pendingTx struct {
	txid string
	tx   *transaction.Transaction
}

// run persists a transition, the epoch key with it, before anything is
// sent, then takes it through settlement, its proof and publication.
func (e *Engine) run(ctx context.Context, tr *state.Transition, tx *transaction.Transaction) error {
	o := e.St.Org
	o.Pending = tr
	if err := e.St.Save(); err != nil {
		return err
	}
	e.pending = &pendingTx{tr.Txid, tx}
	return e.FinishTransition(ctx)
}

// FinishTransition takes the persisted transition, if any, to the chain:
// it is sent (again, the same bytes, when a previous run stopped), its
// proof awaited, and once mined in the publisher's own headers it joins
// the chain and is published. One that loses its predecessor to another
// transaction is discarded with its epoch key (spec section 7.3), unless
// the transaction that won carries byte for byte the same record: then it
// is the same transition, re-encoded, and is taken as it is.
func (e *Engine) FinishTransition(ctx context.Context) error {
	o := e.St.Org
	if o == nil || o.Pending == nil {
		return nil
	}
	tr := o.Pending
	var tx *transaction.Transaction
	if e.pending != nil && e.pending.txid == tr.Txid {
		tx = e.pending.tx
	} else {
		var err error
		if tx, err = funding.Rebuild(tr.RawHex, "", ""); err != nil {
			return fmt.Errorf("%s: %w", tr.What, err)
		}
	}
	payer := e.NewPayer()
	if mp, h, err := e.Legs.Chain.Proof(ctx, tr.Txid); err == nil {
		tx.MerklePath = mp
		return e.adopt(ctx, tr, tx, mp, h, payer)
	}
	if !tr.Submitted {
		if err := e.sources(ctx, tx); err != nil {
			return fmt.Errorf("%s: %w (it is persisted; the next command sends it again)", tr.What, err)
		}
		e.note("%s %s: settling via %s", tr.What, Short(tr.Txid), e.Legs.Settler.Name())
		if err := e.Legs.Settler.Submit(ctx, tx); err != nil && !alreadyKnown(err) {
			if why := e.refusal(ctx, tx, err); why != "" {
				return e.lostOrCopy(ctx, tr, why, payer)
			}
			return fmt.Errorf("%s: settle via %s: %w (it is persisted; the next command sends it again)", tr.What, e.Legs.Settler.Name(), err)
		}
		tr.Submitted = true
		if err := e.St.Save(); err != nil {
			return err
		}
	}
	mp, h, err := payer.Await(ctx, tr.What, tx)
	if err != nil {
		if errors.Is(err, producer.ErrRefused) {
			return e.lostOrCopy(ctx, tr, err.Error(), payer)
		}
		return fmt.Errorf("%s %s: %w (it is persisted; the next command waits again)", tr.What, Short(tr.Txid), err)
	}
	return e.adopt(ctx, tr, tx, mp, h, payer)
}

// lostOrCopy settles a transition a leg refused: the predecessor's spender
// is read from the node. Another transaction that spends it is read: a
// mined one carrying the same record is this transition re-encoded and is
// adopted; any other won, and this one is discarded. A spender the node
// cannot show settles nothing, and the transition is kept. A predecessor
// that is unspent, or spent by this very transaction, is beaten by nobody:
// the transition is kept, unless another transaction spends its fee coin.
func (e *Engine) lostOrCopy(ctx context.Context, tr *state.Transition, why string, payer *producer.Payer) error {
	by, err := e.Legs.Chain.Spender(ctx, tr.Parent, tr.ParentVout)
	err = reader.QuietSpend(err)
	if err != nil {
		return fmt.Errorf("%s: refused (%s), and the node could not say what spends its predecessor: %w (the next command looks again)", tr.What, why, err)
	}
	if by != "" && by != tr.Txid {
		// Another transaction spends the predecessor. Whether it won, or is
		// this transition re-encoded, is settled only by reading it: a node
		// that cannot show it settles nothing, and the transition is kept.
		if _, herr := reader.Hash(by); herr != nil {
			return fmt.Errorf("%s: the node names a spender of its predecessor that is not a transaction id; it is kept, and the next command looks again", tr.What)
		}
		raw, err := e.Legs.Chain.TxRaw(ctx, by)
		if err != nil {
			return fmt.Errorf("%s: its predecessor is spent by %s, which the node could not show (%v): whether that is this transition under another txid is not known yet; it is kept, and the next command looks again", tr.What, Short(by), err)
		}
		other, err := guard.ParseTransaction(raw, guard.DefaultBound)
		if err != nil || other.TxID().String() != by {
			return fmt.Errorf("%s: its predecessor is spent by %s, and the node answered other bytes for it; it is kept, and the next command looks again", tr.What, Short(by))
		}
		if t, err := borgrec.ReadToken(other); err == nil && hex.EncodeToString(t.Record) == tr.Record {
			mp, h, perr := e.Legs.Chain.Proof(ctx, by)
			if perr != nil {
				return fmt.Errorf("%s: a copy of it, %s, spends its predecessor and has not mined yet (the next command looks again)", tr.What, Short(by))
			}
			e.note("%s: a copy with another txid, %s, carrying the same record, was mined in its place: the same transition", tr.What, Short(by))
			other.MerklePath = mp
			return e.adopt(ctx, tr, other, mp, h, payer)
		}
	}
	feeLost := false
	if by == "" || by == tr.Txid {
		// The predecessor is unspent, or spent by this very transaction
		// (the node took it, and it has not mined yet): no transition beat
		// this one, whatever a leg answered. Only if its fee coin is spent
		// by ANOTHER transaction (another instance of the tooling held the
		// same coin) can these bytes never mine: the transition is then
		// discarded like a lost one, and built again. Anything else keeps
		// it persisted, to be sent and waited for again.
		kept := fmt.Errorf("%s %s: refused (%s), and its predecessor is not spent by another transaction: it is kept, and the next command sends it and waits again", tr.What, Short(tr.Txid), why)
		f := tr.Fee
		if f == nil {
			return kept
		}
		spent, err := e.Legs.Chain.Spender(ctx, f.TxID, f.Vout)
		if err != nil || spent == "" || spent == tr.Txid {
			return kept
		}
		if _, herr := reader.Hash(spent); herr != nil {
			return kept
		}
		by, feeLost = spent, true
	}
	e.St.Org.Pending = nil
	if f := tr.Fee; f != nil {
		if spent, err := e.Legs.Chain.Spender(ctx, f.TxID, f.Vout); err == nil && spent == "" {
			_ = e.Pool.Return(*f)
		}
	}
	if tr.Kind == state.KindGroupToken && tr.Seq == 0 {
		o := e.St.Org
		o.Groups = slices.DeleteFunc(o.Groups, func(g state.Group) bool { return g.ID == tr.Group && len(g.Tokens) == 0 })
	}
	if err := e.St.Save(); err != nil {
		return err
	}
	e.pending = nil
	return &LostError{What: tr.What, Txid: tr.Txid, By: by, Fee: feeLost}
}

// adopt joins a mined transition to its chain, keeps the epoch key it
// committed to, and publishes it.
func (e *Engine) adopt(ctx context.Context, tr *state.Transition, tx *transaction.Transaction, mp *transaction.MerklePath, h uint32, payer *producer.Payer) error {
	o := e.St.Org
	payer.Change(tx, h, mp)
	tok := state.Token{Seq: tr.Seq, Txid: tx.TxID().String(), RawHex: tx.Hex(), BumpHex: mp.Hex(), Height: h, Record: tr.Record,
		Vout: tr.Vout, Parent: tr.Parent}
	switch tr.Kind {
	case state.KindOrgToken:
		o.Tokens = append(o.Tokens, tok)
		for _, c := range tr.Grants {
			if g := o.GrantByCommitment(c); g != nil {
				g.Admitted = true
			}
		}
		for _, c := range tr.Revokes {
			if g := o.GrantByCommitment(c); g != nil {
				g.Revoked, g.RevSeq = true, tr.Seq
			}
		}
	default:
		g := o.Group(tr.Group)
		if g == nil {
			return fmt.Errorf("%s: group %s is not in the home", tr.What, tr.Group)
		}
		g.Tokens = append(g.Tokens, tok)
		if tr.Key != "" {
			rec, _ := hex.DecodeString(tr.Record)
			k := hashOfHex(tr.Key)
			c := borgrec.Commitment(k)
			id := borgrec.EpochID(rec)
			g.Epochs = append(g.Epochs, state.Epoch{Epoch: tr.Epoch, Seq: tr.Seq, Key: tr.Key, Commitment: hex.EncodeToString(c[:]),
				EpochID: hex.EncodeToString(id[:])})
		}
	}
	o.Pending = nil
	e.pending = nil
	if err := e.St.Save(); err != nil {
		return err
	}
	if err := e.publishToken(ctx, tr.Kind, tr.Group, tok.Txid); err != nil {
		return err
	}
	if tr.Kind == state.KindOrgToken && len(tr.Grants) > 0 {
		// The wraps the transition owes are made whatever a host lacks.
		if err := e.ConfirmGrants(ctx, tr.Grants); err != nil {
			if oerr := e.Owed(ctx); oerr != nil {
				e.note("%v", oerr)
			}
			return err
		}
	}
	return e.Owed(ctx)
}

// tokenTx rebuilds a mined token of the home's own under a proof that
// verifies against the publisher's headers, replacing a kept proof a
// reorganisation left stale with the node's current one, in the home too
// (spec section 11.5).
func (e *Engine) tokenTx(ctx context.Context, t *state.Token) (*transaction.Transaction, error) {
	tx, err := rebuild(t)
	if err != nil {
		return nil, err
	}
	changed, err := e.current(ctx, tx)
	if err != nil {
		return nil, err
	}
	if changed {
		e.note("token %s: its kept proof named a block that is no longer in the header source's chain; the node's current proof, at height %d, replaces it", Short(t.Txid), tx.MerklePath.BlockHeight)
		t.BumpHex, t.Height = tx.MerklePath.Hex(), tx.MerklePath.BlockHeight
		if err := e.St.Save(); err != nil {
			return nil, err
		}
	}
	return tx, nil
}

// parentTx is the transaction a token's predecessor input spends, under a
// proof that verifies against the publisher's headers: the token before it
// in its chain, or at seq 0 the chain tree.
func (e *Engine) parentTx(ctx context.Context, ts []state.Token, t *state.Token) (*transaction.Transaction, error) {
	for i := range ts {
		if ts[i].Txid == t.Parent {
			return e.tokenTx(ctx, &ts[i])
		}
	}
	return e.treeTx(ctx, t.Parent)
}

// tokensOf are the tokens of the chain a token of kind and group is in.
func (e *Engine) tokensOf(kind, group string) []state.Token {
	o := e.St.Org
	if kind == state.KindGroupToken {
		if g := o.Group(group); g != nil {
			return g.Tokens
		}
		return nil
	}
	return o.Tokens
}

// tokenRef finds a token of the home's own chains.
func (e *Engine) tokenRef(kind, group, txid string) (*state.Token, error) {
	o := e.St.Org
	ts := o.Tokens
	if kind == state.KindGroupToken {
		g := o.Group(group)
		if g == nil {
			return nil, fmt.Errorf("group %s is not in the home", group)
		}
		ts = g.Tokens
	}
	for i := range ts {
		if ts[i].Txid == txid {
			return &ts[i], nil
		}
	}
	return nil, fmt.Errorf("token %s is not in the home", txid)
}

// inHeaders waits, up to Opts.Wait, until tx's proof verifies against the
// publisher's own headers: a mined object is published only once its block
// is in them (spec section 7.2).
func (e *Engine) inHeaders(ctx context.Context, what string, tx *transaction.Transaction) error {
	deadline := time.Now().Add(e.Opts.Wait)
	for {
		if borgrec.Mined(ctx, tx, e.Legs.Headers) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s %s: its block is not in the header source's chain yet; it is kept and the next command publishes it", what, Short(tx.TxID().String()))
		}
		poll := e.Opts.Poll
		if poll <= 0 {
			poll = producer.DefaultPoll
		}
		if err := e.Sleep(ctx, poll); err != nil {
			return err
		}
	}
}

// publishToken publishes a mined token of the home's own, after the
// propagation delay, in the BEEF a host admits: the token and its parent,
// each with its minimal proof. It is held to the host's rules first.
func (e *Engine) publishToken(ctx context.Context, kind, group, txid string) error {
	o := e.St.Org
	t, err := e.tokenRef(kind, group, txid)
	if err != nil {
		return err
	}
	// A token just mined waits for its block to reach the header source;
	// one whose kept proof a reorganisation left stale takes the node's
	// current proof.
	tx, err := e.tokenTx(ctx, t)
	if err != nil {
		if tx, err = rebuild(t); err != nil {
			return err
		}
		if err := e.inHeaders(ctx, kind, tx); err != nil {
			return err
		}
	}
	parent, err := e.parentTx(ctx, e.tokensOf(kind, group), t)
	if err != nil {
		return fmt.Errorf("token %s: its parent %s: %w", Short(txid), Short(t.Parent), err)
	}
	for _, x := range []*transaction.Transaction{tx, parent} {
		if x.MerklePath, err = Minimal(x.MerklePath, x.TxID()); err != nil {
			return err
		}
	}
	beef, err := borgrec.TokenBEEF(tx, parent)
	if err != nil {
		return err
	}
	if _, err := borgrec.Admit(ctx, beef, nil, borgrec.Host{Hall: o.Hall, Headers: e.Legs.Headers}); err != nil {
		return fmt.Errorf("token %s breaks the host's rules (%s) and is not published: %w", Short(txid), borgrec.Reason(err), err)
	}
	p := state.Pending{Kind: kind, Hall: o.Hall, Txid: txid, Beef: hex.EncodeToString(beef), Org: e.orgHex(), Group: group, Seq: t.Seq,
		NotBefore: e.Now().Add(e.Opts.Delay).Unix()}
	if err := e.queue(ctx, p); err != nil {
		return err
	}
	t.Published = true
	return e.St.Save()
}

// ConfirmGrants asks every host named whether it answers each grant now
// that the roll admits it, and offers the grant again to a host that lacks
// it (spec section 12: anyone who holds an object may submit it to a host
// that lacks it). A grant fewer hosts than the quorum answer is not
// published: it goes back to the outbox, and the next command publishes it.
func (e *Engine) ConfirmGrants(ctx context.Context, commitments []string) error {
	o := e.St.Org
	rd := e.Legs.Reader
	if rd == nil || o == nil {
		return nil
	}
	var first error
	for _, c := range commitments {
		g := o.GrantByCommitment(c)
		if g == nil {
			continue
		}
		q := reader.Query{"hall": o.Hall, "org": e.orgHex(), "member": g.Subject}
		held := 0
		var missing []string
		for _, host := range rd.Hosts {
			ok, err := rd.Held(ctx, host, q, g.Txid)
			if ok {
				held++
				continue
			}
			if err != nil {
				e.note("grant %s: host %s could not be asked: %v", Short(g.Txid), host, err)
				missing = append(missing, host)
				continue
			}
			beef, err := e.grantBEEF(g)
			if err != nil {
				e.note("grant %s: %v", Short(g.Txid), err)
				missing = append(missing, host)
				continue
			}
			set := &unicast.Set{Hosts: []string{host}, Need: 1, Retries: Retries, HTTP: rd.HTTP, Note: e.Note}
			confirm := func(ctx context.Context, h string) (bool, error) { return rd.Held(ctx, h, q, g.Txid) }
			if err := set.Send(ctx, "grant "+Short(g.Txid), borgrec.Topic(o.Hall), beef, 1, confirm).Err(); err != nil {
				e.note("grant %s: host %s lacks it and did not take it again: %v", Short(g.Txid), host, err)
				missing = append(missing, host)
				continue
			}
			e.note("grant %s: host %s lacked it; offered again and confirmed", Short(g.Txid), host)
			held++
		}
		if need := e.carrierNeed(); held < need {
			if !e.St.Queued(g.Txid) {
				e.St.Outbox = append(e.St.Outbox, state.Pending{Kind: state.KindGrant, Hall: o.Hall, Txid: g.Txid, Beef: g.Beef, Org: e.orgHex(), Member: g.Subject})
				if err := e.St.Save(); err != nil {
					return err
				}
			}
			err := fmt.Errorf("grant %s: %d of %d host(s) named answer it and %d must (%s lack it or could not be asked); it is persisted and the next command publishes it", Short(g.Txid), held, len(rd.Hosts), need, strings.Join(missing, ", "))
			if first == nil {
				first = err
			} else {
				e.note("%v", err)
			}
		}
	}
	return first
}

// grantBEEF is a grant's carrier as it was published.
func (e *Engine) grantBEEF(g *state.Grant) ([]byte, error) {
	return hex.DecodeString(g.Beef)
}

// Owed publishes what the home owes the hall: a token of its own chains
// mined and not yet published, and the wraps of each group's current epoch
// not yet made for some member.
func (e *Engine) Owed(ctx context.Context) error {
	o := e.St.Org
	if o == nil {
		return nil
	}
	if t := state.Latest(o.Tokens); t != nil && !t.Published && !e.St.Queued(t.Txid) {
		if err := e.publishToken(ctx, state.KindOrgToken, "", t.Txid); err != nil {
			return err
		}
	}
	for i := range o.Groups {
		g := &o.Groups[i]
		t := state.Latest(g.Tokens)
		if t == nil {
			continue
		}
		if !t.Published && !e.St.Queued(t.Txid) {
			if err := e.publishToken(ctx, state.KindGroupToken, g.ID, t.Txid); err != nil {
				return err
			}
		}
		if err := e.wrapOwed(ctx, g); err != nil {
			return err
		}
	}
	return nil
}

// wrapOwed wraps the current epoch's key to every member of the group's
// latest token a wrap was not yet made for (spec sections 6.3 and 7.1), in
// records of up to Opts.Entries, each on its own record-tree output, only
// to members holding a current grant. With more than one signer it waits
// one further block after the token (spec section 7.2).
func (e *Engine) wrapOwed(ctx context.Context, g *state.Group) error {
	o := e.St.Org
	last := state.Latest(g.Tokens)
	recs, err := GroupRecords(g)
	if err != nil {
		return err
	}
	cur := recs[len(recs)-1]
	ep := g.EpochOf(uint32(cur.Epoch)) //nolint:gosec // at most 2^32 - 1
	if ep == nil {
		// Another instance began the epoch and this home has not opened
		// its key (Sync): it wraps to nobody, and the instance that holds
		// the key does.
		e.note("group %s: this home holds no key for epoch %d, begun by another instance: nothing is wrapped from here until `borg org sync` opens it, or the group rotates (borg epoch rotate %s)", g.ID, cur.Epoch, g.ID)
		return nil
	}
	members, _, err := Members(g)
	if err != nil {
		return err
	}
	if st, at, err := Flagged(g); err != nil {
		return err
	} else if st != "" {
		e.note("group %s: seq %d is %s; the key of epoch %d is wrapped to nobody else until the group rotates (borg epoch rotate %s)", g.ID, at, st, cur.Epoch, g.ID)
		return nil
	}
	var owed [][]byte
	for _, m := range members {
		h := hex.EncodeToString(m)
		if slices.Contains(ep.Wrapped, h) {
			continue
		}
		if e.currentGrant(h) == nil {
			e.note("group %s: %s holds no current grant and is not wrapped epoch %d: rotation debt, cleared by the group's next transition", g.ID, h, cur.Epoch)
			continue
		}
		owed = append(owed, m)
	}
	if len(owed) == 0 {
		return nil
	}
	if e.Opts.Signers > 1 {
		tip, err := e.Legs.Headers.CurrentHeight(ctx)
		if err != nil || tip < last.Height+1 {
			e.note("group %s: the wraps of epoch %d wait for one block after the token (signers %d); the next command makes them", g.ID, cur.Epoch, e.Opts.Signers)
			return nil
		}
	}
	k := hashOfHex(ep.Key)
	id := hashOfHex(ep.EpochID)
	if borgrec.Commitment(k) != cur.Commitment {
		return fmt.Errorf("group %s: the key kept for epoch %d does not match the token's commitment", g.ID, cur.Epoch)
	}
	for start := 0; start < len(owed); start += e.Opts.Entries {
		chunk := owed[start:min(start+e.Opts.Entries, len(owed))]
		w := &borgrec.WrapRecord{Hall: o.Hall, Org: e.OrgKey(), Group: cur.Group, Epoch: cur.Epoch, EpochID: id}
		for _, m := range chunk {
			pub, err := ec.PublicKeyFromBytes(m)
			if err != nil {
				return err
			}
			entry, err := borgrec.Wrap(ctx, e.Signer, e.OrgKey(), pub, id, uint32(cur.Epoch), k) //nolint:gosec // at most 2^32 - 1
			if err != nil {
				return fmt.Errorf("wrap to %s: %w", hex.EncodeToString(m), err)
			}
			w.Entries = append(w.Entries, entry)
		}
		rec, err := w.Encode()
		if err != nil {
			return err
		}
		tree, vout, err := e.spend(ctx, state.TreeRecord)
		if err != nil {
			return err
		}
		a, beef, err := e.carrierFor(ctx, o.Hall, state.TreeRecord, rec, tree, vout)
		if err != nil {
			return err
		}
		txid := Txid(a.Txid)
		e.markSpent(state.TreeRecord, tree, vout)
		for _, m := range chunk {
			ep.Wrapped = append(ep.Wrapped, hex.EncodeToString(m))
		}
		p := state.Pending{Kind: state.KindWrap, Hall: o.Hall, Txid: txid, Beef: hex.EncodeToString(beef), Org: e.orgHex(), Group: g.ID,
			Epoch: ep.Epoch, Member: hex.EncodeToString(chunk[0])}
		if err := e.queue(ctx, p); err != nil {
			return err
		}
		e.note("group %s epoch %d: wrap %s to %d member(s)", g.ID, cur.Epoch, Short(txid), len(chunk))
	}
	return nil
}
